//go:build !linux

package pppoe

import "syscall"

func setSysProcAttr(cmd *syscall.SysProcAttr) {
	// Windows 不支持 Setpgid
}

func killProcess(pid int, sig syscall.Signal) error {
	// Windows 不支持通过 PID 发送信号
	// 实际部署在 Linux 上，这里只是占位
	return nil
}
