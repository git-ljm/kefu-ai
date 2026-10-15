// Package llm 封装大模型调用：对话 / 看图（本地 Ollama 或任何 OpenAI 兼容接口）和向量（Ollama 或 OpenAI 兼容 /embeddings）。
//
// 模型配置不在这里保存，而是每次调用时从 Settings 读取，所以在设置页修改后立即生效。
package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Settings 提供键值形式的设置（由 store.Store 实现）
type Settings interface {
	Settings() map[string]string
}

// Config 模型配置，保存在 settings 表
type Config struct {
	Provider      string // ollama / openai（任何 OpenAI 兼容接口：DeepSeek、通义、Kimi、OpenAI…）
	OllamaURL     string
	ChatModel     string
	VisionModel   string
	EmbedModel    string
	OpenAIBase    string
	OpenAIKey     string
	OpenAIModel   string
	OpenAIVision  string
	ReplyLanguage string // auto 跟随客户 / en 固定英文
	Persona       string // 店铺/公司介绍、语气要求、签名等
	// 向量模型来源：ollama 本地 / openai 云端（百炼等 OpenAI 兼容 /embeddings 接口）
	EmbedProvider string
	EmbedBase     string
	EmbedKey      string
	EmbedCloud    string // 云端向量模型名
	EmbedDim      int    // 云端向量维度
}

// EmbedSignature 当前向量模型的“身份”。换了模型或维度，旧向量就不能再用，必须全部重建
func (c Config) EmbedSignature() string {
	if c.EmbedProvider == "openai" {
		return fmt.Sprintf("openai|%s|%s|%d", c.EmbedBase, c.EmbedCloud, c.EmbedDim)
	}
	return "ollama|" + c.EmbedModel
}

func (c Config) EmbedName() string {
	if c.EmbedProvider == "openai" {
		return c.EmbedCloud + "（云端）"
	}
	return c.EmbedModel
}

var DefaultSettings = map[string]string{
	"provider":       "ollama",
	"ollama_url":     "http://127.0.0.1:11434",
	"chat_model":     "qwen3:14b",
	"vision_model":   "qwen3-vl:8b-instruct", // instruct 版不输出思考过程，识别更快
	"embed_model":    "bge-m3",
	"openai_base":    "https://api.deepseek.com/v1",
	"openai_key":     "",
	"openai_model":   "deepseek-chat",
	"openai_vision":  "",
	"reply_language": "en",
	// 向量模型
	"embed_provider": "ollama",
	"embed_base":     "https://dashscope.aliyuncs.com/compatible-mode/v1",
	"embed_key":      "", // 留空则用云端大模型的 Key
	"embed_cloud":    "text-embedding-v4",
	"embed_dim":      "1024",
	// 联网搜索
	"web_provider": "",    // 空=关闭；bocha / ollama / tavily / searxng / ddg
	"web_key":      "",    // 搜索服务的 API Key
	"web_searxng":  "",    // SearXNG 地址
	"web_proxy":    "",    // 可选代理，如 http://127.0.0.1:7890
	"web_reply":    "off", // 写回复时：off 不联网 / auto 资料库没有时自动联网
	"persona":      "We are a professional online store. Be polite, warm, concise and helpful. Never promise anything not stated in the knowledge base.",
}

func LoadConfig(s Settings) Config {
	m := s.Settings()
	get := func(k string) string {
		if v, ok := m[k]; ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
		return DefaultSettings[k]
	}
	return Config{
		Provider:      get("provider"),
		OllamaURL:     strings.TrimRight(get("ollama_url"), "/"),
		ChatModel:     get("chat_model"),
		VisionModel:   get("vision_model"),
		EmbedModel:    get("embed_model"),
		OpenAIBase:    strings.TrimRight(get("openai_base"), "/"),
		OpenAIKey:     get("openai_key"),
		OpenAIModel:   get("openai_model"),
		OpenAIVision:  get("openai_vision"),
		ReplyLanguage: get("reply_language"),
		Persona:       get("persona"),
		EmbedProvider: get("embed_provider"),
		EmbedBase:     strings.TrimRight(get("embed_base"), "/"),
		EmbedKey: func() string {
			if k := get("embed_key"); k != "" {
				return k
			}
			return get("openai_key")
		}(),
		EmbedCloud: get("embed_cloud"),
		EmbedDim: func() int {
			n, _ := strconv.Atoi(get("embed_dim"))
			return n
		}(),
	}
}

type LLM struct {
	store Settings
	http  *http.Client
}

