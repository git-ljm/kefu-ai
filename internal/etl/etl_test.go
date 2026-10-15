package etl

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"
)

func write(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDetectFormatWithoutExtension(t *testing.T) {
	gbk, _ := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte("发货时间：两个工作日内"))
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"notes", []byte("退货政策：30 天内可退。\nShipping: 5-7 days."), "text"},
		{"gbk_notes", gbk, "text"},
		{"page", []byte("<!DOCTYPE html><html><body>hi</body></html>"), "html"},
		{"doc", []byte("%PDF-1.4 ..."), "pdf"},
		{"pic", []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"), "image"},
	}
	for _, c := range cases {
		got, byContent, err := detectFormat(write(t, c.name, c.data), c.name)
		if err != nil || got != c.want || !byContent {
			t.Errorf("%s: got %q byContent=%v err=%v, want %q", c.name, got, byContent, err, c.want)
		}
	}
	if _, _, err := detectFormat(write(t, "junk", []byte{0, 1, 2, 3, 0, 255, 0}), "junk"); err == nil {
		t.Error("二进制垃圾文件应该识别失败")
	}
}

func TestExtractGBKText(t *testing.T) {
	gbk, _ := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte("发货时间：两个工作日内"))
	pieces, err := Extract(context.Background(), nil, write(t, "gbk", gbk), "gbk", "doc", "")
	if err != nil || len(pieces) != 1 || !strings.Contains(pieces[0], "两个工作日内") {
		t.Fatalf("GBK 文本应正确解码：%v %v", pieces, err)
	}
}

func TestRowsToChunks(t *testing.T) {
	rows := [][]string{
		{"订单号", "状态"},
		{"A12345", "已发货"},
		{"A12345", "已发货"}, // 重复行要去掉
		{"", ""},          // 空行要去掉
	}
	got := rowsToChunks("orders", rows, "doc")
	if len(got) != 1 || got[0] != "【orders】订单号: A12345；状态: 已发货" {
		t.Fatalf("got %q", got)
	}
	// 首行只有一个格子：当作标题；没有表头时用 | 连接
	got = rowsToChunks("话术", [][]string{{"谈价"}, {"嫌贵", "我理解您的顾虑…", "I understand…"}}, "doc")
	if len(got) != 1 || !strings.HasPrefix(got[0], "【话术 / 谈价】") || !strings.Contains(got[0], " | ") {
		t.Fatalf("got %q", got)
	}
}

func TestChunkTextSplitsLongText(t *testing.T) {
	long := strings.Repeat("这是一段很长的产品介绍。", 200)
	got := chunkText("产品", long)
	if len(got) < 2 {
		t.Fatalf("超长文本应切成多段，got %d", len(got))
	}
	for _, c := range got {
		if !strings.HasPrefix(c, "【产品】") {
			t.Fatalf("每段都应带标题: %q", c[:20])
		}
	}
}

func TestRejectReason(t *testing.T) {
	if RejectReason("a.xls") == "" || RejectReason("v.mp4") == "" {
		t.Error("老格式和视频应被拒绝")
	}
	if RejectReason("notes") != "" || RejectReason("a.conf") != "" {
		t.Error("无后缀和未知后缀应允许导入（按内容识别）")
	}
}
