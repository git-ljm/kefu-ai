package server

// 自定义话术：客服在界面上手动添加/编辑，不需要上传文件。
// 存在 documents 表（kind='snippet'），字段对应：
//   name=标题/场景   tags=分类   description=客户常见问法   usage=推荐回复
// 生成回复时作为【自定义话术】单独检索，优先级高于普通资料。

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/git-ljm/kefu-ai/internal/store"
)

type snippetIn struct {
	Title    string `json:"title"`
	Category string `json:"category"`
	Question string `json:"question"`
	Answer   string `json:"answer"`
}

type Snippet struct {
	ID        int64  `json:"id"`
	Title     string `json:"title"`
	Category  string `json:"category"`
	Question  string `json:"question"`
	Answer    string `json:"answer"`
	CreatedAt int64  `json:"created_at"`
	Warning   string `json:"warning,omitempty"`
}

func toSnippet(d *store.Document) Snippet {
	return Snippet{ID: d.ID, Title: d.Name, Category: d.Tags, Question: d.Description, Answer: d.Usage, CreatedAt: d.CreatedAt, Warning: d.Error}
}

func snippetText(d *store.Document) string {
	var b strings.Builder
	b.WriteString("【自定义话术")
	if d.Tags != "" {
		b.WriteString("·" + d.Tags)
	}
	b.WriteString("】" + d.Name + "\n")
	if d.Description != "" {
		b.WriteString("客户常见问法：" + d.Description + "\n")
	}
	b.WriteString("推荐回复：" + d.Usage)
	return b.String()
}

// indexSnippet 建立（或重建）话术的检索片段
func (a *App) indexSnippet(ctx context.Context, d *store.Document) {
	cs := []store.Chunk{{DocID: d.ID, Kind: "snippet", Content: snippetText(d)}}
	warn := a.embedChunks(ctx, cs)
	ids, err := a.store.ReplaceChunks(d.ID, cs)
	if err != nil {
		warn = err.Error()
	} else {
		cs[0].ID = ids[0]
		a.index.RemoveDoc(d.ID)
		a.index.Add(cs)
	}
	a.store.UpdateDocStatus(d.ID, "done", warn, 1)
	d.Error = warn
}

func (in *snippetIn) clean() string {
	in.Title, in.Category = strings.TrimSpace(in.Title), mergeTags(in.Category, "")
	in.Question, in.Answer = strings.TrimSpace(in.Question), strings.TrimSpace(in.Answer)
	if in.Answer == "" {
		return "请填写回复话术"
	}
	if in.Title == "" { // 没写标题时用问法（没有就用回复）的前 24 个字
		src := in.Question
		if src == "" {
			src = in.Answer
		}
		r := []rune(strings.Join(strings.Fields(src), " "))
		if len(r) > 24 {
			r = append(r[:24], '…')
		}
		in.Title = string(r)
	}
	return ""
}

func (a *App) listSnippets(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	ds, err := a.store.ListSnippets()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := []Snippet{}
	for i := range ds {
		d := &ds[i]
		if q != "" && !strings.Contains(strings.ToLower(d.Name+"\n"+d.Tags+"\n"+d.Description+"\n"+d.Usage), q) {
			continue
		}
		out = append(out, toSnippet(d))
	}
	writeJSON(w, out)
}

func (a *App) createSnippet(w http.ResponseWriter, r *http.Request) {
	var in snippetIn
	if err := readBody(r, &in); err != nil {
		writeErr(w, 400, "参数错误")
		return
	}
	if msg := in.clean(); msg != "" {
		writeErr(w, 400, msg)
		return
	}
	d := &store.Document{Name: in.Title, Kind: "snippet", Status: "processing", Tags: in.Category, Description: in.Question, Usage: in.Answer}
	if err := a.store.AddDocument(d); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	d.CreatedAt = time.Now().Unix()
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	a.indexSnippet(ctx, d)
	writeJSON(w, toSnippet(d))
}

func (a *App) updateSnippet(w http.ResponseWriter, r *http.Request) {
	d, err := a.store.GetDocument(pathID(r))
	if err != nil || d.Kind != "snippet" {
		writeErr(w, 404, "话术不存在")
		return
	}
	var in snippetIn
	if err := readBody(r, &in); err != nil {
		writeErr(w, 400, "参数错误")
		return
	}
	if msg := in.clean(); msg != "" {
		writeErr(w, 400, msg)
		return
	}
	d.Name, d.Tags, d.Description, d.Usage = in.Title, in.Category, in.Question, in.Answer
	if err := a.store.UpdateMediaInfo(d); err != nil { // 同一组字段，复用
		writeErr(w, 500, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	a.indexSnippet(ctx, d)
	writeJSON(w, toSnippet(d))
}

func (a *App) deleteSnippet(w http.ResponseWriter, r *http.Request) {
	d, err := a.store.GetDocument(pathID(r))
	if err != nil || d.Kind != "snippet" {
		writeErr(w, 404, "话术不存在")
		return
	}
	if err := a.store.DeleteDocument(d.ID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	a.index.RemoveDoc(d.ID)
	writeJSON(w, map[string]bool{"ok": true})
}
