package server

// 资料库页「问一下 AI」：资料库 + 自定义话术 + 素材库一起查，AI 总结回答；
// 资料库没有时（或选择“总是联网”时）联网搜索补充。

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/git-ljm/kefu-ai/internal/llm"
	"github.com/git-ljm/kefu-ai/internal/rag"
	"github.com/git-ljm/kefu-ai/internal/websearch"
)

type AskResult struct {
	Answer       string             `json:"answer"`        // 给客服看的中文回答
	ReplyEN      string             `json:"reply_en"`      // 可以直接发给客户的英文
	Covered      bool               `json:"covered"`       // 资料库是否覆盖了这个问题
	WebQuery     string             `json:"web_query"`     // 联网时用的搜索词
	UsedWeb      bool               `json:"used_web"`      // 回答是否用到了网络结果
	KB           []rag.Hit          `json:"kb_hits"`       // 资料库命中
	Media        []*MediaItem       `json:"media"`         // 相关素材
	Web          []websearch.Result `json:"web_results"`   // 网络结果
	WebError     string             `json:"web_error"`     // 联网失败原因
	Error        string             `json:"error"`         // AI 回答失败原因（检索结果照样返回）
	EmbedWarning string             `json:"embed_warning"` // 向量模型不可用时只能关键词匹配
}

const askSystem = `你是跨境电商店铺的内部知识助手，回答客服同事的问题。
规则：
1. 优先依据【自定义话术】和【资料库】回答；只用其中明确写了的事实，不编造价格、库存、时效、承诺。
2. 资料不足以回答时，covered=false，并在 web_query 里给出适合上网搜索的关键词（只保留通用知识部分，去掉客户姓名、订单号、联系方式等个人信息；与本店价格/库存/订单相关的问题不要上网搜，web_query 留空）。
3. answer 用中文，简洁、分点说清楚，并标注依据编号（如 [2]、[W1]）。
4. reply_en 是可以直接发给海外客户的英文回复（1-3 句，自然口语），信息不足时写“需要客服确认”的礼貌说法。
5. 如果【相关素材】里有适合发给客户的图片/视频，在 answer 末尾提一句“可配合发送：素材名”。
只输出 JSON：{"answer":"","reply_en":"","covered":true,"web_query":""}`

