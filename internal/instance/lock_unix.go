//go:build !windows

package instance

import (
	"os"
	"syscall"
)

// AcquireLock 独占锁，进程退出（包括被强制关闭）时系统自动释放
func AcquireLock(path string) (*os.File, bool) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, false
	}
	return f, true
}
