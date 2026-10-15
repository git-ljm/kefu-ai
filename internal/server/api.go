package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/git-ljm/kefu-ai/internal/etl"
	"github.com/git-ljm/kefu-ai/internal/instance"
	"github.com/git-ljm/kefu-ai/internal/llm"
	"github.com/git-ljm/kefu-ai/internal/rag"
	"github.com/git-ljm/kefu-ai/internal/store"
)

const maskedKey = "********"

// secretKeys 页面上只显示 ******** 的设置项
var secretKeys = []string{"openai_key", "web_key", "embed_key"}

func (a *App) Routes(mux *http.ServeMux) {
	mux.Handle("/files/", http.StripPrefix("/files/", http.FileServer(http.Dir(filepath.Join(a.dataDir, "files")))))

	mux.HandleFunc("GET /api/ping", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, instance.PingInfo{App: instance.AppID, DataDir: a.dataDir, Version: a.Version, PID: os.Getpid()})
	})
	mux.HandleFunc("GET /api/settings", a.getSettings)
	mux.HandleFunc("PUT /api/settings", a.putSettings)
	mux.HandleFunc("GET /api/status", a.getStatus)
	mux.HandleFunc("GET /api/stats", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, a.store.Stats()) })

	mux.HandleFunc("GET /api/customers", a.listCustomers)
	mux.HandleFunc("POST /api/customers", a.createCustomer)
	mux.HandleFunc("GET /api/customers/{id}", a.getCustomer)
	mux.HandleFunc("PUT /api/customers/{id}", a.updateCustomer)
	mux.HandleFunc("DELETE /api/customers/{id}", a.deleteCustomer)
	mux.HandleFunc("POST /api/customers/{id}/analyze", a.analyze)
	mux.HandleFunc("POST /api/customers/{id}/reply", a.reply)
	mux.HandleFunc("POST /api/customers/{id}/profile", a.profile)
	mux.HandleFunc("POST /api/messages/{id}/suggest", a.resuggest)
	mux.HandleFunc("DELETE /api/messages/{id}", a.deleteMessage)
	mux.HandleFunc("POST /api/transform", a.transform)

	mux.HandleFunc("GET /api/docs", a.listDocs)
	mux.HandleFunc("POST /api/docs", a.uploadDocs)
	mux.HandleFunc("DELETE /api/docs/{id}", a.deleteDoc)
	mux.HandleFunc("GET /api/docs/{id}/chunks", a.docChunks)
	mux.HandleFunc("POST /api/docs/{id}/password", a.docPassword)
	mux.HandleFunc("POST /api/docs/reembed", a.reembed)
	mux.HandleFunc("GET /api/docs/reembed", a.reembedProgress)
	mux.HandleFunc("POST /api/embed/test", a.embedTest)
	mux.HandleFunc("GET /api/search", a.search)
	mux.HandleFunc("POST /api/ask", a.askHandler)
	mux.HandleFunc("POST /api/websearch/test", a.webTest)
	mux.HandleFunc("GET /api/backup", a.backup)
	mux.HandleFunc("GET /api/backup/full", a.backupFull)
	mux.HandleFunc("POST /api/datadir/open", a.openDataDir)

	mux.HandleFunc("GET /api/snippets", a.listSnippets)
	mux.HandleFunc("POST /api/snippets", a.createSnippet)
	mux.HandleFunc("PUT /api/snippets/{id}", a.updateSnippet)
	mux.HandleFunc("DELETE /api/snippets/{id}", a.deleteSnippet)

	mux.HandleFunc("GET /api/media", a.listMedia)
	mux.HandleFunc("PUT /api/media/{id}", a.updateMedia)
	mux.HandleFunc("POST /api/media/{id}/rescan", a.rescanMedia)
	mux.HandleFunc("POST /api/media/{id}/reveal", a.revealMedia)
	mux.HandleFunc("DELETE /api/media/{id}", a.deleteDoc)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func pathID(r *http.Request) int64 {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id
}

func readBody(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 10<<20)).Decode(v)
}

// ---------- 设置 ----------

func (a *App) getSettings(w http.ResponseWriter, r *http.Request) {
	m := map[string]string{}
	for k, v := range llm.DefaultSettings {
		m[k] = v
	}
	for k, v := range a.store.Settings() {
		m[k] = v
	}
	for _, k := range secretKeys {
		if m[k] != "" {
			m[k] = maskedKey
		}
	}
	writeJSON(w, m)
}

