//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// openAppWindow opens url in its own app window (Microsoft Edge, which
// every Windows 10/11 has, or Chrome) with a private profile in
// profileDir. When wait is true it returns after the window closes
// (waited = true). Without Edge/Chrome it falls back to the default
// browser and returns at once (waited = false).
func openAppWindow(url, profileDir string, wait bool) (bool, error) {
	if bin := findAppBrowser(); bin != "" {
		_ = os.MkdirAll(profileDir, 0o700)
		cmd := exec.Command(bin, "--app="+url, "--user-data-dir="+profileDir,
			"--no-first-run", "--no-default-browser-check", "--window-size=1280,860")
		if err := cmd.Start(); err == nil {
			if !wait {
				go func() { _ = cmd.Wait() }()
				return false, nil
			}
			_ = cmd.Wait()
			return true, nil
		}
	}
	cmd := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	if err := cmd.Start(); err != nil {
		return false, fmt.Errorf("no browser found to open %s: %w", url, err)
	}
	go func() { _ = cmd.Wait() }()
	return false, nil
}

func findAppBrowser() string {
	cands := []string{}
	for _, env := range []string{"ProgramFiles(x86)", "ProgramFiles", "LocalAppData"} {
		if d := os.Getenv(env); d != "" {
			cands = append(cands,
				filepath.Join(d, "Microsoft", "Edge", "Application", "msedge.exe"),
				filepath.Join(d, "Google", "Chrome", "Application", "chrome.exe"))
		}
	}
	for _, c := range cands {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	if p, err := exec.LookPath("msedge.exe"); err == nil {
		return p
	}
	return ""
}
