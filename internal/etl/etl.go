// Package etl 把各种格式的资料统一转成可检索的文本片段（chunk）。
//
// 支持 Excel（含加密、合并单元格）、Word、PPT、PDF、CSV、HTML、纯文本（UTF-8 / GBK）和图片；
// 没有后缀或后缀不认识的文件按内容识别。
package etl

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"
	"github.com/xuri/excelize/v2"
	"golang.org/x/text/encoding/simplifiedchinese"
)

const chunkSize = 700 // 每个片段的大约字数

// knownExt 能直接按后缀识别的格式；其余（没有后缀、.conf、.FAQ 等任意后缀）按文件内容判断
var knownExt = map[string]string{
	".txt": "text", ".md": "text", ".json": "text", ".log": "text", ".csv": "csv", ".tsv": "csv",
	".html": "html", ".htm": "html",
	".xlsx": "xlsx", ".xlsm": "xlsx", ".docx": "docx", ".pptx": "pptx", ".pdf": "pdf",
}

// RejectReason 明显不能当资料导入的文件返回原因；返回 "" 表示可以导入（未知格式导入时再按内容识别）
func RejectReason(name string) string {
	switch ext := strings.ToLower(filepath.Ext(name)); {
	case ext == ".xls" || ext == ".doc" || ext == ".ppt":
		return "老格式，请用 Office/WPS 另存为 " + ext + "x 后再导入"
	case VideoExt[ext]:
		return "视频请放到「素材库」"
	case ext == ".exe" || ext == ".dll" || ext == ".zip" || ext == ".rar" || ext == ".7z" || ext == ".dmg" ||
		ext == ".mp3" || ext == ".wav" || ext == ".m4a" || ext == ".heic":
		return "不支持的格式，已跳过"
	}
	return ""
}

// detectFormat 先看后缀，后缀不认识（或没有后缀）就看文件内容
func detectFormat(path, name string) (format string, byContent bool, err error) {
	ext := strings.ToLower(filepath.Ext(name))
	if f, ok := knownExt[ext]; ok {
		return f, false, nil
	}
	if ImageExt[ext] {
		return "image", false, nil
	}
	fh, err := os.Open(path)
	if err != nil {
		return "", true, err
	}
	defer fh.Close()
	head := make([]byte, 8192)
	n, _ := io.ReadFull(fh, head)
	head = head[:n]
	switch {
	case n == 0:
		return "", true, errors.New("文件是空的")
	case bytes.HasPrefix(head, []byte("%PDF")):
		return "pdf", true, nil
	case bytes.HasPrefix(head, []byte("PK\x03\x04")), bytes.HasPrefix(head, oleMagic):
		return "office", true, nil // 具体是 xlsx/docx/pptx，打开后再看
	}
	if ct := http.DetectContentType(head); strings.HasPrefix(ct, "image/") {
		return "image", true, nil
	}
	if looksLikeText(head) {
		lower := strings.ToLower(string(head))
		if strings.Contains(lower, "<html") || strings.Contains(lower, "<!doctype html") {
			return "html", true, nil
		}
		return "text", true, nil
	}
	return "", true, errors.New("无法识别的文件格式（不是文本、Office、PDF 或图片）")
}

// looksLikeText 没有 NUL 字节，且能按 UTF-8 或 GBK 解码
func looksLikeText(b []byte) bool {
	if bytes.IndexByte(b, 0) >= 0 {
		return false
	}
	if len(b) > 4 { // 截断位置可能切在多字节字符中间，去掉末尾几个字节再判断
		b = b[:len(b)-4]
	}
	if utf8.Valid(b) {
		return true
	}
	_, err := simplifiedchinese.GB18030.NewDecoder().Bytes(b)
	return err == nil
}

// officeType 根据 zip 包里的内容判断是 xlsx / docx / pptx
func officeType(pkg []byte) string {
	zr, err := zip.NewReader(bytes.NewReader(pkg), int64(len(pkg)))
	if err != nil {
		return ""
	}
	for _, f := range zr.File {
		switch {
		case f.Name == "xl/workbook.xml":
			return "xlsx"
		case f.Name == "word/document.xml":
			return "docx"
		case f.Name == "ppt/presentation.xml":
			return "pptx"
		}
	}
	return ""
}