func (a *App) putSettings(w http.ResponseWriter, r *http.Request) {
	var m map[string]string
	if err := readBody(r, &m); err != nil {
		writeErr(w, 400, "参数错误")
		return
	}
	clean := map[string]string{}
	for k, v := range m {
		if _, ok := llm.DefaultSettings[k]; !ok {
			continue
		}
		if v == maskedKey && slices.Contains(secretKeys, k) {
			continue // 未修改
		}
		clean[k] = v
	}
	if err := a.store.SaveSettings(clean); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// 换了向量模型：旧向量作废，后台全部重建
	writeJSON(w, map[string]bool{"ok": true, "reembedding": a.CheckEmbedModel()})
}

func (a *App) getStatus(w http.ResponseWriter, r *http.Request) {
	cfg := llm.LoadConfig(a.store)
	url := r.URL.Query().Get("ollama_url")
	if url == "" {
		url = cfg.OllamaURL
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	models, err := a.llm.OllamaModels(ctx, url)
	res := map[string]any{"ollama_ok": err == nil, "models": models, "provider": cfg.Provider}
	if err != nil {
		res["ollama_error"] = err.Error()
	}
	has := func(name string) bool {
		for _, m := range models {
			if m == name || m == name+":latest" || strings.TrimSuffix(m, ":latest") == name {
				return true
			}
		}
		return false
	}
	res["checks"] = map[string]bool{
		"chat":   cfg.Provider == "openai" && cfg.OpenAIKey != "" || has(cfg.ChatModel),
		"vision": has(cfg.VisionModel) || (cfg.Provider == "openai" && cfg.OpenAIVision != ""),
		"embed":  (cfg.EmbedProvider == "openai" && cfg.EmbedKey != "") || (cfg.EmbedProvider != "openai" && has(cfg.EmbedModel)),
	}
	res["ffmpeg"] = etl.HasFFmpeg()
	res["data_dir"] = a.dataDir
	res["other_data_dir"] = a.OtherDataDir
	res["version"] = a.Version
	res["config"] = map[string]string{"chat": cfg.ChatModel, "vision": cfg.VisionModel, "embed": cfg.EmbedName(), "embed_provider": cfg.EmbedProvider}
	writeJSON(w, res)
}

// ---------- 客户 ----------

func (a *App) listCustomers(w http.ResponseWriter, r *http.Request) {
	cs, err := a.store.ListCustomers()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, cs)
}

func (a *App) createCustomer(w http.ResponseWriter, r *http.Request) {
	var c store.Customer
	if err := readBody(r, &c); err != nil || strings.TrimSpace(c.Name) == "" {
		writeErr(w, 400, "请填写客户名称")
		return
	}
	c.Name = strings.TrimSpace(c.Name)
	if err := a.store.AddCustomer(&c); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, c)
}

func (a *App) getCustomer(w http.ResponseWriter, r *http.Request) {
	c, err := a.store.GetCustomer(pathID(r))
	if err != nil {
		writeErr(w, 404, "客户不存在")
		return
	}
	msgs, _ := a.store.Messages(c.ID, 0)
	media := map[int64]*MediaItem{} // 聊天记录里发过的素材，用于显示缩略图
	for _, m := range msgs {
		for _, id := range store.ParseIDs(m.MediaIDs) {
			if it, ok := a.mediaItem(id, nil); ok {
				media[id] = it
			}
		}
	}
	writeJSON(w, map[string]any{"customer": c, "messages": msgs, "media": media})
}

func (a *App) updateCustomer(w http.ResponseWriter, r *http.Request) {
	c, err := a.store.GetCustomer(pathID(r))
	if err != nil {
		writeErr(w, 404, "客户不存在")
		return
	}
	var in store.Customer
	if err := readBody(r, &in); err != nil {
		writeErr(w, 400, "参数错误")
		return
	}
	if strings.TrimSpace(in.Name) != "" {
		c.Name = strings.TrimSpace(in.Name)
	}
	c.Platform, c.Notes, c.Tags = in.Platform, in.Notes, in.Tags
	if err := a.store.UpdateCustomer(c); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, c)
}

