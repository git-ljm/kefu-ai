// Package websearch 联网搜索：资料库里实在没有的通用知识，去网上查。
//
// 支持的服务（在「设置 → 联网搜索」里选）：
//
//	bocha   博查        国内可直接访问，需要 API Key（open.bochaai.com）
//	ollama  Ollama      需要 ollama.com 账号的 API Key，国内可能要代理
//	tavily  Tavily      需要 API Key，每月有免费额度，国内可能要代理
//	searxng SearXNG     自己部署的搜索服务，不需要 Key
//	ddg     DuckDuckGo  免费、不需要 Key，国内需要代理
//
// 隐私：调用方应传入 AI 提炼后的搜索关键词（去掉客户姓名、订单号、联系方式），而不是客户原话。
package websearch

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type Result struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
	Site    string `json:"site,omitempty"`
}

var ProviderNames = map[string]string{
	"model": "大模型自带联网", "bocha": "博查", "ollama": "Ollama", "tavily": "Tavily", "searxng": "SearXNG", "ddg": "DuckDuckGo",
}

// ProviderModel 由对话模型自己联网（如阿里云百炼的 enable_search），不走搜索 API，调用方自行处理
const ProviderModel = "model"

// overseas 在国内通常需要代理才能访问的服务
var overseas = map[string]bool{"ollama": true, "tavily": true, "ddg": true}

// friendly 把网络层错误翻译成能看懂的提示
func friendly(provider string, err error, hasProxy bool) error {
	msg := err.Error()
	low := strings.ToLower(msg)
	netErr := strings.Contains(low, "timeout") || strings.Contains(low, "deadline") || strings.Contains(low, "connection refused") ||
		strings.Contains(low, "connection reset") || strings.Contains(low, "no such host") || strings.Contains(low, "eof") ||
		strings.Contains(low, "tls") || strings.Contains(low, "dial tcp")
	switch {
	case netErr && overseas[provider] && !hasProxy:
		return fmt.Errorf("连不上 %s（国内网络通常无法直接访问）。请在「代理」一栏填写代理地址，如 http://127.0.0.1:7890；或改用「博查」或「大模型自带联网」。原始错误：%s", ProviderNames[provider], Trunc(msg, 120))
	case netErr && hasProxy:
		return fmt.Errorf("通过代理仍连不上 %s，请检查代理是否开启、地址端口是否正确。原始错误：%s", ProviderNames[provider], Trunc(msg, 120))
	case netErr:
		return fmt.Errorf("连不上 %s，请检查网络。原始错误：%s", ProviderNames[provider], Trunc(msg, 120))
	}
	return fmt.Errorf("%s 搜索失败：%w", ProviderNames[provider], err)
}

func Enabled(m map[string]string) bool {
	return ProviderNames[m["web_provider"]] != ""
}

func client(m map[string]string) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if p := strings.TrimSpace(m["web_proxy"]); p != "" {
		if u, err := url.Parse(p); err == nil && u.Host != "" {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	return &http.Client{Timeout: 20 * time.Second, Transport: tr}
}

// Search 按设置（web_provider / web_key / web_searxng / web_proxy）调用对应的搜索服务，最多返回 n 条
func Search(ctx context.Context, m map[string]string, query string, n int) ([]Result, error) {
	provider, key := m["web_provider"], strings.TrimSpace(m["web_key"])
	if !Enabled(m) {
		return nil, errors.New("还没有配置联网搜索（设置 → 联网搜索）")
	}
	if provider == ProviderModel {
		return nil, errors.New("「大模型自带联网」不返回搜索结果列表，由调用方直接让模型联网回答")
	}
	if provider != "searxng" && provider != "ddg" && key == "" {
		return nil, fmt.Errorf("%s 需要填写 API Key（设置 → 联网搜索）", ProviderNames[provider])
	}
	query = strings.TrimSpace(query)
	if r := []rune(query); len(r) > 200 {
		query = string(r[:200])
	}
	c := client(m)
	var out []Result
	var err error
	switch provider {
	case "bocha":
		out, err = searchBocha(ctx, c, key, query, n)
	case "ollama":
		out, err = searchOllama(ctx, c, key, query, n)
	case "tavily":
		out, err = searchTavily(ctx, c, key, query, n)
	case "searxng":
		out, err = searchSearxng(ctx, c, m["web_searxng"], query, n)
	case "ddg":
		out, err = searchDDG(ctx, c, query, n)
	}
	if err != nil {
		return nil, friendly(provider, err, strings.TrimSpace(m["web_proxy"]) != "")
	}
	for i := range out {
		out[i].Title = strings.TrimSpace(out[i].Title)
		out[i].Snippet = Trunc(strings.TrimSpace(out[i].Snippet), 600)
		if out[i].Site == "" {
			if u, err := url.Parse(out[i].URL); err == nil {
				out[i].Site = u.Host
			}
		}
	}
	if len(out) > n {
		out = out[:n]
	}
	return out, nil
}

// Trunc 按字符截断，超出部分用“…”表示
func Trunc(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func doJSON(ctx context.Context, c *http.Client, method, u, key string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	res, err := c.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, 5<<20))
	if res.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d: %s", res.StatusCode, Trunc(string(data), 200))
	}
	return json.Unmarshal(data, out)
}

