// Package server 是业务核心与 HTTP 接口：资料导入、AI 分析与回复建议、客户画像、素材推荐、
// 自定义话术、问答与联网搜索、备份等。
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/git-ljm/kefu-ai/internal/etl"
	"github.com/git-ljm/kefu-ai/internal/llm"
	"github.com/git-ljm/kefu-ai/internal/rag"
	"github.com/git-ljm/kefu-ai/internal/store"
	"github.com/git-ljm/kefu-ai/internal/websearch"
)

type App struct {
	Version      string // 程序版本（页面和 /api/ping 显示）
	OtherDataDir string // 另一个位置也有数据（程序被移动/复制过），页面上提醒

	store   *store.Store
	llm     *llm.LLM
	index   *rag.Index
	dataDir string
	jobs    chan int64

	exampleMu    sync.Mutex
	exampleDocID int64

	// 文件打开密码：只在内存里保留到导入完成，不写入数据库或磁盘
	passwords sync.Map // docID -> string

	reMu    sync.Mutex
	reState ReembedState // 后台重建向量的进度
}

// NewApp 加载资料索引到内存。之后需调用 StartWorkers 启动后台导入队列
func NewApp(s *store.Store, dataDir, version string) *App {
	a := &App{Version: version, store: s, llm: llm.New(s), index: &rag.Index{}, dataDir: dataDir, jobs: make(chan int64, 100000)}
	cs, err := s.AllChunks()
	if err != nil {
		log.Printf("加载资料索引失败: %v", err)
	}
	a.index.Load(cs)
	log.Printf("资料库已加载 %d 个片段", len(cs))
	return a
}

// describeImage 资料库导入图片时：用看图模型把图片转成可检索的文字
func (a *App) describeImage(ctx context.Context, img []byte) (string, error) {
	return a.llm.Chat(ctx, llm.ChatReq{
		System: "你是资料整理助手，负责把图片转成可检索的文字资料。",
		User: "请完整提取这张图片中的所有文字（保持原文语言，表格按行输出），" +
			"然后用一两句话说明图片内容（例如：产品图、价格表、尺码表、聊天截图、物流单）。只输出结果，不要解释。",
		Images: [][]byte{img},
	})
}

// webSearch 按当前设置联网搜索
func (a *App) webSearch(ctx context.Context, query string, n int) ([]websearch.Result, error) {
	return websearch.Search(ctx, a.store.Settings(), query, n)
}

// ================= 资料导入 =================

func (a *App) StartWorkers() {
	go func() {
		for id := range a.jobs {
			a.ingest(id)
		}
	}()
	// 上次没处理完的重新排队
	for _, media := range []bool{false, true} {
		docs, _ := a.store.ListDocuments(media)
		for i := len(docs) - 1; i >= 0; i-- {
			if docs[i].Status == "processing" {
				a.jobs <- docs[i].ID
			}
		}
	}
}

func (a *App) ingest(id int64) {
	doc, err := a.store.GetDocument(id)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if doc.Kind == "media" {
		a.ingestMedia(ctx, doc)
		return
	}
	password := ""
	if v, ok := a.passwords.LoadAndDelete(id); ok {
		password = v.(string)
	}
	pieces, err := etl.Extract(ctx, a.describeImage, doc.Path, doc.Name, doc.Kind, password)
	if err != nil {
		a.store.UpdateDocStatus(id, "error", err.Error(), 0)
		log.Printf("导入失败 %s: %v", doc.Name, err)
		return
	}
	if len(pieces) == 0 {
		a.store.UpdateDocStatus(id, "error", "没有提取到内容", 0)
		return
	}
	chunks := make([]store.Chunk, len(pieces))
	for i, p := range pieces {
		chunks[i] = store.Chunk{DocID: id, Kind: doc.Kind, Content: p}
	}
	warn := a.embedChunks(ctx, chunks)
	ids, err := a.store.ReplaceChunks(id, chunks) // 重新导入时清掉旧片段
	if err != nil {
		a.store.UpdateDocStatus(id, "error", err.Error(), 0)
		return
	}
	for i := range chunks {
		chunks[i].ID = ids[i]
	}
	a.index.RemoveDoc(id)
	a.index.Add(chunks)
	a.store.UpdateDocStatus(id, "done", warn, len(chunks))
	log.Printf("导入完成 %s: %d 个片段", doc.Name, len(chunks))
}

