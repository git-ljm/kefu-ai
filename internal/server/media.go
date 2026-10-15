package server

// 素材库：可以发给客户的图片/视频（实拍图、细节图、视频、发货凭证、好评截图…）
//
// 扫描方式：
//   图片 —— 交给看图模型，输出“画面描述 / 图中文字 / 标签 / 适合发送的场景”
//   视频 —— 浏览器在上传前截取 4 个关键帧拼成一张图（无需安装任何软件）；
//          浏览器解不了的格式，如果电脑装了 ffmpeg 就用 ffmpeg 截帧；都不行就只按文件名和人工描述检索
// 推荐方式：每次生成回复时，用客户消息检索素材库，把候选素材交给 AI，由 AI 判断要不要附带、附哪个。

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/git-ljm/kefu-ai/internal/etl"
	"github.com/git-ljm/kefu-ai/internal/llm"
	"github.com/git-ljm/kefu-ai/internal/store"
)

type mediaScan struct {
	Description string   `json:"description"`
	Text        string   `json:"text_in_image"`
	Tags        []string `json:"tags"`
	Usage       string   `json:"usage"`
}

const mediaScanPrompt = `这是我们店铺准备发给客户看的%s。请仔细观察并输出 JSON（全部用中文，图中原文保持原样）：
{"description":"客观描述画面：是什么产品/物品、型号、颜色、角度、细节、场景、是否为实拍、是否是包装/物流单/付款记录/聊天好评截图等",
 "text_in_image":"画面中能看到的所有文字（订单号、型号、尺寸、价格等），没有就留空",
 "tags":["3-8 个简短标签，如 产品名、颜色、实拍、细节、包装、尺寸、物流凭证、好评"],
 "usage":"客服在什么情况下适合把它发给客户，如：客户想看实物/怀疑质量/问尺寸/问包装/催发货要凭证/犹豫要不要买时"}
只输出 JSON。`

// ingestMedia 扫描一个素材并建立索引
func (a *App) ingestMedia(ctx context.Context, d *store.Document) {
	var img []byte
	var warn string
	what := "图片"
	switch d.MediaType {
	case "image":
		b, err := os.ReadFile(d.Path)
		if err != nil {
			a.store.UpdateDocStatus(d.ID, "error", err.Error(), 0)
			return
		}
		img = b
	case "video":
		what = "视频（下图是从视频中按时间顺序截取的 4 个画面拼图）"
		if d.Thumb == "" {
			if p, err := etl.VideoPoster(ctx, d.Path); err == nil {
				d.Thumb = p
			}
		}
		if d.Thumb != "" {
			img, _ = os.ReadFile(d.Thumb)
		}
		if len(img) == 0 {
			warn = "无法截取视频画面（可换 Chrome/Edge 浏览器上传，或安装 ffmpeg），目前只能按文件名和你填写的描述匹配，建议手动补充描述"
		}
	}

	if len(img) > 0 {
		out, err := a.llm.Chat(ctx, llm.ChatReq{
			System: "你是电商素材整理助手。",
			User:   fmt.Sprintf(mediaScanPrompt, what),
			Images: [][]byte{img},
			JSON:   true,
		})
		var sc mediaScan
		if err == nil {
			err = llm.ExtractJSON(out, &sc)
		}
		if err != nil {
			warn = "AI 识别失败，可手动填写描述后保存：" + err.Error()
		} else {
			desc := strings.TrimSpace(sc.Description)
			if t := strings.TrimSpace(sc.Text); t != "" {
				desc += "\n图中文字：" + t
			}
			if strings.TrimSpace(d.Description) == "" { // 不覆盖人工填写的内容
				d.Description = desc
			}
			d.Tags = mergeTags(d.Tags, strings.Join(sc.Tags, ","))
			if strings.TrimSpace(d.Usage) == "" {
				d.Usage = sc.Usage
			}
		}
	}
	if err := a.store.UpdateMediaInfo(d); err != nil {
		a.store.UpdateDocStatus(d.ID, "error", err.Error(), 0)
		return
	}
	if w := a.indexMedia(ctx, d); w != "" && warn == "" {
		warn = w
	}
	a.store.UpdateDocStatus(d.ID, "done", warn, 1)
	log.Printf("素材已入库 %s", d.Name)
}

// indexMedia 用 文件名+描述+标签+场景 建立（或重建）检索片段
func (a *App) indexMedia(ctx context.Context, d *store.Document) string {
	kind := "图片"
	if d.MediaType == "video" {
		kind = "视频"
	}
	text := fmt.Sprintf("【素材·%s】%s\n内容：%s\n标签：%s\n适合发送的场景：%s",
		kind, strings.TrimSuffix(d.Name, filepath.Ext(d.Name)), d.Description, d.Tags, d.Usage)
	cs := []store.Chunk{{DocID: d.ID, Kind: "media", Content: text}}
	warn := a.embedChunks(ctx, cs)
	ids, err := a.store.ReplaceChunks(d.ID, cs)
	if err != nil {
		return err.Error()
	}
	cs[0].ID = ids[0]
	a.index.RemoveDoc(d.ID)
	a.index.Add(cs)
	return warn
}

func mergeTags(a, b string) string {
	seen := map[string]bool{}
	var out []string
	for _, t := range strings.FieldsFunc(a+","+b, func(r rune) bool { return r == ',' || r == '，' || r == '、' }) {
		t = strings.TrimSpace(t)
		if t != "" && !seen[strings.ToLower(t)] {
			seen[strings.ToLower(t)] = true
			out = append(out, t)
		}
	}
	return strings.Join(out, ",")
}

// ---------- 推荐 ----------

type MediaItem struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	MediaType   string `json:"media_type"`
	URL         string `json:"url"`
	ThumbURL    string `json:"thumb_url"`
	Description string `json:"description"`
	Tags        string `json:"tags"`
	Usage       string `json:"usage"`
	Sent        bool   `json:"sent"` // 已经发给过这个客户
}

// fileURL 把数据目录内的绝对路径转成 /files/... 访问地址
func (a *App) fileURL(p string) string {
	if p == "" {
		return ""
	}
	rel, err := filepath.Rel(filepath.Join(a.dataDir, "files"), p)
	if err != nil || strings.HasPrefix(rel, "..") {
		return ""
	}
	return "/files/" + filepath.ToSlash(rel)
}

func (a *App) decorate(d *store.Document) {
	d.URL = a.fileURL(d.Path)
	d.ThumbURL = a.fileURL(d.Thumb)
	if d.ThumbURL == "" && d.MediaType == "image" {
		d.ThumbURL = d.URL
	}
}

func (a *App) mediaItem(id int64, sent map[int64]bool) (*MediaItem, bool) {
	d, err := a.store.GetDocument(id)
	if err != nil || d.Kind != "media" {
		return nil, false
	}
	a.decorate(d)
	return &MediaItem{ID: d.ID, Name: d.Name, MediaType: d.MediaType, URL: d.URL, ThumbURL: d.ThumbURL,
		Description: d.Description, Tags: d.Tags, Usage: d.Usage, Sent: sent[d.ID]}, true
}

// revealFile 在系统文件管理器中定位文件，方便拖进聊天软件发送
func revealFile(p string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", "-R", p).Start()
	case "windows":
		return exec.Command("explorer", "/select,", p).Start()
	default:
		return exec.Command("xdg-open", filepath.Dir(p)).Start()
	}
}