// ErrNeedPassword 文件有打开密码
var ErrNeedPassword = errors.New("文件有密码保护，请点「🔑 输入密码」后重新导入")

var oleMagic = []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}

// openOOXML 读取 xlsx/docx/pptx；加了打开密码的文件（外层是 OLE 加密容器）用密码解密
func openOOXML(path, password string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(raw, oleMagic) {
		return raw, nil
	}
	if password == "" {
		return nil, ErrNeedPassword
	}
	pkg, err := excelize.Decrypt(raw, &excelize.Options{Password: password})
	if err != nil {
		if errors.Is(err, excelize.ErrWorkbookPassword) {
			return nil, errors.New("密码不正确，请重新输入")
		}
		// .xls/.doc 老格式也是 OLE 容器，但不是加密的新格式
		return nil, fmt.Errorf("无法解密（如果是 .xls/.doc 老格式，请另存为 .xlsx/.docx）：%w", err)
	}
	if !bytes.HasPrefix(pkg, []byte("PK")) { // 密码错误时解密结果是乱码，不是 zip
		return nil, errors.New("密码不正确，请重新输入")
	}
	return pkg, nil
}

// ImageDescriber 把图片转成文字（由看图模型实现）
type ImageDescriber func(ctx context.Context, img []byte) (string, error)

// Extract 解析文件，返回文本片段。kind=history 时表格按多行合并，保持对话上下文；
// password 用于打开加密的 Office 文件，只在本次调用中使用
func Extract(ctx context.Context, describe ImageDescriber, path, name, kind, password string) ([]string, error) {
	if describe == nil {
		describe = func(context.Context, []byte) (string, error) { return "", errors.New("没有配置看图模型") }
	}
	return extract(ctx, describe, path, name, kind, password)
}

func extract(ctx context.Context, describe ImageDescriber, path, name, kind, password string) ([]string, error) {
	format, byContent, err := detectFormat(path, name)
	if err != nil {
		return nil, err
	}
	title := strings.TrimSuffix(name, filepath.Ext(name))
	if byContent || title == "" { // 没有后缀或后缀不认识时保留完整文件名
		title = name
	}
	// Office 文件（可能有密码）：先解密，再按包内容确认具体类型
	var pkg []byte
	if format == "xlsx" || format == "docx" || format == "pptx" || format == "office" {
		if pkg, err = openOOXML(path, password); err != nil {
			return nil, err
		}
		if t := officeType(pkg); t != "" {
			format = t
		} else if format == "office" {
			return nil, errors.New("无法识别的压缩包/Office 文件（.xls/.doc 老格式请另存为 .xlsx/.docx）")
		}
	}
	switch format {
	case "xlsx":
		return extractExcel(pkg, title, kind)
	case "csv":
		data, err := readText(path)
		if err != nil {
			return nil, err
		}
		r := csv.NewReader(strings.NewReader(data))
		r.LazyQuotes, r.FieldsPerRecord = true, -1
		if strings.EqualFold(filepath.Ext(name), ".tsv") {
			r.Comma = '\t'
		}
		rows, err := r.ReadAll()
		if err != nil {
			return chunkText(title, data), nil // 解析失败按纯文本处理
		}
		return rowsToChunks(title, rows, kind), nil
	case "docx":
		t, err := extractOfficeXML(pkg, "word/document.xml")
		if err != nil {
			return nil, err
		}
		return chunkText(title, t), nil
	case "pptx":
		t, err := extractOfficeXML(pkg, "ppt/slides/")
		if err != nil {
			return nil, err
		}
		return chunkText(title, t), nil
	case "pdf":
		t, err := extractPDF(path)
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(t) == "" {
			return nil, errors.New("PDF 中没有可提取的文字（可能是扫描件，请截图后作为图片导入）")
		}
		return chunkText(title, t), nil
	case "image":
		img, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		t, err := describe(ctx, img)
		if err != nil {
			return nil, err
		}
		return chunkText(title+"（图片）", t), nil
	case "html":
		data, err := readText(path)
		if err != nil {
			return nil, err
		}
		return chunkText(title, stripHTML(data)), nil
	default:
		data, err := readText(path)
		if err != nil {
			return nil, err
		}
		return chunkText(title, data), nil
	}
}