// embedChunks 批量生成向量；向量模型不可用时返回提示，仍可用关键词检索
func (a *App) embedChunks(ctx context.Context, chunks []store.Chunk) string {
	const batch = 16
	for i := 0; i < len(chunks); i += batch {
		end := min(i+batch, len(chunks))
		texts := make([]string, 0, end-i)
		for _, c := range chunks[i:end] {
			texts = append(texts, c.Content)
		}
		vecs, err := a.llm.Embed(ctx, texts)
		if err != nil {
			return "向量模型不可用，暂时只能关键词检索（装好后点“重建向量”）：" + err.Error()
		}
		for j := range vecs {
			chunks[i+j].Embedding = vecs[j]
		}
	}
	return ""
}

// Reembed 为没有向量的片段补生成向量（比如后来才装好向量模型）
// Reembed 为没有向量的片段生成向量（后来才装好向量模型、或换了向量模型后全部重建）。
// 每 100 条写一次库，中途失败或关闭程序，已完成的部分不会丢，下次接着补。
func (a *App) Reembed(ctx context.Context, progress func(done, total int)) (int, error) {
	all, err := a.store.AllChunks()
	if err != nil {
		return 0, err
	}
	var todo []store.Chunk
	for _, c := range all {
		if len(c.Embedding) == 0 {
			todo = append(todo, c)
		}
	}
	sig := llm.LoadConfig(a.store).EmbedSignature()
	if len(todo) == 0 {
		_ = a.store.SaveSettings(map[string]string{"embed_signature": sig})
		return 0, nil
	}
	const step = 100
	done := 0
	for i := 0; i < len(todo); i += step {
		part := todo[i:min(i+step, len(todo))]
		if warn := a.embedChunks(ctx, part); warn != "" {
			a.index.Load(mustChunks(a.store))
			return done, fmt.Errorf("%s", warn)
		}
		if err := a.store.UpdateEmbeddings(part); err != nil {
			return done, err
		}
		done += len(part)
		if progress != nil {
			progress(done, len(todo))
		}
	}
	_ = a.store.SaveSettings(map[string]string{"embed_signature": sig})
	a.store.ClearVectorWarnings()
	a.index.Load(mustChunks(a.store))
	return done, nil
}

func mustChunks(s *store.Store) []store.Chunk {
	cs, _ := s.AllChunks()
	return cs
}

// ---------- 后台重建向量（带进度） ----------

type ReembedState struct {
	Running bool   `json:"running"`
	Done    int    `json:"done"`
	Total   int    `json:"total"`
	Error   string `json:"error"`
	Model   string `json:"model"`
}

func (a *App) reembedStatus() ReembedState {
	a.reMu.Lock()
	defer a.reMu.Unlock()
	return a.reState
}

// StartReembed 在后台开始重建；已经在跑时直接返回
func (a *App) StartReembed() bool {
	a.reMu.Lock()
	if a.reState.Running {
		a.reMu.Unlock()
		return false
	}
	a.reState = ReembedState{Running: true, Model: llm.LoadConfig(a.store).EmbedName()}
	a.reMu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
		defer cancel()
		n, err := a.Reembed(ctx, func(done, total int) {
			a.reMu.Lock()
			a.reState.Done, a.reState.Total = done, total
			a.reMu.Unlock()
		})
		a.reMu.Lock()
		a.reState.Running = false
		a.reState.Done = max(a.reState.Done, n)
		if err != nil {
			a.reState.Error = err.Error()
			log.Printf("重建向量失败: %v", err)
		} else {
			log.Printf("重建向量完成: %d 个片段", n)
		}
		a.reMu.Unlock()
	}()
	return true
}

