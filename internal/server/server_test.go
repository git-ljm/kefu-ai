package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/git-ljm/kefu-ai/internal/store"
)

// fakeModel 同时模拟 Ollama（/api/*）和 OpenAI 兼容接口（/v1/*），记录收到的请求
type fakeModel struct {
	mu   sync.Mutex
	reqs []map[string]any
}

func (f *fakeModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	f.reqs = append(f.reqs, body)
	f.mu.Unlock()
	user := ""
	if msgs, ok := body["messages"].([]any); ok && len(msgs) > 1 {
		user, _ = msgs[len(msgs)-1].(map[string]any)["content"].(string)
	}
	answer := func() string {
		switch {
		case strings.Contains(user, "【联网】"):
			return `{"answer":"ONLINE_ANSWER（来源：example.org）","reply_en":"Store at -20°C.","covered":false}`
		case strings.Contains(user, "【问题】") && strings.Contains(user, "没有找到相关资料"):
			return `{"answer":"资料库没有","covered":false,"web_query":"peptide storage"}`
		case strings.Contains(user, "【问题】"):
			return `{"answer":"KB_ANSWER","covered":true}`
		}
		return `{"intent":"售后退换","reply":"You can return within 30 days.","reply_zh":"30天内可退","confidence":90,"sources":[1]}`
	}
	switch r.URL.Path {
	case "/api/embed":
		n := len(body["input"].([]any))
		out := make([][]float32, n)
		for i := range out {
			out[i] = []float32{1, 0}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": out})
	case "/api/chat":
		_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]string{"content": answer()}})
	case "/v1/chat/completions":
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": answer()}}}})
	default:
		w.WriteHeader(404)
	}
}

func (f *fakeModel) sawEnableSearch() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.reqs {
		if r["enable_search"] == true {
			return true
		}
	}
	return false
}

func setup(t *testing.T) (*httptest.Server, *fakeModel, *store.Store) {
	t.Helper()
	fm := &fakeModel{}
	model := httptest.NewServer(fm)
	t.Cleanup(model.Close)
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "kefu.db"))
	if err != nil {
		t.Fatal(err)
	}
	_ = st.SaveSettings(map[string]string{"ollama_url": model.URL})
	app := NewApp(st, dir, "test")
	app.StartWorkers()
	mux := http.NewServeMux()
	app.Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, fm, st
}

func call(t *testing.T, method, url string, body any, out any) int {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, rd)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if out != nil {
		_ = json.NewDecoder(res.Body).Decode(out)
	}
	return res.StatusCode
}

func TestCustomerFlow(t *testing.T) {
	srv, _, _ := setup(t)
	var c store.Customer
	call(t, "POST", srv.URL+"/api/customers", map[string]string{"name": "Tom"}, &c)

	// 粘贴客户消息 → AI 建议
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("text", "Can I return it?")
	mw.Close()
	res, err := http.Post(srv.URL+"/api/customers/1/analyze", mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	var an struct {
		Message    store.Message `json:"message"`
		Suggestion Suggestion    `json:"suggestion"`
	}
	_ = json.NewDecoder(res.Body).Decode(&an)
	res.Body.Close()
	if an.Suggestion.Reply == "" || an.Suggestion.Intent == "" {
		t.Fatalf("没有生成建议：%+v", an)
	}

	// 采纳 → 记录为 accepted
	var m store.Message
	call(t, "POST", srv.URL+"/api/customers/1/reply", map[string]any{"reply_to": an.Message.ID, "content": an.Suggestion.Reply, "ai_reply": an.Suggestion.Reply}, &m)
	if m.Action != "accepted" {
		t.Fatalf("action = %q", m.Action)
	}
	var list []store.Customer
	call(t, "GET", srv.URL+"/api/customers", nil, &list)
	if len(list) != 1 || list[0].LastRole != "agent" {
		t.Fatalf("customers: %+v", list)
	}
}

func TestSnippetCRUD(t *testing.T) {
	srv, _, _ := setup(t)
	var s Snippet
	if code := call(t, "POST", srv.URL+"/api/snippets", map[string]string{"question": "too expensive", "answer": "Quality first."}, &s); code != 200 || s.Title == "" {
		t.Fatalf("create %d %+v", code, s)
	}
	if code := call(t, "POST", srv.URL+"/api/snippets", map[string]string{"answer": "  "}, nil); code != 400 {
		t.Fatalf("空回复应拒绝，got %d", code)
	}
	var list []Snippet
	call(t, "GET", srv.URL+"/api/snippets?q=quality", nil, &list)
	if len(list) != 1 {
		t.Fatalf("search: %+v", list)
	}
	call(t, "DELETE", srv.URL+"/api/snippets/"+itoa(s.ID), nil, nil)
	call(t, "GET", srv.URL+"/api/snippets", nil, &list)
	if len(list) != 0 {
		t.Fatal("删除失败")
	}
}

func TestAskWithModelBuiltinSearch(t *testing.T) {
	srv, fm, st := setup(t)
	// 云端模型指向假的 OpenAI 兼容接口，联网方式选“大模型自带联网”
	ollama := st.Settings()["ollama_url"]
	_ = st.SaveSettings(map[string]string{
		"provider": "openai", "openai_base": ollama + "/v1", "openai_key": "k", "openai_model": "qwen3.8-flash",
		"web_provider": "model",
	})
	var r AskResult
	call(t, "POST", srv.URL+"/api/ask", map[string]string{"q": "how to store peptide", "web": "auto"}, &r)
	if !r.UsedWeb || !strings.Contains(r.Answer, "ONLINE_ANSWER") || r.Error != "" {
		t.Fatalf("应由模型联网回答：%+v", r)
	}
	if !fm.sawEnableSearch() {
		t.Fatal("请求里应带 enable_search=true")
	}

	// 本地 Ollama 不能自带联网：要给出明确提示
	_ = st.SaveSettings(map[string]string{"provider": "ollama"})
	var r2 AskResult
	call(t, "POST", srv.URL+"/api/ask", map[string]string{"q": "how to store peptide", "web": "auto"}, &r2)
	if r2.UsedWeb || !strings.Contains(r2.Error, "本地 Ollama 模型不能自己联网") {
		t.Fatalf("本地模型应提示不能联网：%+v", r2)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }
