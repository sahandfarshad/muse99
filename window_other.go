//go:build !windows

package main

import (
	"os/exec"
	"runtime"
)

// openAppWindow opens url in the default browser (the desktop app is
// built for Windows; this keeps --desktop usable elsewhere too).
func openAppWindow(url, profileDir string, wait bool) (bool, error) {
	bin := "xdg-open"
	if runtime.GOOS == "darwin" {
		bin = "open"
	}
	cmd := exec.Command(bin, url)
	if err := cmd.Start(); err != nil {
		return false, err
	}
	go func() { _ = cmd.Wait() }()
	return false, nil
}