// CheckEmbedModel 向量模型换了（本地↔云端、换模型、换维度）：清空旧向量并在后台全部重建。
// 不同模型的向量不能混用，维度碰巧相同时混用会让检索结果完全错乱。
func (a *App) CheckEmbedModel() bool {
	sig := llm.LoadConfig(a.store).EmbedSignature()
	old := a.store.Settings()["embed_signature"]
	if old == sig {
		return false
	}
	if old == "" { // 新库，或老版本没有记录：认为现有向量就是当前设置生成的
		_ = a.store.SaveSettings(map[string]string{"embed_signature": sig})
		return false
	}
	log.Printf("向量模型已更换（%s → %s），清空旧向量并后台重建", old, sig)
	_ = a.store.ClearEmbeddings()
	// 旧向量已清空，之后生成的都是新模型的向量：立刻记下新模型。
	// 这样即使这次重建失败（比如 Key 填错），再切回原来的模型也会正确触发重建
	_ = a.store.SaveSettings(map[string]string{"embed_signature": sig})
	a.index.Load(mustChunks(a.store))
	a.StartReembed()
	return true
}

// ================= AI 分析与回复建议 =================

type Suggestion struct {
	Intent       string    `json:"intent"`
	Language     string    `json:"customer_language"`
	CustomerZh   string    `json:"customer_message_zh"`
	Sentiment    string    `json:"sentiment"`
	Risk         string    `json:"risk"`
	Reply        string    `json:"reply"`
	ReplyZh      string    `json:"reply_zh"`
	Confidence   int       `json:"confidence"`
	MissingInfo  string    `json:"missing_info"`
	SourceIDs    []int     `json:"sources"`
	Sources      []rag.Hit `json:"source_hits"`
	EmbedWarning string    `json:"embed_warning,omitempty"`
	// 素材推荐
	MediaIDs    []int        `json:"media"`        // AI 选中的候选编号
	MediaReason string       `json:"media_reason"` // 为什么建议发
	Media       []*MediaItem `json:"media_items"`  // AI 推荐发送的素材
	MediaMore   []*MediaItem `json:"media_more"`   // 其他相关素材（AI 没选，供客服参考）
	// 联网搜索（设置里开启“资料库没有时自动联网”才会用）
	WebQuery   string             `json:"web_query"`
	WebResults []websearch.Result `json:"web_results,omitempty"`
	WebUsed    bool               `json:"web_used,omitempty"` // 用了大模型自带联网（没有结果列表）
	WebError   string             `json:"web_error,omitempty"`
}

// ExtractScreenshot 识别客户发来的截图
func (a *App) ExtractScreenshot(ctx context.Context, img []byte) (string, error) {
	return a.llm.Chat(ctx, llm.ChatReq{
		System: "你是客服助手，负责识别客户发来的图片。",
		User: "判断这张图片的类型并输出：\n" +
			"1. 如果是聊天截图：按顺序提取【客户】发的消息原文（通常在左侧的气泡；右侧通常是我方客服），每条一行；如能看到我方消息，用“[我方] ”前缀标出。\n" +
			"2. 如果是商品/包裹/订单/物流/付款等图片：先提取图中所有文字（订单号、金额、型号等保持原样），再用一句话描述图片内容，以“[图片内容] ”开头。\n" +
			"只输出结果，不要解释。",
		Images: [][]byte{img},
	})
}

