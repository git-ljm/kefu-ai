// Package rag 是内存中的混合检索：向量余弦相似度 + 关键词（订单号、型号精确命中加分）。
//
// 全部片段常驻内存、逐条打分，几万个片段以内毫秒级，不需要额外的向量数据库。
package rag

import (
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
	"unicode"

	"github.com/git-ljm/kefu-ai/internal/store"
)

type Index struct {
	mu     sync.RWMutex
	chunks []store.Chunk
}

func (ix *Index) Load(cs []store.Chunk) {
	ix.mu.Lock()
	ix.chunks = cs
	ix.mu.Unlock()
}

func (ix *Index) Add(cs []store.Chunk) {
	ix.mu.Lock()
	ix.chunks = append(ix.chunks, cs...)
	ix.mu.Unlock()
}

func (ix *Index) RemoveDoc(docID int64) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	kept := ix.chunks[:0]
	for _, c := range ix.chunks {
		if c.DocID != docID {
			kept = append(kept, c)
		}
	}
	ix.chunks = kept
}

type Hit struct {
	ID      int64   `json:"id"`
	DocID   int64   `json:"doc_id"`
	Kind    string  `json:"kind"`
	Content string  `json:"content"`
	Score   float64 `json:"score"`
}

// 订单号、型号、SKU 这类带数字的“硬关键词”
var hardTokenRe = regexp.MustCompile(`[A-Za-z0-9][A-Za-z0-9\-_#./]{3,}`)
var wordRe = regexp.MustCompile(`[A-Za-z]{3,}`)

var stopWords = map[string]bool{
	"the": true, "and": true, "you": true, "your": true, "for": true, "are": true, "can": true, "have": true,
	"what": true, "how": true, "this": true, "that": true, "with": true, "please": true, "hello": true, "thanks": true,
	"thank": true, "will": true, "when": true, "does": true, "about": true, "would": true, "could": true, "there": true,
}

func queryTerms(q string) (hard []string, soft []string) {
	lq := strings.ToLower(q)
	for _, t := range hardTokenRe.FindAllString(lq, -1) {
		if strings.IndexFunc(t, unicode.IsDigit) >= 0 {
			hard = append(hard, strings.Trim(t, ".-/"))
		}
	}
	seen := map[string]bool{}
	for _, w := range wordRe.FindAllString(lq, -1) {
		if !stopWords[w] && !seen[w] {
			seen[w] = true
			soft = append(soft, w)
		}
	}
	// 中文按二字切分
	var han []rune
	for _, r := range q {
		if unicode.Is(unicode.Han, r) {
			han = append(han, r)
		} else {
			han = append(han, ' ')
		}
	}
	rs := han
	for i := 0; i+1 < len(rs); i++ {
		if rs[i] != ' ' && rs[i+1] != ' ' {
			bg := string(rs[i : i+2])
			if !seen[bg] {
				seen[bg] = true
				soft = append(soft, bg)
			}
		}
	}
	return
}

// Search 在指定类型的片段中检索 topK
func (ix *Index) Search(query string, qv []float32, kinds map[string]bool, topK int) []Hit {
	hard, soft := queryTerms(query)
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	var hits []Hit
	for _, c := range ix.chunks {
		if !kinds[c.Kind] {
			continue
		}
		score, minScore := 0.0, 0.1 // 只有关键词时的门槛
		if len(qv) > 0 && len(c.Embedding) == len(qv) {
			score, minScore = cosine(qv, c.Embedding), 0.42
		}
		lc := strings.ToLower(c.Content)
		for _, h := range hard {
			if strings.Contains(lc, h) {
				score += 1.0 // 精确命中订单号/型号，强制排前
			}
		}
		if len(soft) > 0 {
			m := 0
			for _, s := range soft {
				if strings.Contains(lc, s) {
					m++
				}
			}
			score += 0.35 * float64(m) / float64(len(soft))
		}
		if score > minScore {
			hits = append(hits, Hit{ID: c.ID, DocID: c.DocID, Kind: c.Kind, Content: c.Content, Score: score})
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if len(hits) > topK {
		hits = hits[:topK]
	}
	return hits
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
