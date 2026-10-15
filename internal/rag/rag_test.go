package rag

import (
	"testing"

	"github.com/git-ljm/kefu-ai/internal/store"
)

func TestSearchHybrid(t *testing.T) {
	ix := &Index{}
	ix.Load([]store.Chunk{
		{ID: 1, DocID: 1, Kind: "doc", Content: "订单号: A12345；状态: 已发货", Embedding: []float32{0, 1}},
		{ID: 2, DocID: 1, Kind: "doc", Content: "订单号: A12346；状态: 待发货", Embedding: []float32{0, 1}},
		{ID: 3, DocID: 2, Kind: "doc", Content: "退货政策：30 天内可退", Embedding: []float32{1, 0}},
		{ID: 4, DocID: 3, Kind: "media", Content: "黑色实拍图", Embedding: []float32{1, 0}},
	})
	kinds := map[string]bool{"doc": true}

	// 订单号精确命中必须排第一
	hits := ix.Search("where is my order A12345", []float32{0, 1}, kinds, 5)
	if len(hits) == 0 || hits[0].ID != 1 {
		t.Fatalf("订单号应精确命中：%+v", hits)
	}
	// 按意思（向量）命中
	hits = ix.Search("can I send it back", []float32{1, 0}, kinds, 5)
	if len(hits) == 0 || hits[0].ID != 3 {
		t.Fatalf("应按向量命中退货政策：%+v", hits)
	}
	// 只查指定类型
	for _, h := range ix.Search("黑色", []float32{1, 0}, kinds, 5) {
		if h.Kind != "doc" {
			t.Fatalf("不应返回 %s", h.Kind)
		}
	}
	// 没有向量时退化为关键词检索
	hits = ix.Search("退货", nil, kinds, 5)
	if len(hits) == 0 || hits[0].ID != 3 {
		t.Fatalf("关键词检索失败：%+v", hits)
	}
	// 删除文档后不再命中
	ix.RemoveDoc(2)
	if hits = ix.Search("退货", nil, kinds, 5); len(hits) != 0 {
		t.Fatalf("删除后仍命中：%+v", hits)
	}
}

func TestDimensionMismatchIgnored(t *testing.T) {
	ix := &Index{}
	ix.Load([]store.Chunk{{ID: 1, Kind: "doc", Content: "hello", Embedding: []float32{1, 0, 0}}})
	// 查询向量维度不同（换过向量模型）：不能拿来算相似度，也不能崩溃
	if hits := ix.Search("unrelated", []float32{1, 0}, map[string]bool{"doc": true}, 5); len(hits) != 0 {
		t.Fatalf("维度不同不应命中：%+v", hits)
	}
}