const suggestSystem = `你是一名资深跨境电商客服，帮助客服同事给客户写回复。规则：
1. 只依据【自定义话术】【资料库】【历史成交聊天】【对话记录】中的事实回答。资料里没有的信息（价格、库存、发货时间、订单状态、优惠等）绝不编造；需要时礼貌告诉客户会马上核实，并在 missing_info 里用中文写明客服需要确认什么。
2. 客户的问题与【自定义话术】场景相符时，以它的推荐回复为准（意思不变，可根据上下文微调措辞、补上称呼）；其次模仿【客服示范回复】和【历史成交聊天】的语气与话术。
3. 回复要像真人：自然、友好、简洁，一般 1-4 句，不用 markdown、不用列表符号，不要每次都道歉或过度客套。
4. 闲聊、寒暄时友好回应，可适当自然引导回产品或订单，但不要生硬推销。
5. 客户情绪负面，或提到退款、投诉、差评、拒付(chargeback)、律师、平台申诉等，在 risk 中用中文说明风险和处理建议；否则 risk 为空字符串。
6. confidence 为 0-100：资料完全覆盖且明确时高，靠推测时低。
7. 【可发送素材】是店里现成的真实图片/视频。当发图/视频能明显帮到客户时才选用，典型场景：客户想看实物/细节/颜色/尺寸对比/使用效果、怀疑质量或真假、问包装、催发货需要发货凭证或物流单、犹豫不决需要好评截图或实拍视频增加信任。
   选中时把编号填进 media（最多 3 个，只能选与客户问题直接相关的），在 media_reason 用中文简述理由，并在 reply 里自然地提一句（如 "I've attached a real photo of ..." / "Here is a short video showing ..."）。
   标注“已发过”的素材不要重复推荐，除非客户再次要求。不需要发就返回空数组，reply 里也不要提附件。
只输出一个 JSON 对象，字段如下：
{"intent":"订单查询|产品咨询|价格议价|物流配送|售后退换|投诉|付款问题|闲聊|其他",
 "customer_language":"客户使用的语言，如 English",
 "customer_message_zh":"客户最新消息的中文翻译",
 "sentiment":"积极|中性|消极|愤怒",
 "risk":"",
 "reply":"给客户的回复",
 "reply_zh":"回复的中文翻译",
 "confidence":80,
 "missing_info":"",
 "sources":[用到的资料编号，如 1,3],
 "media":[要附带发送的素材编号，如 1；不需要则为空数组],
 "media_reason":"",
 "web_query":"只有当客户问的是通用知识（不是本店价格/库存/订单/发货）且资料完全没有覆盖时，才填写适合上网搜索的关键词（去掉客户姓名、订单号、联系方式）；否则留空"}`

