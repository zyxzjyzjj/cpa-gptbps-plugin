//go:build !windows

package auth

import (
	"os/exec"
	"syscall"
	"time"
)

func prepareBrowserProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func stopBrowserProcess(cmd *exec.Cmd, done <-chan struct{}) {
	select {
	case <-done:
	case <-time.After(250 * time.Millisecond):
	}
	// The process group belongs to this temporary profile, not the user's
	// existing Chrome. Terminate its child renderers along with the parent.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
}
