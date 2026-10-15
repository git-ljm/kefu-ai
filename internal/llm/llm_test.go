package llm

import "testing"

func TestExtractJSON(t *testing.T) {
	var v struct {
		Reply string `json:"reply"`
	}
	for _, in := range []string{
		`{"reply":"ok"}`,
		"```json\n{\"reply\":\"ok\"}\n```",
		`好的，结果如下：{"reply":"ok"} 以上`,
	} {
		v.Reply = ""
		if err := ExtractJSON(in, &v); err != nil || v.Reply != "ok" {
			t.Errorf("%q: %v %q", in, err, v.Reply)
		}
	}
	if ExtractJSON("没有 JSON", &v) == nil {
		t.Error("应返回错误")
	}
}

func TestStripThink(t *testing.T) {
	if got := stripThink("<think>嗯…</think>\n答案"); got != "答案" {
		t.Fatalf("got %q", got)
	}
}

type fakeSettings map[string]string

func (f fakeSettings) Settings() map[string]string { return f }

func TestEmbedSignatureChangesWithModel(t *testing.T) {
	local := LoadConfig(fakeSettings{})
	cloud := LoadConfig(fakeSettings{"embed_provider": "openai", "embed_dim": "1024"})
	cloud512 := LoadConfig(fakeSettings{"embed_provider": "openai", "embed_dim": "512"})
	if local.EmbedSignature() == cloud.EmbedSignature() || cloud.EmbedSignature() == cloud512.EmbedSignature() {
		t.Fatal("换向量模型或维度后签名必须不同（否则不会触发重建）")
	}
	// 云端向量 Key 留空时沿用对话模型的 Key
	c := LoadConfig(fakeSettings{"embed_provider": "openai", "openai_key": "k1"})
	if c.EmbedKey != "k1" {
		t.Fatalf("EmbedKey = %q", c.EmbedKey)
	}
}