func (a *App) Suggest(ctx context.Context, c *store.Customer, msg *store.Message, hint string) (*Suggestion, error) {
	cfg := llm.LoadConfig(a.store)

	history, _ := a.store.Messages(c.ID, 16)
	var convo strings.Builder
	for _, m := range history {
		if m.ID >= msg.ID {
			continue
		}
		who := "客户"
		if m.Role == "agent" {
			who = "我方"
		}
		convo.WriteString(who + ": " + m.Content)
		for _, id := range store.ParseIDs(m.MediaIDs) {
			if d, err := a.store.GetDocument(id); err == nil {
				convo.WriteString(" [附带发送了素材：" + d.Name + "]")
			}
		}
		convo.WriteString("\n")
	}

	// 检索：用最新消息 + 上一条客户消息，避免“那这个呢？”这类追问丢失上下文
	query := msg.Content
	for i := len(history) - 1; i >= 0; i-- {
		if history[i].ID < msg.ID && history[i].Role == "customer" {
			query = history[i].Content + "\n" + query
			break
		}
	}
	sug := &Suggestion{}
	var qv []float32
	if vs, err := a.llm.Embed(ctx, []string{query}); err == nil {
		qv = vs[0]
	} else {
		sug.EmbedWarning = "向量模型不可用，仅使用关键词检索"
	}
	docs := a.index.Search(query, qv, map[string]bool{"doc": true}, 6)
	hist := a.index.Search(query, qv, map[string]bool{"history": true}, 3)
	exs := a.index.Search(query, qv, map[string]bool{"example": true}, 3)
	snips := a.index.Search(query, qv, map[string]bool{"snippet": true}, 3)
	mediaHits := a.index.Search(query, qv, map[string]bool{"media": true}, 5)
	sent := a.store.SentMedia(c.ID)

	var u strings.Builder
	u.WriteString("【店铺说明与语气要求】\n" + cfg.Persona + "\n\n")
	if c.Notes != "" || c.Profile != "" {
		u.WriteString("【客户信息】\n名称: " + c.Name + "\n")
		if c.Notes != "" {
			u.WriteString("客服备注: " + c.Notes + "\n")
		}
		if c.Profile != "" {
			u.WriteString("客户画像: " + c.Profile + "\n")
		}
		u.WriteString("\n")
	}
	if len(snips) > 0 {
		u.WriteString("【自定义话术（店铺标准答法，场景匹配时优先使用，可按对话自然调整措辞）】\n")
		for _, h := range snips {
			u.WriteString(h.Content + "\n---\n")
		}
		u.WriteString("\n")
	}
	u.WriteString("【资料库】\n")
	if len(docs) == 0 {
		u.WriteString("（没有找到相关资料）\n")
	}
	for i, h := range docs {
		fmt.Fprintf(&u, "[%d] %s\n", i+1, h.Content)
	}
	if len(mediaHits) > 0 {
		u.WriteString("\n【可发送素材】\n")
		for i, h := range mediaHits {
			flag := ""
			if sent[h.DocID] {
				flag = "（已发过给该客户）"
			}
			fmt.Fprintf(&u, "[%d]%s %s\n", i+1, flag, h.Content)
		}
	}
	if len(hist) > 0 {
		u.WriteString("\n【历史成交聊天（参考话术）】\n")
		for _, h := range hist {
			u.WriteString(h.Content + "\n---\n")
		}
	}
	if len(exs) > 0 {
		u.WriteString("\n【客服示范回复（优先参考）】\n")
		for _, h := range exs {
			u.WriteString(h.Content + "\n---\n")
		}
	}
	if convo.Len() > 0 {
		u.WriteString("\n【对话记录】\n" + convo.String())
	}
	u.WriteString("\n【客户最新消息】\n" + msg.Content + "\n\n")
	if cfg.ReplyLanguage == "auto" {
		u.WriteString("回复语言：与客户最新消息使用相同的语言。\n")
	} else {
		u.WriteString("回复语言：English。\n")
	}
	if hint != "" {
		u.WriteString("客服的额外要求：" + hint + "\n")
	}
	u.WriteString("请输出 JSON。")

	req := llm.ChatReq{System: suggestSystem, User: u.String(), JSON: true}
	if hint != "" {
		req.Temperature = 0.75 // “换一个”时需要多样性
	}
	out, err := a.llm.Chat(ctx, req)
	if err != nil {
		return nil, err
	}
	if err := llm.ExtractJSON(out, sug); err != nil {
		// 模型没按格式输出时，把原文当回复，避免完全失败
		sug.Reply, sug.Intent, sug.Confidence = out, "其他", 30
	}

	// 资料库没有覆盖的通用知识：按设置自动联网，再写一次
	if m := a.store.Settings(); m["web_reply"] == "auto" && m["web_provider"] == websearch.ProviderModel && strings.TrimSpace(sug.WebQuery) != "" {
		// 大模型自带联网：同一个提示词，打开联网开关再写一次
		req.WebSearch = true
		req.User = u.String() + "\n\n【联网】资料库没有覆盖这个问题，请联网搜索“" + sug.WebQuery + "”相关的通用知识，把查到的内容当作【网络搜索结果】对待。\n" +
			websearch.Rules + "\n请输出 JSON。"
		if out2, err := a.llm.Chat(ctx, req); err == nil {
			sug2 := &Suggestion{EmbedWarning: sug.EmbedWarning}
			if llm.ExtractJSON(out2, sug2) == nil && sug2.Reply != "" {
				sug2.WebQuery, sug2.WebUsed = sug.WebQuery, true
				sug = sug2
			}
		} else {
			sug.WebError = "大模型联网失败：" + err.Error()
		}
	} else if m["web_reply"] == "auto" && websearch.Enabled(m) && strings.TrimSpace(sug.WebQuery) != "" {
		web, err := a.webSearch(ctx, sug.WebQuery, 5)
		if err != nil {
			sug.WebError = err.Error()
		} else if len(web) > 0 {
			req.User = u.String() + "\n\n【网络搜索结果】\n" + websearch.Context(web) + "\n" + websearch.Rules + "\n请结合以上内容重新输出 JSON。"
			if out2, err := a.llm.Chat(ctx, req); err == nil {
				sug2 := &Suggestion{EmbedWarning: sug.EmbedWarning}
				if llm.ExtractJSON(out2, sug2) == nil && sug2.Reply != "" {
					sug2.WebQuery = sug.WebQuery
					sug = sug2
				}
			} else {
				sug.WebError = "结合网络结果重写失败：" + err.Error()
			}
			sug.WebResults, sug.WebUsed = web, true
		}
	}
	for _, id := range sug.SourceIDs {
		if id >= 1 && id <= len(docs) {
			sug.Sources = append(sug.Sources, docs[id-1])
		}
	}
	if len(sug.Sources) == 0 && len(docs) > 0 {
		sug.Sources = docs[:min(2, len(docs))]
	}
	sug.Sources = append(append([]rag.Hit{}, snips...), sug.Sources...) // 参考来源里也列出匹配到的自定义话术
	picked := map[int64]bool{}
	for _, id := range sug.MediaIDs {
		if id >= 1 && id <= len(mediaHits) && !picked[mediaHits[id-1].DocID] {
			if it, ok := a.mediaItem(mediaHits[id-1].DocID, sent); ok {
				picked[it.ID] = true
				sug.Media = append(sug.Media, it)
			}
		}
	}
	for _, h := range mediaHits { // 其余相关素材也列出来，客服可以自己挑
		if !picked[h.DocID] {
			if it, ok := a.mediaItem(h.DocID, sent); ok {
				sug.MediaMore = append(sug.MediaMore, it)
			}
		}
	}
	if b, err := json.Marshal(sug); err == nil {
		a.store.UpdateMessageAnalysis(msg.ID, sug.CustomerZh, string(b))
	}
	return sug, nil
}