func (a *App) deleteCustomer(w http.ResponseWriter, r *http.Request) {
	if err := a.store.DeleteCustomer(pathID(r)); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// analyze 客服粘贴客户消息（文字和/或截图），AI 识别并生成回复建议
func (a *App) analyze(w http.ResponseWriter, r *http.Request) {
	c, err := a.store.GetCustomer(pathID(r))
	if err != nil {
		writeErr(w, 404, "客户不存在")
		return
	}
	if err := r.ParseMultipartForm(50 << 20); err != nil {
		writeErr(w, 400, "上传内容过大或格式错误")
		return
	}
	text := strings.TrimSpace(r.FormValue("text"))
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	role := r.FormValue("role")
	var imgPaths []string
	var extracted []string
	var warnings []string
	for _, fh := range r.MultipartForm.File["images"] {
		f, err := fh.Open()
		if err != nil {
			continue
		}
		data, _ := io.ReadAll(f)
		f.Close()
		p, err := a.saveFile("img", fh.Filename, data)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		rel, _ := filepath.Rel(filepath.Join(a.dataDir, "files"), p)
		imgPaths = append(imgPaths, filepath.ToSlash(rel))
		t, err := a.ExtractScreenshot(ctx, data)
		if err != nil {
			// 识别失败不影响文字部分，图片照样保存
			warnings = append(warnings, "截图识别失败："+err.Error())
			continue
		}
		extracted = append(extracted, t)
	}
	content := strings.TrimSpace(strings.Join(append([]string{text}, extracted...), "\n"))
	if content == "" {
		if len(warnings) > 0 {
			writeErr(w, 502, strings.Join(warnings, "；"))
			return
		}
		writeErr(w, 400, "请粘贴客户的消息或截图")
		return
	}
	// 记录我方在别处已经发出的消息（不需要 AI 回复）
	if role == "agent" {
		m := &store.Message{CustomerID: c.ID, Role: "agent", Content: content, ImagePath: strings.Join(imgPaths, ","), Action: "manual"}
		if err := a.store.AddMessage(m); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, map[string]any{"message": m})
		return
	}
	msg := &store.Message{CustomerID: c.ID, Role: "customer", Content: content, ImagePath: strings.Join(imgPaths, ",")}
	if err := a.store.AddMessage(msg); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	sug, err := a.Suggest(ctx, c, msg, "")
	if err != nil {
		writeJSON(w, map[string]any{"message": msg, "error": "AI 生成回复失败（可以在下面手动写回复）：" + err.Error()})
		return
	}
	if len(warnings) > 0 {
		sug.EmbedWarning = strings.TrimSpace(sug.EmbedWarning + " " + strings.Join(warnings, "；"))
	}
	msg.Translation = sug.CustomerZh
	writeJSON(w, map[string]any{"message": msg, "suggestion": sug})
}

func (a *App) resuggest(w http.ResponseWriter, r *http.Request) {
	msg, err := a.store.GetMessage(pathID(r))
	if err != nil || msg.Role != "customer" {
		writeErr(w, 404, "消息不存在")
		return
	}
	c, err := a.store.GetCustomer(msg.CustomerID)
	if err != nil {
		writeErr(w, 404, "客户不存在")
		return
	}
	var in struct {
		Hint string `json:"hint"`
	}
	_ = readBody(r, &in)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	sug, err := a.Suggest(ctx, c, msg, in.Hint)
	if err != nil {
		writeErr(w, 502, "AI 生成回复失败："+err.Error())
		return
	}
	writeJSON(w, sug)
}

func (a *App) reply(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	if _, err := a.store.GetCustomer(id); err != nil {
		writeErr(w, 404, "客户不存在")
		return
	}
	var in struct {
		ReplyTo  int64   `json:"reply_to"`
		Content  string  `json:"content"`
		AIReply  string  `json:"ai_reply"`
		MediaIDs []int64 `json:"media_ids"`
	}
	if err := readBody(r, &in); err != nil || (strings.TrimSpace(in.Content) == "" && len(in.MediaIDs) == 0) {
		writeErr(w, 400, "回复内容不能为空")
		return
	}
	m, err := a.SaveReply(r.Context(), id, in.ReplyTo, strings.TrimSpace(in.Content), in.AIReply, in.MediaIDs)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, m)
}

func (a *App) deleteMessage(w http.ResponseWriter, r *http.Request) {
	if err := a.store.DeleteMessage(pathID(r)); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) profile(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	p, err := a.UpdateProfile(ctx, pathID(r))
	if err != nil {
		writeErr(w, 502, "生成画像失败："+err.Error())
		return
	}
	writeJSON(w, map[string]string{"profile": p})
}

func (a *App) transform(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text string `json:"text"`
		Mode string `json:"mode"`
		Lang string `json:"lang"`
	}
	if err := readBody(r, &in); err != nil || strings.TrimSpace(in.Text) == "" {
		writeErr(w, 400, "内容不能为空")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()
	res, err := a.Transform(ctx, in.Text, in.Mode, in.Lang)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	writeJSON(w, res)
}

// ---------- 资料库 ----------

func (a *App) listDocs(w http.ResponseWriter, r *http.Request) {
	ds, err := a.store.ListDocuments(false)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, ds)
}

