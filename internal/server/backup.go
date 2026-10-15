package server

import (
	"archive/zip"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/git-ljm/kefu-ai/internal/etl"
	"github.com/git-ljm/kefu-ai/internal/store"
)

// AutoBackupOnUpgrade 程序版本变化时（升级后第一次启动），先给数据库做一份快照，最多保留 5 份
func AutoBackupOnUpgrade(s *store.Store, dataDir, version string) {
	last := s.Settings()["app_version"]
	if last == version {
		return
	}
	if s.HasData() && last != "" {
		dir := filepath.Join(dataDir, "backups")
		_ = os.MkdirAll(dir, 0o755)
		p := filepath.Join(dir, fmt.Sprintf("kefu-%s-before-%s.db", time.Now().Format("20060102-150405"), safeName(version)))
		if err := s.Backup(p); err != nil {
			log.Printf("升级前自动备份失败: %v", err)
		} else {
			log.Printf("检测到版本升级 %s → %s，已自动备份到 %s", last, version, p)
		}
		pruneBackups(dir, 5)
	}
	_ = s.SaveSettings(map[string]string{"app_version": version})
}

func pruneBackups(dir string, keep int) {
	es, _ := os.ReadDir(dir)
	var names []string
	for _, e := range es {
		if strings.HasSuffix(e.Name(), ".db") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names) // 文件名以时间开头，按名字排序即按时间
	for i := 0; i < len(names)-keep; i++ {
		_ = os.Remove(filepath.Join(dir, names[i]))
	}
}

// backupFull 完整备份：数据库快照 + 所有资料/图片/视频，打成 zip 边压缩边下载
func (a *App) backupFull(w http.ResponseWriter, r *http.Request) {
	tmp := filepath.Join(os.TempDir(), fmt.Sprintf("kefu-backup-%d.db", time.Now().UnixNano()))
	defer os.Remove(tmp)
	if err := a.store.Backup(tmp); err != nil {
		writeErr(w, 500, "备份失败："+err.Error())
		return
	}
	name := "kefu-full-backup-" + time.Now().Format("20060102-1504") + ".zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	zw := zip.NewWriter(w)
	defer zw.Close()

	add := func(src, dst string) error {
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		defer f.Close()
		method := zip.Deflate
		if etl.MediaTypeOf(src) != "" { // 图片视频本身已压缩，直接存储更快
			method = zip.Store
		}
		zf, err := zw.CreateHeader(&zip.FileHeader{Name: dst, Method: method, Modified: time.Now()})
		if err != nil {
			return err
		}
		_, err = io.Copy(zf, f)
		return err
	}
	if err := add(tmp, "kefu-data/kefu.db"); err != nil {
		log.Printf("完整备份失败: %v", err)
		return
	}
	root := filepath.Join(a.dataDir, "files")
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(a.dataDir, p)
		if err := add(p, "kefu-data/"+filepath.ToSlash(rel)); err != nil {
			log.Printf("备份文件失败 %s: %v", p, err)
		}
		return nil
	})
}

// openDataDir 在文件管理器中打开数据目录
func (a *App) openDataDir(w http.ResponseWriter, r *http.Request) {
	if err := revealFile(filepath.Join(a.dataDir, "kefu.db")); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
