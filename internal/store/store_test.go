package store

import (
	"path/filepath"
	"testing"
)

func TestStoreRoundTrip(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "kefu.db"))
	if err != nil {
		t.Fatal(err)
	}
	// 设置
	if err := s.SaveSettings(map[string]string{"a": "1"}); err != nil || s.Settings()["a"] != "1" {
		t.Fatalf("settings: %v", err)
	}
	// 资料 + 片段 + 向量
	d := &Document{Name: "policy.txt", Kind: "doc", Status: "done"}
	if err := s.AddDocument(d); err != nil {
		t.Fatal(err)
	}
	ids, err := s.AddChunks([]Chunk{{DocID: d.ID, Kind: "doc", Content: "退货", Embedding: []float32{0.5, -1.25}}})
	if err != nil {
		t.Fatal(err)
	}
	cs, _ := s.AllChunks()
	if len(cs) != 1 || cs[0].ID != ids[0] || cs[0].Embedding[1] != -1.25 {
		t.Fatalf("chunk round trip: %+v", cs)
	}
	if total, missing := s.EmbeddingStats(); total != 1 || missing != 0 {
		t.Fatalf("stats %d %d", total, missing)
	}
	_ = s.ClearEmbeddings()
	if _, missing := s.EmbeddingStats(); missing != 1 {
		t.Fatal("ClearEmbeddings 后应缺 1 个向量")
	}
	// 客户 + 消息
	c := &Customer{Name: "Tom"}
	if err := s.AddCustomer(c); err != nil {
		t.Fatal(err)
	}
	_ = s.AddMessage(&Message{CustomerID: c.ID, Role: "customer", Content: "hi"})
	_ = s.AddMessage(&Message{CustomerID: c.ID, Role: "agent", Content: "hello", MediaIDs: JoinIDs([]int64{d.ID})})
	list, _ := s.ListCustomers()
	if len(list) != 1 || list[0].LastRole != "agent" || list[0].LastMessage != "hello" {
		t.Fatalf("customers: %+v", list)
	}
	if !s.SentMedia(c.ID)[d.ID] {
		t.Fatal("SentMedia 应包含已发素材")
	}
	// 备份
	if err := s.Backup(filepath.Join(t.TempDir(), "b.db")); err != nil {
		t.Fatal(err)
	}
}

func TestParseIDs(t *testing.T) {
	got := ParseIDs(" 1, 2,x,,0,3")
	if len(got) != 3 || got[0] != 1 || got[2] != 3 {
		t.Fatalf("got %v", got)
	}
	if JoinIDs([]int64{4, 5}) != "4,5" {
		t.Fatal("JoinIDs")
	}
}