// SaveReply 记录客服最终发送的回复；修改过的回复沉淀进“示范回复库”
func (a *App) SaveReply(ctx context.Context, customerID, replyTo int64, content, aiReply string, mediaIDs []int64) (*store.Message, error) {
	action := "manual"
	if aiReply != "" {
		if strings.TrimSpace(aiReply) == strings.TrimSpace(content) {
			action = "accepted"
		} else {
			action = "edited"
		}
	}
	m := &store.Message{CustomerID: customerID, Role: "agent", Content: content, AIReply: aiReply, Action: action, ReplyTo: replyTo, MediaIDs: store.JoinIDs(mediaIDs)}
	if err := a.store.AddMessage(m); err != nil {
		return nil, err
	}
	a.store.IncSendCount(mediaIDs)
	if replyTo > 0 && action != "accepted" {
		if q, err := a.store.GetMessage(replyTo); err == nil {
			go a.addExample(q.Content, content)
		}
	}
	// 每 3 条客户消息自动更新一次画像
	if n := a.store.CountCustomerMessages(customerID); n > 0 && n%3 == 0 {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			defer cancel()
			if _, err := a.UpdateProfile(ctx, customerID); err != nil {
				log.Printf("自动更新画像失败: %v", err)
			}
		}()
	}
	return m, nil
}