func New(s Settings) *LLM {
	return &LLM{store: s, http: &http.Client{Timeout: 5 * time.Minute}}
}

type ChatReq struct {
	System      string
	User        string
	Images      [][]byte
	JSON        bool
	Temperature float64 // 0 表示默认 0.3
	// WebSearch 让云端模型自己联网（阿里云百炼 enable_search）。本地 Ollama 模型不支持
	WebSearch bool
}

// ErrNoBuiltinSearch 当前对话模型不能自己联网
var ErrNoBuiltinSearch = errors.New("本地 Ollama 模型不能自己联网：请在「设置 → 云端大模型」选择「使用云端 API」并填写百炼（DashScope）的地址和 Key，或改用博查等搜索服务")

func (r ChatReq) temp() float64 {
	if r.Temperature > 0 {
		return r.Temperature
	}
	return 0.3
}

// Chat 调用对话模型；带图片时自动使用视觉模型
func (l *LLM) Chat(ctx context.Context, r ChatReq) (string, error) {
	cfg := LoadConfig(l.store)
	var out string
	var err error
	if r.WebSearch && !(cfg.Provider == "openai" && cfg.OpenAIKey != "") {
		return "", ErrNoBuiltinSearch
	}
	if cfg.Provider == "openai" && cfg.OpenAIKey != "" && (len(r.Images) == 0 || cfg.OpenAIVision != "") {
		model := cfg.OpenAIModel
		if len(r.Images) > 0 {
			model = cfg.OpenAIVision
		}
		out, err = l.openaiChat(ctx, cfg, model, r)
	} else {
		model := cfg.ChatModel
		if len(r.Images) > 0 {
			model = cfg.VisionModel
		}
		out, err = l.ollamaChat(ctx, cfg, model, r)
	}
	if err != nil {
		return "", err
	}
	return stripThink(out), nil
}

var thinkRe = regexp.MustCompile(`(?s)<think>.*?</think>`)

func stripThink(s string) string { return strings.TrimSpace(thinkRe.ReplaceAllString(s, "")) }

func (l *LLM) ollamaChat(ctx context.Context, cfg Config, model string, r ChatReq) (string, error) {
	type msg struct {
		Role    string   `json:"role"`
		Content string   `json:"content"`
		Images  []string `json:"images,omitempty"`
	}
	user := msg{Role: "user", Content: r.User}
	for _, img := range r.Images {
		user.Images = append(user.Images, base64.StdEncoding.EncodeToString(img))
	}
	body := map[string]any{
		"model":      model,
		"messages":   []msg{{Role: "system", Content: r.System}, user},
		"stream":     false,
		"keep_alive": "30m",
		"options":    map[string]any{"temperature": r.temp(), "num_ctx": 8192},
	}
	// 关闭 qwen3 / qwen3-vl 等模型的思考过程，回复更快；不支持的模型会在下面自动去掉重试
	body["think"] = false
	if r.JSON {
		body["format"] = "json"
	}
	var resp struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		Error string `json:"error"`
	}
	err := l.postJSON(ctx, cfg.OllamaURL+"/api/chat", "", body, &resp)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "think") {
		// 不支持 think 参数的模型 / 旧版 Ollama：去掉后重试
		delete(body, "think")
		err = l.postJSON(ctx, cfg.OllamaURL+"/api/chat", "", body, &resp)
	}
	if err != nil {
		return "", fmt.Errorf("Ollama(%s) 调用失败: %w", model, err)
	}
	if resp.Error != "" {
		return "", fmt.Errorf("Ollama(%s): %s", model, resp.Error)
	}
	return resp.Message.Content, nil
}