func (a *App) Ask(ctx context.Context, q, webMode string) *AskResult {
	res := &AskResult{KB: []rag.Hit{}, Media: []*MediaItem{}, Web: []websearch.Result{}} // 空列表而不是 null，前端好处理
	var qv []float32
	if vs, err := a.llm.Embed(ctx, []string{q}); err == nil {
		qv = vs[0]
	} else {
		res.EmbedWarning = "向量模型不可用，目前只能按关键词匹配（意思相近但用词不同的内容查不到）。请在「设置」检查向量模型，装好后点「重建向量」"
	}
	res.KB = append(res.KB, a.index.Search(q, qv, map[string]bool{"doc": true, "history": true, "example": true, "snippet": true}, 8)...)
	for _, h := range a.index.Search(q, qv, map[string]bool{"media": true}, 6) {
		if it, ok := a.mediaItem(h.DocID, nil); ok {
			res.Media = append(res.Media, it)
		}
	}
	settings := a.store.Settings()
	webOn := websearch.Enabled(settings)
	modelSearch := settings["web_provider"] == websearch.ProviderModel // 让大模型自己联网
	online := false                                                    // 当前这次回答是否让模型联网

	build := func(web []websearch.Result) string {
		var u strings.Builder
		u.WriteString("【资料库】（含自定义话术）\n")
		if len(res.KB) == 0 {
			u.WriteString("（没有找到相关资料）\n")
		}
		for i, h := range res.KB {
			fmt.Fprintf(&u, "[%d] %s\n", i+1, h.Content)
		}
		if len(res.Media) > 0 {
			u.WriteString("\n【相关素材】\n")
			for _, m := range res.Media {
				fmt.Fprintf(&u, "- %s：%s\n", m.Name, websearch.Trunc(m.Description, 120))
			}
		}
		if len(web) > 0 {
			u.WriteString("\n【网络搜索结果】\n" + websearch.Context(web) + "\n" + websearch.Rules + "\n")
		}
		if online {
			u.WriteString("\n【联网】资料库不足以回答，请联网搜索“" + res.WebQuery + "”相关的通用知识后回答，把网上查到的内容当作【网络搜索结果】对待，并在回答里注明来源网站。\n" + websearch.Rules + "\n")
		}
		u.WriteString("\n【问题】\n" + q + "\n请输出 JSON。")
		return u.String()
	}
	ask := func(web []websearch.Result) error {
		out, err := a.llm.Chat(ctx, llm.ChatReq{System: askSystem, User: build(web), JSON: true, WebSearch: online})
		if err != nil {
			return err
		}
		var v struct {
			Answer   string `json:"answer"`
			ReplyEN  string `json:"reply_en"`
			Covered  *bool  `json:"covered"`
			WebQuery string `json:"web_query"`
		}
		if err := llm.ExtractJSON(out, &v); err != nil {
			v.Answer = out
		}
		res.Answer, res.ReplyEN = v.Answer, v.ReplyEN
		if len(web) == 0 && !online { // 已经联网搜过时，保留实际用的搜索词
			res.WebQuery = strings.TrimSpace(v.WebQuery)
		}
		res.Covered = v.Covered == nil || *v.Covered
		return nil
	}
	// doWeb 准备联网：搜索服务模式下先拿到结果列表；大模型自带联网模式下只是打开开关，由模型自己搜
	doWeb := func(query string) bool {
		if query == "" {
			query = q
		}
		res.WebQuery = query
		if modelSearch {
			online = true
			return true
		}
		web, err := a.webSearch(ctx, query, 6)
		if err != nil {
			res.WebError = err.Error()
			return false
		}
		res.Web = web
		return len(web) > 0
	}

	switch {
	case webMode == "always" && webOn:
		// 先搜网，再一次性回答
		doWeb(q)
		if err := ask(res.Web); err != nil {
			res.Error = "AI 回答失败：" + err.Error()
		}
		res.UsedWeb = len(res.Web) > 0 || (online && res.Error == "")
	default:
		if err := ask(nil); err != nil {
			res.Error = "AI 回答失败：" + err.Error()
			// AI 不可用时：资料库一条都没查到，且开了联网搜索服务，就直接给网络结果
			if webMode == "auto" && webOn && !modelSearch && len(res.KB) == 0 {
				doWeb(q)
			}
			break
		}
		if webMode == "auto" && !res.Covered {
			if !webOn {
				res.WebError = "资料库里没有，可以在「设置 → 联网搜索」配置后自动上网查"
				break
			}
			first := *res
			if doWeb(res.WebQuery) {
				if err := ask(res.Web); err != nil {
					// 第二次失败就保留第一次的回答
					res.Answer, res.ReplyEN = first.Answer, first.ReplyEN
					res.Error = "结合网络结果回答失败：" + err.Error()
				} else {
					res.UsedWeb = true
				}
			}
			res.Covered = false
		}
	}
	return res
}

func (a *App) askHandler(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Q   string `json:"q"`
		Web string `json:"web"` // off / auto / always
	}
	if err := readBody(r, &in); err != nil || strings.TrimSpace(in.Q) == "" {
		writeErr(w, 400, "请输入问题")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 4*time.Minute)
	defer cancel()
	writeJSON(w, a.Ask(ctx, strings.TrimSpace(in.Q), in.Web))
}

// webTest 设置页“测试联网搜索”
func (a *App) webTest(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Q string `json:"q"`
	}
	_ = readBody(r, &in)
	if strings.TrimSpace(in.Q) == "" {
		in.Q = "FedEx international shipping time to USA"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if a.store.Settings()["web_provider"] == websearch.ProviderModel {
		out, err := a.llm.Chat(ctx, llm.ChatReq{
			System:    "你是联网搜索助手。",
			User:      "请联网搜索并用一句话回答：" + in.Q + "。最后注明信息来源网站。",
			WebSearch: true,
		})
		if err != nil {
			writeErr(w, 502, "大模型联网失败："+err.Error())
			return
		}
		writeJSON(w, []websearch.Result{{Title: "大模型联网回答", Site: "模型自带搜索", Snippet: websearch.Trunc(out, 300)}})
		return
	}
	rs, err := a.webSearch(ctx, in.Q, 5)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	writeJSON(w, rs)
}
