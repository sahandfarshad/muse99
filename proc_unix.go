//go:build !windows

package main

import (
	"context"
	"os/exec"
	"syscall"
)

// shellName is the shell the run_shell tool uses on this system.
func shellName() string { return "bash" }

// backgroundHint tells the model how to keep a long job running.
func backgroundHint() string { return "nohup ... &" }

// shellCommand builds the run_shell command: bash in its own process
// group, so a timeout or Stop kills the whole tree, not just bash.
func shellCommand(ctx context.Context, command string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	return cmd
}

// detachedAttr starts a child in its own session (survives the menu).
func detachedAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

// hideWindow is a no-op outside Windows.
func hideWindow(cmd *exec.Cmd) {}
