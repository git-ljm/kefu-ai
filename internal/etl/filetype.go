package etl

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// ImageExt / VideoExt 按后缀识别的图片、视频格式
var (
	ImageExt = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".webp": true, ".gif": true, ".bmp": true}
	VideoExt = map[string]bool{".mp4": true, ".mov": true, ".m4v": true, ".webm": true, ".avi": true, ".mkv": true, ".3gp": true}
)

// MediaTypeOf 按后缀判断是图片还是视频，都不是返回 ""
func MediaTypeOf(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	switch {
	case ImageExt[ext]:
		return "image"
	case VideoExt[ext]:
		return "video"
	}
	return ""
}

// SniffMediaType 按文件内容判断图片/视频（用于没有后缀的文件）
func SniffMediaType(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	ct := http.DetectContentType(head)
	switch {
	case strings.HasPrefix(ct, "image/"):
		return "image"
	case strings.HasPrefix(ct, "video/"):
		return "video"
	case n >= 12 && string(head[4:8]) == "ftyp": // mp4 / mov / m4v / 3gp 的通用文件头
		return "video"
	}
	return ""
}

// IsKnownDocExt 后缀是否是能直接识别的文档格式（txt/xlsx/pdf…）
func IsKnownDocExt(name string) bool {
	_, ok := knownExt[strings.ToLower(filepath.Ext(name))]
	return ok
}