func (l *LLM) openaiChat(ctx context.Context, cfg Config, model string, r ChatReq) (string, error) {
	var userContent any = r.User
	if len(r.Images) > 0 {
		parts := []map[string]any{{"type": "text", "text": r.User}}
		for _, img := range r.Images {
			mime := http.DetectContentType(img)
			parts = append(parts, map[string]any{
				"type":      "image_url",
				"image_url": map[string]string{"url": "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(img)},
			})
		}
		userContent = parts
	}
	body := map[string]any{
		"model": model,
		"messages": []map[string]any{
			{"role": "system", "content": r.System},
			{"role": "user", "content": userContent},
		},
		"temperature": r.temp(),
	}
	if r.JSON && !r.WebSearch { // 联网时部分模型不支持 JSON 模式，靠提示词约束输出格式
		body["response_format"] = map[string]string{"type": "json_object"}
	}
	if r.WebSearch { // 阿里云百炼：让模型自己联网搜索（非 OpenAI 标准参数）
		body["enable_search"] = true
		body["search_options"] = map[string]any{"forced_search": true}
	}
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := l.postJSON(ctx, cfg.OpenAIBase+"/chat/completions", cfg.OpenAIKey, body, &resp); err != nil {
		return "", fmt.Errorf("云端模型(%s) 调用失败: %w", model, err)
	}
	if resp.Error != nil {
		return "", errors.New(resp.Error.Message)
	}
	if len(resp.Choices) == 0 {
		return "", errors.New("云端模型没有返回内容")
	}
	return resp.Choices[0].Message.Content, nil
}

// Embed 生成向量：默认本地 Ollama；设置里选了云端则走 OpenAI 兼容的 /embeddings 接口
func (l *LLM) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	cfg := LoadConfig(l.store)
	if cfg.EmbedProvider == "openai" {
		return l.embedCloud(ctx, cfg, texts)
	}
	var resp struct {
		Embeddings [][]float32 `json:"embeddings"`
		Error      string      `json:"error"`
	}
	body := map[string]any{"model": cfg.EmbedModel, "input": texts, "truncate": true}
	if err := l.postJSON(ctx, cfg.OllamaURL+"/api/embed", "", body, &resp); err != nil {
		return nil, fmt.Errorf("向量模型(%s) 调用失败: %w", cfg.EmbedModel, err)
	}
	if resp.Error != "" {
		return nil, fmt.Errorf("向量模型(%s): %s", cfg.EmbedModel, resp.Error)
	}
	if len(resp.Embeddings) != len(texts) {
		return nil, errors.New("向量数量不匹配")
	}
	return resp.Embeddings, nil
}

// embedCloud 百炼 text-embedding-v4 / qwen3.7-text-embedding、OpenAI text-embedding-3 等
func (l *LLM) embedCloud(ctx context.Context, cfg Config, texts []string) ([][]float32, error) {
	if cfg.EmbedKey == "" {
		return nil, errors.New("云端向量模型需要 API Key（设置 → 向量模型）")
	}
	const batch = 10 // 百炼 text-embedding-v4 单次最多 10 条
	out := make([][]float32, 0, len(texts))
	for i := 0; i < len(texts); i += batch {
		part := texts[i:min(i+batch, len(texts))]
		body := map[string]any{"model": cfg.EmbedCloud, "input": part, "encoding_format": "float"}
		if cfg.EmbedDim > 0 {
			body["dimensions"] = cfg.EmbedDim
		}
		var resp struct {
			Data []struct {
				Index     int       `json:"index"`
				Embedding []float32 `json:"embedding"`
			} `json:"data"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := l.postJSON(ctx, cfg.EmbedBase+"/embeddings", cfg.EmbedKey, body, &resp); err != nil {
			return nil, fmt.Errorf("云端向量模型(%s) 调用失败: %w", cfg.EmbedCloud, err)
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("云端向量模型(%s): %s", cfg.EmbedCloud, resp.Error.Message)
		}
		if len(resp.Data) != len(part) {
			return nil, errors.New("向量数量不匹配")
		}
		vecs := make([][]float32, len(part))
		for _, d := range resp.Data {
			if d.Index >= 0 && d.Index < len(vecs) {
				vecs[d.Index] = d.Embedding
			}
		}
		out = append(out, vecs...)
	}
	return out, nil
}

// OllamaModels 列出本地已安装模型
func (l *LLM) OllamaModels(ctx context.Context, url string) ([]string, error) {
	if url == "" {
		url = LoadConfig(l.store).OllamaURL
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(url, "/")+"/api/tags", nil)
	c := &http.Client{Timeout: 5 * time.Second}
	res, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var out struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, err
	}
	names := []string{}
	for _, m := range out.Models {
		names = append(names, m.Name)
	}
	return names, nil
}

func (l *LLM) postJSON(ctx context.Context, url, key string, body any, out any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	res, err := l.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 400 {
		msg := string(data)
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return fmt.Errorf("HTTP %d: %s", res.StatusCode, msg)
	}
	return json.Unmarshal(data, out)
}

// extractJSON 从模型输出中取出第一个 JSON 对象
func ExtractJSON(s string, v any) error {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "```json")
	s = strings.TrimPrefix(s, "```")
	s = strings.TrimSuffix(s, "```")
	if err := json.Unmarshal([]byte(s), v); err == nil {
		return nil
	}
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end <= start {
		return errors.New("模型输出不是 JSON")
	}
	return json.Unmarshal([]byte(s[start:end+1]), v)
}
