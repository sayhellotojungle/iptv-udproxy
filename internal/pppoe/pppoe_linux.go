//go:build linux

package pppoe

import "syscall"

func setSysProcAttr(cmd *syscall.SysProcAttr) {
	cmd.Setpgid = true
}

func killProcess(pid int, sig syscall.Signal) error {
	return syscall.Kill(-pid, sig)
}
