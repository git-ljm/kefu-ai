// Package web 内嵌前端页面（原生 HTML/CSS/JS，无需构建），打包进单个可执行文件。
package web

import (
	"embed"
	"io/fs"
)

//go:embed static
var files embed.FS

// FS 前端静态文件（根目录即 static/）
func FS() fs.FS {
	sub, err := fs.Sub(files, "static")
	if err != nil {
		panic(err)
	}
	return sub
}