func searchBocha(ctx context.Context, c *http.Client, key, q string, n int) ([]Result, error) {
	var r struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			WebPages struct {
				Value []struct {
					Name, URL, SiteName, Snippet, Summary string
				} `json:"value"`
			} `json:"webPages"`
		} `json:"data"`
		// 部分版本直接返回 webPages
		WebPages struct {
			Value []struct {
				Name, URL, SiteName, Snippet, Summary string
			} `json:"value"`
		} `json:"webPages"`
	}
	err := doJSON(ctx, c, "POST", "https://api.bochaai.com/v1/web-search", key,
		map[string]any{"query": q, "summary": true, "count": n}, &r)
	if err != nil {
		return nil, err
	}
	if r.Code != 0 && r.Code != 200 && r.Msg != "" {
		return nil, errors.New(r.Msg)
	}
	vals := r.Data.WebPages.Value
	if len(vals) == 0 {
		vals = r.WebPages.Value
	}
	var out []Result
	for _, v := range vals {
		s := v.Summary
		if s == "" {
			s = v.Snippet
		}
		out = append(out, Result{Title: v.Name, URL: v.URL, Snippet: s, Site: v.SiteName})
	}
	return out, nil
}

func searchOllama(ctx context.Context, c *http.Client, key, q string, n int) ([]Result, error) {
	var r struct {
		Results []struct{ Title, URL, Content string } `json:"results"`
	}
	if err := doJSON(ctx, c, "POST", "https://ollama.com/api/web_search", key,
		map[string]any{"query": q, "max_results": min(n, 10)}, &r); err != nil {
		return nil, err
	}
	var out []Result
	for _, v := range r.Results {
		out = append(out, Result{Title: v.Title, URL: v.URL, Snippet: v.Content})
	}
	return out, nil
}

func searchTavily(ctx context.Context, c *http.Client, key, q string, n int) ([]Result, error) {
	var r struct {
		Results []struct{ Title, URL, Content string } `json:"results"`
	}
	if err := doJSON(ctx, c, "POST", "https://api.tavily.com/search", key,
		map[string]any{"query": q, "max_results": n, "search_depth": "basic"}, &r); err != nil {
		return nil, err
	}
	var out []Result
	for _, v := range r.Results {
		out = append(out, Result{Title: v.Title, URL: v.URL, Snippet: v.Content})
	}
	return out, nil
}

func searchSearxng(ctx context.Context, c *http.Client, base, q string, n int) ([]Result, error) {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return nil, errors.New("请填写 SearXNG 地址，如 http://192.168.1.20:8080")
	}
	var r struct {
		Results []struct{ Title, URL, Content string } `json:"results"`
	}
	u := base + "/search?format=json&q=" + url.QueryEscape(q)
	if err := doJSON(ctx, c, "GET", u, "", nil, &r); err != nil {
		return nil, fmt.Errorf("%w（SearXNG 需要在 settings.yml 的 formats 里开启 json）", err)
	}
	var out []Result
	for _, v := range r.Results {
		out = append(out, Result{Title: v.Title, URL: v.URL, Snippet: v.Content})
	}
	return out, nil
}

var (
	ddgResultRe  = regexp.MustCompile(`(?s)<a[^>]+class="result__a"[^>]+href="([^"]+)"[^>]*>(.*?)</a>.*?class="result__snippet"[^>]*>(.*?)</a>`)
	htmlTagRe    = regexp.MustCompile(`<[^>]+>`)
	ddgUddgParam = regexp.MustCompile(`[?&]uddg=([^&]+)`)
)

func searchDDG(ctx context.Context, c *http.Client, q string, n int) ([]Result, error) {
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://html.duckduckgo.com/html/",
		strings.NewReader(url.Values{"q": {q}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140 Safari/537.36")
	res, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 3<<20))
	if res.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d", res.StatusCode)
	}
	clean := func(s string) string { return html.UnescapeString(htmlTagRe.ReplaceAllString(s, "")) }
	var out []Result
	for _, m := range ddgResultRe.FindAllStringSubmatch(string(body), n) {
		link := html.UnescapeString(m[1])
		if mm := ddgUddgParam.FindStringSubmatch(link); mm != nil { // 跳转链接里取真实地址
			if u, err := url.QueryUnescape(mm[1]); err == nil {
				link = u
			}
		}
		out = append(out, Result{Title: clean(m[2]), URL: link, Snippet: clean(m[3])})
	}
	if len(out) == 0 {
		return nil, errors.New("没有拿到结果（可能被限流或网络不通）")
	}
	return out, nil
}

// Context 把搜索结果整理成给模型看的文本，编号 W1、W2…
func Context(rs []Result) string {
	var b strings.Builder
	for i, r := range rs {
		fmt.Fprintf(&b, "[W%d] %s（%s）\n%s\n", i+1, r.Title, r.Site, r.Snippet)
	}
	return b.String()
}

// Rules 告诉模型网络结果能用在哪、不能用在哪
const Rules = `关于【网络搜索结果】：
- 它不是店铺资料，只能用于通用知识（产品原理、成分常识、行业常识、法规常识、快递公司常识等）。
- 本店的价格、库存、发货时间、优惠、订单状态、售后承诺，绝不能用网络结果回答。
- 与【资料库】或【自定义话术】冲突时，一律以店铺资料为准。
- 用到网络信息时，在 missing_info 里写明“含网络信息，请核实”。`
