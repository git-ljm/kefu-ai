// Package instance 负责单实例运行与数据目录定位。
package instance

// 单实例与数据目录定位
//
// 解决的问题（Windows 桌面快捷方式多次打开时最明显）：
//  1. 重复打开时要可靠地找到已在运行的程序，而不是再启动一个（不同端口 = 浏览器里不同网站，记不住当前客户）
//  2. 端口固定在 8686 起的一小段范围内，不用随机端口
//  3. 程序被移动/复制到别处（比如拖到桌面）后，仍然找到原来的数据，而不是新建一个空库

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	basePort  = 8686
	portRange = 20 // 8686 ~ 8705
	AppID     = "kefu-ai-assistant"
)

type PingInfo struct {
	App     string `json:"app"`
	DataDir string `json:"data_dir"`
	Version string `json:"version"`
	PID     int    `json:"pid"`
}

// ping 轻量探测：不访问数据库和 Ollama，毫秒级返回
func ping(port int) (*PingInfo, bool) {
	c := &http.Client{Timeout: 800 * time.Millisecond}
	res, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/api/ping", port))
	if err != nil {
		return nil, false
	}
	defer res.Body.Close()
	var p PingInfo
	if res.StatusCode != 200 || json.NewDecoder(res.Body).Decode(&p) != nil || p.App != AppID {
		return nil, false
	}
	return &p, true
}

func SamePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "windows" || runtime.GOOS == "darwin" { // 这两个系统默认不区分大小写
		return strings.EqualFold(a, b)
	}
	return a == b
}

// FindRunning 找使用同一数据目录、正在运行的实例；others 返回使用其他数据目录的实例
func FindRunning(dataDir string, preferPort int) (port int, others []PingInfo) {
	cands := map[int]bool{}
	if lp := readLockPort(dataDir); lp > 0 {
		cands[lp] = true
	}
	if preferPort > 0 {
		cands[preferPort] = true
	}
	for p := basePort; p < basePort+portRange; p++ {
		cands[p] = true
	}
	// 并行探测，整体耗时 ≈ 单个端口的耗时
	type res struct {
		port int
		info *PingInfo
	}
	ch := make(chan res, len(cands))
	for p := range cands {
		go func(p int) {
			info, _ := ping(p)
			ch <- res{p, info}
		}(p)
	}
	for range cands {
		r := <-ch
		if r.info == nil {
			continue
		}
		if SamePath(r.info.DataDir, dataDir) {
			if port == 0 || r.port < port {
				port = r.port
			}
		} else {
			others = append(others, *r.info)
		}
	}
	return port, others
}

// ListenStable 在固定范围内找第一个空闲端口（同一台电脑上基本每次都是 8686）
func ListenStable(host string, prefer int) (net.Listener, error) {
	if prefer > 0 {
		if ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", host, prefer)); err == nil {
			return ln, nil
		}
	}
	for p := basePort; p < basePort+portRange; p++ {
		if ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", host, p)); err == nil {
			return ln, nil
		}
	}
	return net.Listen("tcp", host+":0")
}

// ---------- 锁文件：记录当前实例的端口 ----------

func lockPath(dataDir string) string { return filepath.Join(dataDir, "kefu.lock") }

func readLockPort(dataDir string) int {
	b, err := os.ReadFile(lockPath(dataDir))
	if err != nil {
		return 0
	}
	var v struct {
		Port int `json:"port"`
	}
	_ = json.Unmarshal(b, &v)
	return v.Port
}

func WriteLock(dataDir string, port int) {
	b, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "port": port, "started": time.Now().Format(time.RFC3339)})
	_ = os.WriteFile(lockPath(dataDir), b, 0o644)
}

// ---------- 数据目录定位 ----------

// pointerFile 记录“上次使用的数据目录”，放在系统的用户配置目录
// Windows: %AppData%\kefu-ai\data_dir.txt   Mac: ~/Library/Application Support/kefu-ai/data_dir.txt
func pointerFile() string {
	d, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "kefu-ai", "data_dir.txt")
}

func ReadPointer() string {
	p := pointerFile()
	if p == "" {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func SavePointer(dir string) {
	p := pointerFile()
	if p == "" {
		return
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, []byte(dir), 0o644)
}

func HasDB(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, "kefu.db"))
	return err == nil && st.Size() > 0
}

// ResolveDataDir 决定数据目录，返回目录和一句说明（显示在启动窗口）
//
//	优先级：命令行 -data  >  程序旁边已有的 kefu-data  >  上次使用的目录（程序被移动/复制后仍能找到）
//	       >  程序旁边新建 kefu-data（不可写或在临时目录时改用用户目录）
func ResolveDataDir(flagDir string) (dir, note string) {
	if flagDir != "" {
		abs, _ := filepath.Abs(flagDir)
		return abs, "（命令行指定）"
	}
	exe, err := os.Executable()
	if err == nil {
		exe, _ = filepath.EvalSymlinks(exe)
	}
	exeDir := filepath.Dir(exe)
	local := filepath.Join(exeDir, "kefu-data")
	inTemp := IsTempDir(exeDir)

	if !inTemp && HasDB(local) {
		return local, ""
	}
	if prev := ReadPointer(); prev != "" && HasDB(prev) && !SamePath(prev, local) {
		return prev, "（沿用上次的数据目录。程序可能被移动或复制过，数据仍在原处）"
	}
	home, _ := os.UserHomeDir()
	if inTemp {
		// go run 开发时用项目目录；双击压缩包内的 exe 时用用户目录，避免数据被系统清理
		if wd, err := os.Getwd(); err == nil && !IsTempDir(wd) {
			if _, err := os.Stat(filepath.Join(wd, "go.mod")); err == nil {
				return filepath.Join(wd, "kefu-data"), ""
			}
		}
		return filepath.Join(home, "kefu-data"), "（程序在临时目录运行，数据改存到用户目录。建议先把程序解压到固定文件夹）"
	}
	if err := os.MkdirAll(local, 0o755); err == nil && writable(local) {
		return local, ""
	}
	return filepath.Join(home, "kefu-data"), "（程序所在位置不可写，数据改存到用户目录）"
}

// IsTempDir 判断是否位于系统临时目录（Windows 下忽略大小写，并兼容 8.3 短路径）
func IsTempDir(p string) bool {
	norm := func(s string) string {
		if r, err := filepath.EvalSymlinks(s); err == nil {
			s = r
		}
		if a, err := filepath.Abs(s); err == nil {
			s = a
		}
		return strings.ToLower(filepath.Clean(s)) + string(filepath.Separator)
	}
	np := norm(p)
	if strings.HasPrefix(np, norm(os.TempDir())) {
		return true
	}
	if runtime.GOOS == "windows" {
		return strings.Contains(np, `\appdata\local\temp\`)
	}
	return false
}

// writable 目录是否可写
func writable(dir string) bool {
	f, err := os.CreateTemp(dir, ".w")
	if err != nil {
		return false
	}
	f.Close()
	os.Remove(f.Name())
	return true
}
