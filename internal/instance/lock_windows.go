//go:build windows

package instance

import (
	"os"
	"syscall"
)

// AcquireLock 以“不共享”方式打开锁文件：同一时间只有一个进程能打开成功。
// 进程退出（包括直接关掉黑色窗口、任务管理器结束）时系统自动释放
func AcquireLock(path string) (*os.File, bool) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, false
	}
	h, err := syscall.CreateFile(p, syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		0, // share mode = 0：独占
		nil, syscall.OPEN_ALWAYS, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, false
	}
	return os.NewFile(uintptr(h), path), true
}
