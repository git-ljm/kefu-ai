package etl

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// FindTool 查找外部命令（如 ffmpeg）：优先程序同目录，其次系统 PATH
func FindTool(name string) string {
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	p, _ := exec.LookPath(name)
	return p
}

// HasFFmpeg 电脑上是否有 ffmpeg + ffprobe
func HasFFmpeg() bool { return FindTool("ffmpeg") != "" && FindTool("ffprobe") != "" }

// VideoPoster 用 ffmpeg 从视频里均匀截 4 帧拼成 2x2 的图，返回图片路径（video + ".poster.jpg"）。
// 只在浏览器截不到画面时作为备用方案。
func VideoPoster(ctx context.Context, video string) (string, error) {
	ff, probe := FindTool("ffmpeg"), FindTool("ffprobe")
	if ff == "" || probe == "" {
		return "", fmt.Errorf("未安装 ffmpeg")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, probe, "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", video).Output()
	if err != nil {
		return "", err
	}
	dur, _ := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if dur <= 0 {
		dur = 10
	}
	poster := video + ".poster.jpg"
	cmd := exec.CommandContext(ctx, ff, "-y", "-v", "error", "-i", video,
		"-vf", fmt.Sprintf("fps=%.5f,scale=480:-2,tile=2x2", 4.0/dur), "-frames:v", "1", poster)
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return poster, nil
}
