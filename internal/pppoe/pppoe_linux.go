//go:build linux

package pppoe

import "syscall"

func setSysProcAttr(cmd *syscall.SysProcAttr) {
	cmd.Setpgid = true
	// 兜底：主进程异常退出（未走 Shutdown）时由内核直接结束 pppd，
	// 避免残留进程继续占用 BRAS 会话导致下次拨号失败。
	cmd.Pdeathsig = syscall.SIGKILL
}

func killProcess(pid int, sig syscall.Signal) error {
	return syscall.Kill(-pid, sig)
}
