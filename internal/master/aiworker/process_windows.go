//go:build windows

package aiworker

import (
	"os/exec"
	"strconv"
	"syscall"
)

func configureProcess(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	c.Cancel = func() error { killProcess(c); return nil }
}
func killProcess(c *exec.Cmd) {
	if c.Process == nil {
		return
	}
	cmd := exec.Command("taskkill", "/PID", strconv.Itoa(c.Process.Pid), "/T", "/F")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	_ = cmd.Run()
}