// readText 读取文本，自动识别 UTF-8 / GBK
func readText(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	b = bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})
	if !utf8.Valid(b) {
		if d, err := simplifiedchinese.GB18030.NewDecoder().Bytes(b); err == nil {
			b = d
		}
	}
	return string(b), nil
}

func extractExcel(pkg []byte, title, kind string) ([]string, error) {
	f, err := excelize.OpenReader(bytes.NewReader(pkg))
	if err != nil {
		return nil, fmt.Errorf("无法打开 Excel（.xls 老格式请另存为 .xlsx）: %w", err)
	}
	defer f.Close()
	var out []string
	for _, sheet := range f.GetSheetList() {
		rows, err := f.GetRows(sheet)
		if err != nil {
			continue
		}
		fillMerged(f, sheet, rows)
		out = append(out, rowsToChunks(title+" / "+sheet, rows, kind)...)
	}
	return out, nil
}

// fillMerged 合并单元格只有左上角有值，把值填到整个合并区域，
// 否则“场景/产品名”这类纵向合并的列，除第一行外都会丢失上下文
func fillMerged(f *excelize.File, sheet string, rows [][]string) {
	merged, err := f.GetMergeCells(sheet)
	if err != nil {
		return
	}
	for _, mc := range merged {
		c1, r1, err1 := excelize.CellNameToCoordinates(mc.GetStartAxis())
		c2, r2, err2 := excelize.CellNameToCoordinates(mc.GetEndAxis())
		if err1 != nil || err2 != nil || r2-r1 > 500 {
			continue
		}
		v := mc.GetCellValue()
		for r := r1; r <= r2 && r-1 < len(rows); r++ {
			for c := c1; c <= c2; c++ {
				for len(rows[r-1]) < c {
					rows[r-1] = append(rows[r-1], "")
				}
				if rows[r-1][c-1] == "" {
					rows[r-1][c-1] = v
				}
			}
		}
	}
}

func nonEmpty(r []string) (n, maxLen int) {
	for _, v := range r {
		if v = strings.TrimSpace(v); v != "" {
			n++
			maxLen = max(maxLen, utf8.RuneCountInString(v))
		}
	}
	return
}

// rowsToChunks 表格转文本：有表头时每行转成 “表头: 值”，便于按订单号/型号精确匹配；
// 没有表头（话术表、说明表常见）时用 “ | ” 连接。首行只有一个格子时当作表格标题
func rowsToChunks(title string, rows [][]string, kind string) []string {
	var clean [][]string
	for _, r := range rows {
		if n, _ := nonEmpty(r); n > 0 {
			clean = append(clean, r)
		}
	}
	if len(clean) == 0 {
		return nil
	}
	if n, _ := nonEmpty(clean[0]); n == 1 && len(clean) > 1 {
		for _, v := range clean[0] {
			if v = strings.TrimSpace(v); v != "" && !strings.HasSuffix(title, v) {
				title += " / " + v
			}
		}
		clean = clean[1:]
	}
	var header []string
	if n, ml := nonEmpty(clean[0]); n >= 2 && ml <= 30 && len(clean) > 1 {
		header, clean = clean[0], clean[1:]
	}
	var lines []string
	for _, r := range clean {
		var parts []string
		prev := ""
		for i, v := range r {
			v = strings.TrimSpace(v)
			if v == "" || v == prev { // 横向合并的单元格会重复，跳过
				continue
			}
			prev = v
			h := ""
			if i < len(header) {
				h = strings.TrimSpace(header[i])
			}
			if h != "" && h != v {
				parts = append(parts, h+": "+v)
			} else {
				parts = append(parts, v)
			}
		}
		if len(parts) > 0 {
			sep := " | "
			if header != nil {
				sep = "；"
			}
			lines = append(lines, strings.Join(parts, sep))
		}
	}
	if kind == "history" {
		return chunkText(title, strings.Join(lines, "\n"))
	}
	out := make([]string, 0, len(lines))
	seen := map[string]bool{}
	for _, l := range lines {
		if seen[l] { // 纵向合并单元格填充后可能出现完全相同的行
			continue
		}
		seen[l] = true
		if utf8.RuneCountInString(l) > chunkSize*2 { // 超长的一行（如大段产品介绍）再切分
			out = append(out, chunkText(title, strings.ReplaceAll(l, " | ", "\n"))...)
			continue
		}
		out = append(out, "【"+title+"】"+l)
	}
	return out
}