func (a *App) addExample(question, answer string) {
	a.exampleMu.Lock()
	defer a.exampleMu.Unlock()
	if a.exampleDocID == 0 {
		id, ok := a.store.FirstDocOfKind("example")
		if !ok {
			d := &store.Document{Name: "客服示范回复（自动积累）", Kind: "example", Status: "done"}
			if err := a.store.AddDocument(d); err != nil {
				return
			}
			id = d.ID
		}
		a.exampleDocID = id
	}
	c := []store.Chunk{{DocID: a.exampleDocID, Kind: "example", Content: "客户: " + question + "\n优秀回复: " + answer}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	a.embedChunks(ctx, c)
	ids, err := a.store.AddChunks(c)
	if err != nil {
		return
	}
	c[0].ID = ids[0]
	a.index.Add(c)
	a.store.IncDocChunks(a.exampleDocID)
}

// ================= 客户画像 =================

const profileSystem = `你是客户分析师。根据客服备注、旧画像和聊天记录，输出最新的客户画像 JSON（全部用中文）：
{"summary":"一句话概括这个客户",
 "language":"客户语言",
 "region":"可能的国家/地区，不确定写未知",
 "stage":"新询盘|比价中|已下单|待收货|售后中|老客户|闲聊",
 "purchase_intent":"高|中|低",
 "interests":["感兴趣的产品或话题"],
 "concerns":["顾虑，如价格、物流时间、质量"],
 "personality":"沟通风格，如直接、谨慎、爱砍价、友好",
 "preferences":"偏好，如喜欢简短回复、在意折扣",
 "next_action":"给客服的下一步建议"}
只输出 JSON。`

func (a *App) UpdateProfile(ctx context.Context, customerID int64) (string, error) {
	c, err := a.store.GetCustomer(customerID)
	if err != nil {
		return "", err
	}
	msgs, _ := a.store.Messages(customerID, 60)
	if len(msgs) == 0 {
		return "", fmt.Errorf("还没有聊天记录")
	}
	var u strings.Builder
	u.WriteString("客户名称: " + c.Name + "\n平台: " + c.Platform + "\n客服备注: " + c.Notes + "\n旧画像: " + c.Profile + "\n\n聊天记录:\n")
	for _, m := range msgs {
		who := "客户"
		if m.Role == "agent" {
			who = "我方"
		}
		u.WriteString(who + ": " + m.Content + "\n")
	}
	out, err := a.llm.Chat(ctx, llm.ChatReq{System: profileSystem, User: u.String(), JSON: true})
	if err != nil {
		return "", err
	}
	var v map[string]any
	if err := llm.ExtractJSON(out, &v); err != nil {
		return "", err
	}
	b, _ := json.Marshal(v)
	if err := a.store.SetProfile(customerID, string(b)); err != nil {
		return "", err
	}
	return string(b), nil
}

// ================= 翻译 / 改写 =================

func (a *App) Transform(ctx context.Context, text, mode, lang string) (map[string]string, error) {
	if lang == "" {
		lang = "English"
	}
	var instr string
	switch mode {
	case "to_customer":
		instr = "把下面客服写的内容翻译成地道、礼貌、自然的 " + lang + " 客服回复（意思不变，可以适当润色）。"
	case "to_zh":
		instr = "把下面的内容翻译成中文。text 字段放中文译文，zh 字段也放中文译文。"
	case "formal":
		instr = "把下面的客服回复改写得更正式、专业，保持原语言。"
	case "friendly":
		instr = "把下面的客服回复改写得更亲切、热情，保持原语言。"
	case "short":
		instr = "把下面的客服回复精简到一两句话，保持原语言和关键信息。"
	default:
		return nil, fmt.Errorf("未知操作")
	}
	out, err := a.llm.Chat(ctx, llm.ChatReq{
		System: "你是专业的跨境电商客服文案助手。只输出 JSON：{\"text\":\"结果\",\"zh\":\"结果的中文翻译\"}",
		User:   instr + "\n\n内容：\n" + text,
		JSON:   true,
	})
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := llm.ExtractJSON(out, &raw); err != nil {
		return map[string]string{"text": out}, nil
	}
	res := map[string]string{}
	for k, v := range raw {
		res[k] = fmt.Sprint(v)
	}
	return res, nil
}

func (a *App) saveFile(sub, name string, data []byte) (string, error) {
	dir := filepath.Join(a.dataDir, "files", sub)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	p := filepath.Join(dir, fmt.Sprintf("%d_%s", time.Now().UnixNano(), safeName(name)))
	return p, os.WriteFile(p, data, 0o644)
}

// safeName 去掉文件名中各系统不允许的字符
func safeName(name string) string {
	return strings.Map(func(r rune) rune {
		if strings.ContainsRune(`/\:*?"<>|`, r) || r < 32 {
			return '_'
		}
		return r
	}, filepath.Base(name))
}
