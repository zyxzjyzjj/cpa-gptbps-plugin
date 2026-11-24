package auth

import (
	"context"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

func prepareBrowserProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000200} // CREATE_NEW_PROCESS_GROUP
}

func stopBrowserProcess(cmd *exec.Cmd, done <-chan struct{}) {
	select {
	case <-done:
		return
	case <-time.After(250 * time.Millisecond):
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, "taskkill", "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F").Run()
	_ = cmd.Process.Kill()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
}
