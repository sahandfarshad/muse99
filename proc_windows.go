//go:build windows

package main

import (
	"context"
	"os/exec"
	"strconv"
	"syscall"
)

const (
	createNewProcessGroup = 0x00000200
	detachedProcess       = 0x00000008
	createNoWindow        = 0x08000000
)

// shellName is the shell the run_shell tool uses on this system.
func shellName() string { return "PowerShell" }

// backgroundHint tells the model how to keep a long job running.
func backgroundHint() string { return "Start-Process ... -WindowStyle Hidden" }

// shellCommand builds the run_shell command on Windows: PowerShell with
// UTF-8 output and no console window. A timeout or Stop kills the whole
// process tree with taskkill /T.
func shellCommand(ctx context.Context, command string) *exec.Cmd {
	script := "[Console]::OutputEncoding=[System.Text.Encoding]::UTF8;$OutputEncoding=[System.Text.Encoding]::UTF8;" + command
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNewProcessGroup | createNoWindow}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			k := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
			hideWindow(k)
			_ = k.Run()
			_ = cmd.Process.Kill()
		}
		return nil
	}
	return cmd
}

// detachedAttr starts a child that keeps running on its own.
func detachedAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{HideWindow: true, CreationFlags: detachedProcess | createNewProcessGroup}
}

// hideWindow stops a helper command from flashing a console window.
func hideWindow(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}