// extractOfficeXML 解析 docx/pptx 内部 XML 文本（prefix 可以是文件名或目录前缀）
func extractOfficeXML(pkg []byte, prefix string) (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(pkg), int64(len(pkg)))
	if err != nil {
		return "", fmt.Errorf("无法打开文件（.doc/.ppt 老格式请另存为 docx/pptx）: %w", err)
	}
	var files []*zip.File
	for _, f := range zr.File {
		if f.Name == prefix || (strings.HasSuffix(prefix, "/") && strings.HasPrefix(f.Name, prefix) && strings.HasSuffix(f.Name, ".xml") && !strings.Contains(f.Name[len(prefix):], "/")) {
			files = append(files, f)
		}
	}
	sort.Slice(files, func(i, j int) bool { return naturalLess(files[i].Name, files[j].Name) })
	var sb strings.Builder
	for _, f := range files {
		rc, err := f.Open()
		if err != nil {
			continue
		}
		dec := xml.NewDecoder(rc)
		inText := false
		for {
			tok, err := dec.Token()
			if err != nil {
				break
			}
			switch t := tok.(type) {
			case xml.StartElement:
				switch t.Name.Local {
				case "t":
					inText = true
				case "tab":
					sb.WriteString("\t")
				case "br":
					sb.WriteString("\n")
				}
			case xml.EndElement:
				switch t.Name.Local {
				case "t":
					inText = false
				case "p":
					sb.WriteString("\n")
				case "tc":
					sb.WriteString(" | ")
				}
			case xml.CharData:
				if inText {
					sb.Write(t)
				}
			}
		}
		rc.Close()
		sb.WriteString("\n")
	}
	return sb.String(), nil
}

var digitsRe = regexp.MustCompile(`\d+`)

func naturalLess(a, b string) bool {
	na, nb := digitsRe.FindString(a), digitsRe.FindString(b)
	if na != "" && nb != "" && len(na) != len(nb) {
		return len(na) < len(nb)
	}
	return a < b
}

func extractPDF(path string) (s string, err error) {
	defer func() { // 第三方库遇到异常 PDF 可能 panic
		if r := recover(); r != nil {
			err = fmt.Errorf("PDF 解析失败: %v", r)
		}
	}()
	f, r, err := pdf.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	rd, err := r.GetPlainText()
	if err != nil {
		return "", err
	}
	b, err := io.ReadAll(rd)
	return string(b), err
}

var (
	tagRe    = regexp.MustCompile(`(?s)<script.*?</script>|<style.*?</style>|<[^>]+>`)
	spacesRe = regexp.MustCompile(`[ \t]+`)
	blankRe  = regexp.MustCompile(`\n{3,}`)
)

func stripHTML(s string) string { return tagRe.ReplaceAllString(s, " ") }

// chunkText 按段落把长文本切成片段，相邻片段保留少量重叠
func chunkText(title, text string) []string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = spacesRe.ReplaceAllString(text, " ")
	text = blankRe.ReplaceAllString(text, "\n\n")
	var paras []string
	for _, p := range strings.Split(text, "\n") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		for utf8.RuneCountInString(p) > chunkSize { // 超长段落硬切
			r := []rune(p)
			paras = append(paras, string(r[:chunkSize]))
			p = string(r[chunkSize:])
		}
		paras = append(paras, p)
	}
	var out []string
	var cur []string
	size := 0
	flush := func() {
		if len(cur) > 0 {
			out = append(out, "【"+title+"】\n"+strings.Join(cur, "\n"))
		}
	}
	for _, p := range paras {
		n := utf8.RuneCountInString(p)
		if size+n > chunkSize && len(cur) > 0 {
			flush()
			last := cur[len(cur)-1] // 重叠：保留上一段的最后一行（不太长时）
			cur, size = nil, 0
			if utf8.RuneCountInString(last) < chunkSize/4 {
				cur, size = []string{last}, utf8.RuneCountInString(last)
			}
		}
		cur = append(cur, p)
		size += n
	}
	flush()
	return out
}
