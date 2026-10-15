// Command kefu 启动客服 AI 助手：单文件运行，自带网页界面，数据保存在本地。
//
//	kefu                    默认端口 8686，数据放在程序旁边的 kefu-data/
//	kefu -data D:\kefu-data 指定数据目录
//	kefu -no-browser        启动后不自动打开浏览器
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/git-ljm/kefu-ai/internal/instance"
	"github.com/git-ljm/kefu-ai/internal/server"
	"github.com/git-ljm/kefu-ai/internal/store"
	"github.com/git-ljm/kefu-ai/web"
)

// Version 打包时注入：go build -ldflags "-X main.Version=1.2.3"
var Version = "dev"

func main() {
	port := flag.Int("port", 0, "监听端口（默认 8686，被占用时依次尝试 8687~8705）")
	host := flag.String("host", "127.0.0.1", "监听地址（默认仅本机可访问；本程序没有登录功能，不建议改成 0.0.0.0）")
	dataDir := flag.String("data", "", "数据目录（默认：程序所在目录/kefu-data）")
	noBrowser := flag.Bool("no-browser", false, "启动后不自动打开浏览器")
	showVersion := flag.Bool("version", false, "显示版本号")
	flag.Parse()
	if *showVersion {
		fmt.Println(Version)
		return
	}

	dir, note := instance.ResolveDataDir(*dataDir)

	// 同一份数据已经有程序在运行（重复双击 / 多个快捷方式）：直接打开它，不再启动第二个
	runningPort, others := instance.FindRunning(dir, *port)
	if runningPort > 0 {
		url := fmt.Sprintf("http://127.0.0.1:%d", runningPort)
		fmt.Println("程序已经在运行，正在打开浏览器：" + url)
		if !*noBrowser {
			openBrowser(url)
		}
		time.Sleep(1500 * time.Millisecond)
		return
	}

	if err := os.MkdirAll(filepath.Join(dir, "files"), 0o755); err != nil {
		fatal("无法创建数据目录 %s: %v", dir, err)
	}
	// 同时双击两次时，两个进程可能都还没监听端口；用独占文件锁保证只有一个继续
	lockFile, ok := instance.AcquireLock(filepath.Join(dir, "kefu.instance.lock"))
	if !ok {
		fmt.Println("程序正在启动中，请稍候…")
		for i := 0; i < 40; i++ { // 最多等 20 秒，等另一个进程启动完成
			time.Sleep(500 * time.Millisecond)
			if p, _ := instance.FindRunning(dir, *port); p > 0 {
				if !*noBrowser {
					openBrowser(fmt.Sprintf("http://127.0.0.1:%d", p))
				}
				time.Sleep(1500 * time.Millisecond)
				return
			}
		}
		fatal("同一份数据已被另一个程序占用（数据目录 %s）。请在任务管理器中结束 kefu 后重试。", dir)
	}
	defer lockFile.Close()

	// 提醒：另一个数据目录里也有数据（以前从别的位置运行过程序）
	otherDir := ""
	if prev := instance.ReadPointer(); prev != "" && !instance.SamePath(prev, dir) && instance.HasDB(prev) {
		otherDir = prev
	}
	for _, o := range others {
		if otherDir == "" && !instance.SamePath(o.DataDir, dir) {
			otherDir = o.DataDir
		}
	}
	instance.SavePointer(dir)

	st, err := store.Open(filepath.Join(dir, "kefu.db"))
	if err != nil {
		fatal("打开数据库失败: %v", err)
	}
	server.AutoBackupOnUpgrade(st, dir, Version)
	app := server.NewApp(st, dir, Version)
	app.OtherDataDir = otherDir
	app.CheckEmbedModel()
	app.StartWorkers()

	mux := http.NewServeMux()
	app.Routes(mux)
	static := http.FileServer(http.FS(web.FS()))
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache") // 升级后浏览器立即用上新页面
		static.ServeHTTP(w, r)
	}))

	ln, err := instance.ListenStable(*host, *port)
	if err != nil {
		fatal("无法监听端口: %v", err)
	}
	listenPort := ln.Addr().(*net.TCPAddr).Port
	instance.WriteLock(dir, listenPort)
	url := fmt.Sprintf("http://127.0.0.1:%d", listenPort)
	fmt.Println("==============================================")
	fmt.Println(" 客服 AI 助手已启动  版本 " + Version)
	fmt.Println(" 浏览器打开: " + url)
	fmt.Println(" 数据目录:   " + dir + " " + note)
	if otherDir != "" {
		fmt.Println(" ⚠ 注意：另一个位置也有数据: " + otherDir)
		fmt.Println("   如果找不到以前的客户，请看页面顶部的提示")
	}
	fmt.Println(" 不要关闭此窗口，关闭即退出程序（再次双击快捷方式不会重复打开）")
	fmt.Println("==============================================")

	if !*noBrowser {
		go func() { time.Sleep(600 * time.Millisecond); openBrowser(url) }()
	}
	srv := &http.Server{Handler: logRequests(mux), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.Serve(ln))
}

// fatal 打印错误并等待，避免 Windows 上双击运行时窗口一闪而过看不到原因
func fatal(format string, args ...any) {
	fmt.Printf("\n❌ "+format+"\n", args...)
	if runtime.GOOS == "windows" {
		fmt.Println("按回车键关闭…")
		fmt.Scanln()
	}
	os.Exit(1)
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

// logRequests 只记录写操作，便于排查问题又不刷屏
func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") && r.Method != http.MethodGet {
			log.Printf("%s %s", r.Method, r.URL.Path)
		}
		h.ServeHTTP(w, r)
	})
}
