package websearch

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSearxng(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []map[string]string{
			{"title": "T1", "url": "https://example.org/a", "content": "C1 " + r.URL.Query().Get("q")},
		}})
	}))
	defer srv.Close()
	rs, err := Search(context.Background(), map[string]string{"web_provider": "searxng", "web_searxng": srv.URL}, "peptide", 5)
	if err != nil || len(rs) != 1 || rs[0].Site != "example.org" || !strings.Contains(rs[0].Snippet, "peptide") {
		t.Fatalf("%+v %v", rs, err)
	}
}

func TestSearchNeedsConfig(t *testing.T) {
	if _, err := Search(context.Background(), map[string]string{}, "q", 5); err == nil {
		t.Error("未配置时应报错")
	}
	if _, err := Search(context.Background(), map[string]string{"web_provider": "bocha"}, "q", 5); err == nil {
		t.Error("缺 Key 时应报错")
	}
	if _, err := Search(context.Background(), map[string]string{"web_provider": ProviderModel}, "q", 5); err == nil {
		t.Error("大模型自带联网不应走搜索 API")
	}
}

func TestFriendlyError(t *testing.T) {
	err := friendly("ddg", errors.New(`Post "https://html.duckduckgo.com/html/": dial tcp: i/o timeout`), false)
	if !strings.Contains(err.Error(), "代理") {
		t.Fatalf("国外服务连不上时应提示代理：%v", err)
	}
	err = friendly("bocha", errors.New("HTTP 401: Invalid API KEY"), false)
	if strings.Contains(err.Error(), "代理") {
		t.Fatalf("Key 错误不应提示代理：%v", err)
	}
}