// saveUpload 把上传文件流式写入磁盘并计算哈希（视频可能很大，不整个读进内存）
func (a *App) saveUpload(fh *multipart.FileHeader, sub, name string) (path, hash string, err error) {
	src, err := fh.Open()
	if err != nil {
		return "", "", err
	}
	defer src.Close()
	dir := filepath.Join(a.dataDir, "files", sub)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	path = filepath.Join(dir, fmt.Sprintf("%d_%s", time.Now().UnixNano(), safeName(name)))
	dst, err := os.Create(path)
	if err != nil {
		return "", "", err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(dst, h), src)
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
		return "", "", err
	}
	return path, hex.EncodeToString(h.Sum(nil)), nil
}

func (a *App) uploadDocs(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeErr(w, 400, "上传失败："+err.Error())
		return
	}
	defer r.MultipartForm.RemoveAll()
	kind := r.FormValue("kind")
	if kind != "history" && kind != "media" {
		kind = "doc"
	}
	type result struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	}
	var results []result
	for i, fh := range r.MultipartForm.File["files"] {
		name := filepath.Base(strings.ReplaceAll(fh.Filename, "\\", "/"))
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "~$") {
			continue // 隐藏文件、Office 临时文件
		}
		mt := ""
		if kind == "media" {
			mt = etl.MediaTypeOf(name)
			if mt == "" && etl.IsKnownDocExt(name) {
				results = append(results, result{name, "素材库只支持图片和视频，已跳过"})
				continue
			}
		} else if msg := etl.RejectReason(name); msg != "" {
			results = append(results, result{name, msg})
			continue
		}
		sub := "docs"
		if kind == "media" {
			sub = "media"
		}
		p, hash, err := a.saveUpload(fh, sub, name)
		if err != nil {
			results = append(results, result{name, "保存失败：" + err.Error()})
			continue
		}
		if kind == "media" && mt == "" { // 没有后缀：按内容判断是不是图片/视频
			if mt = etl.SniffMediaType(p); mt == "" {
				os.Remove(p)
				results = append(results, result{name, "不是图片或视频，已跳过"})
				continue
			}
		}
		if _, dup := a.store.DocByHash(hash, kind); dup {
			os.Remove(p)
			results = append(results, result{name, "已存在，跳过"})
			continue
		}
		d := &store.Document{Name: name, Kind: kind, Path: p, Hash: hash, Status: "processing", MediaType: mt}
		if kind == "media" {
			d.Description = strings.TrimSpace(r.FormValue("desc_" + strconv.Itoa(i)))
			d.Tags = strings.TrimSpace(r.FormValue("tags"))
			// 浏览器截取的视频关键帧
			if ph := r.MultipartForm.File["poster_"+strconv.Itoa(i)]; len(ph) > 0 {
				if pp, _, err := a.saveUpload(ph[0], "media", name+".poster.jpg"); err == nil {
					d.Thumb = pp
				}
			}
		}
		if err := a.store.AddDocument(d); err != nil {
			os.Remove(p)
			results = append(results, result{name, "保存失败"})
			continue
		}
		if d.Thumb != "" {
			_ = a.store.UpdateMediaInfo(d)
		}
		if pw := r.FormValue("password"); pw != "" {
			a.passwords.Store(d.ID, pw)
		}
		a.jobs <- d.ID
		results = append(results, result{name, "已加入处理队列"})
	}
	writeJSON(w, results)
}

// ---------- 素材库 ----------

// listMedia 列出素材；带 q 参数时按内容检索
func (a *App) listMedia(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		ds, err := a.store.ListDocuments(true)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		for i := range ds {
			a.decorate(&ds[i])
		}
		writeJSON(w, ds)
		return
	}
	var qv []float32
	if vs, err := a.llm.Embed(r.Context(), []string{q}); err == nil {
		qv = vs[0]
	}
	out := []store.Document{}
	for _, h := range a.index.Search(q, qv, map[string]bool{"media": true}, 30) {
		if d, err := a.store.GetDocument(h.DocID); err == nil {
			a.decorate(d)
			out = append(out, *d)
		}
	}
	writeJSON(w, out)
}

func (a *App) updateMedia(w http.ResponseWriter, r *http.Request) {
	d, err := a.store.GetDocument(pathID(r))
	if err != nil || d.Kind != "media" {
		writeErr(w, 404, "素材不存在")
		return
	}
	var in struct{ Name, Description, Tags, Usage string }
	if err := readBody(r, &in); err != nil {
		writeErr(w, 400, "参数错误")
		return
	}
	if strings.TrimSpace(in.Name) != "" {
		d.Name = strings.TrimSpace(in.Name)
	}
	d.Description, d.Tags, d.Usage = strings.TrimSpace(in.Description), mergeTags(in.Tags, ""), strings.TrimSpace(in.Usage)
	if err := a.store.UpdateMediaInfo(d); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), time.Minute)
	defer cancel()
	warn := a.indexMedia(ctx, d)
	if d.Status == "done" {
		a.store.UpdateDocStatus(d.ID, "done", warn, 1)
	}
	a.decorate(d)
	writeJSON(w, d)
}

// rescanMedia 让 AI 重新识别（会覆盖描述和场景，保留人工标签）
func (a *App) rescanMedia(w http.ResponseWriter, r *http.Request) {
	d, err := a.store.GetDocument(pathID(r))
	if err != nil || d.Kind != "media" {
		writeErr(w, 404, "素材不存在")
		return
	}
	d.Description, d.Usage = "", ""
	_ = a.store.UpdateMediaInfo(d)
	a.store.UpdateDocStatus(d.ID, "processing", "", 0)
	a.jobs <- d.ID
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) revealMedia(w http.ResponseWriter, r *http.Request) {
	d, err := a.store.GetDocument(pathID(r))
	if err != nil || d.Kind != "media" {
		writeErr(w, 404, "素材不存在")
		return
	}
	if err := revealFile(d.Path); err != nil {
		writeErr(w, 500, "无法打开文件夹："+err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// backup 导出数据库快照（包含资料库、客户、聊天记录、设置）
func (a *App) backup(w http.ResponseWriter, r *http.Request) {
	tmp := filepath.Join(os.TempDir(), fmt.Sprintf("kefu-backup-%d.db", time.Now().UnixNano()))
	defer os.Remove(tmp)
	if err := a.store.Backup(tmp); err != nil {
		writeErr(w, 500, "备份失败："+err.Error())
		return
	}
	name := "kefu-backup-" + time.Now().Format("20060102-1504") + ".db"
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	http.ServeFile(w, r, tmp)
}

func (a *App) deleteDoc(w http.ResponseWriter, r *http.Request) {
	id := pathID(r)
	if d, err := a.store.GetDocument(id); err == nil {
		for _, p := range []string{d.Path, d.Thumb} {
			if p != "" {
				_ = os.Remove(p)
			}
		}
	}
	if err := a.store.DeleteDocument(id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	a.index.RemoveDoc(id)
	writeJSON(w, map[string]bool{"ok": true})
}

// docPassword 为有密码的文件提供密码并重新导入（密码不保存）
func (a *App) docPassword(w http.ResponseWriter, r *http.Request) {
	d, err := a.store.GetDocument(pathID(r))
	if err != nil || d.Kind == "media" || d.Kind == "example" {
		writeErr(w, 404, "资料不存在")
		return
	}
	var in struct {
		Password string `json:"password"`
	}
	if err := readBody(r, &in); err != nil || in.Password == "" {
		writeErr(w, 400, "请输入密码")
		return
	}
	a.passwords.Store(d.ID, in.Password)
	a.store.UpdateDocStatus(d.ID, "processing", "", 0)
	a.jobs <- d.ID
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) docChunks(w http.ResponseWriter, r *http.Request) {
	out, err := a.store.ChunkContents(pathID(r), 200)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, out)
}

// reembed 后台补全/重建向量，立即返回；进度用 GET /api/docs/reembed 查询
func (a *App) reembed(w http.ResponseWriter, r *http.Request) {
	a.StartReembed()
	writeJSON(w, a.reembedStatus())
}

func (a *App) reembedProgress(w http.ResponseWriter, r *http.Request) {
	st := a.reembedStatus()
	total, missing := a.store.EmbeddingStats()
	writeJSON(w, map[string]any{"state": st, "chunks": total, "missing": missing, "model": llm.LoadConfig(a.store).EmbedName()})
}

// embedTest 设置页“测试向量模型”：用当前（已保存的）设置生成一条向量
func (a *App) embedTest(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	vs, err := a.llm.Embed(ctx, []string{"测试 test"})
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "dim": len(vs[0]), "model": llm.LoadConfig(a.store).EmbedName()})
}

func (a *App) search(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, []rag.Hit{})
		return
	}
	var qv []float32
	if vs, err := a.llm.Embed(r.Context(), []string{q}); err == nil {
		qv = vs[0]
	}
	writeJSON(w, append([]rag.Hit{}, a.index.Search(q, qv, map[string]bool{"doc": true, "history": true, "example": true, "media": true, "snippet": true}, 10)...))
}
