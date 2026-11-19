package main

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Uninstall (v5) — removes the Muse99 binary and service files, and
// optionally the user data in ~/.muse99. Data is KEPT by default: the
// user must explicitly say yes (menu prompt, or --yes on the CLI).
//
// The running binary can be deleted on Linux (the file is unlinked
// while the process keeps running), so uninstall works even when
// invoked from the binary itself.

// uninstallTarget is everything uninstall needs to know. The default
// target is derived from the real environment; tests pass temp dirs.
type uninstallTarget struct {
	ExePath    string // the running muse99 binary
	InstallDir string // dir holding the binary (removed only if empty)
	DataDir    string // ~/.muse99 — config, DB, usage, logs
	HomeDir    string
}

func defaultUninstallTarget() uninstallTarget {
	exe, err := os.Executable()
	if err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
	}
	home, _ := os.UserHomeDir()
	return uninstallTarget{
		ExePath:    exe,
		InstallDir: filepath.Dir(exe),
		DataDir:    configDir(),
		HomeDir:    home,
	}
}

// serviceFiles returns the systemd unit files Muse99 may have installed.
func (t uninstallTarget) serviceFiles() []string {
	return []string{
		filepath.Join(t.HomeDir, ".config", "systemd", "user", "muse99.service"),
		"/etc/systemd/system/muse99.service",
		filepath.Join(t.DataDir, "muse99.service"),
	}
}

// uninstallPlan lists what will be removed and what will be kept.
type uninstallPlan struct {
	Remove []string
	Keep   []string
}

func planUninstall(t uninstallTarget, removeData bool) uninstallPlan {
	p := uninstallPlan{}
	if t.ExePath != "" {
		if _, err := os.Stat(t.ExePath); err == nil {
			p.Remove = append(p.Remove, "binary: "+t.ExePath)
		}
	}
	for _, s := range t.serviceFiles() {
		if _, err := os.Stat(s); err == nil {
			p.Remove = append(p.Remove, "service: "+s)
		}
	}
	// Runtime files (PID / log) are always removed — they are not data.
	for _, f := range []string{webPIDPath(), webLogPath()} {
		if _, err := os.Stat(f); err == nil {
			p.Remove = append(p.Remove, "runtime file: "+f)
		}
	}
	if removeData {
		if _, err := os.Stat(t.DataDir); err == nil {
			p.Remove = append(p.Remove, "data: "+t.DataDir+" (config, chats, memory, usage)")
		}
	} else {
		if _, err := os.Stat(t.DataDir); err == nil {
			p.Keep = append(p.Keep, "data kept: "+t.DataDir+" (config, chats, memory, usage)")
		}
	}
	return p
}

// runUninstall performs the uninstall and returns what was removed / kept.
func runUninstall(t uninstallTarget, removeData bool) (removed, kept []string, err error) {
	// Stop a web UI started by the installer / another process first.
	_ = stopExternalWeb()

	if t.ExePath != "" {
		if rmErr := os.Remove(t.ExePath); rmErr == nil {
			removed = append(removed, "binary: "+t.ExePath)
		} else if !os.IsNotExist(rmErr) {
			return removed, kept, fmt.Errorf("could not remove %s: %w", t.ExePath, rmErr)
		}
	}
	for _, s := range t.serviceFiles() {
		if rmErr := os.Remove(s); rmErr == nil {
			removed = append(removed, "service: "+s)
			// Tell systemd to forget the unit (best effort, no sudo here).
			_ = reloadSystemd(s)
		} else if !os.IsNotExist(rmErr) {
			kept = append(kept, fmt.Sprintf("service NOT removed (no permission?): %s — remove it with sudo if needed", s))
		}
	}
	// Runtime files go either way.
	for _, f := range []string{webPIDPath(), webLogPath()} {
		if rmErr := os.Remove(f); rmErr == nil {
			removed = append(removed, "runtime file: "+f)
		}
	}
	if removeData {
		if rmErr := os.RemoveAll(t.DataDir); rmErr == nil {
			removed = append(removed, "data: "+t.DataDir)
		} else if !os.IsNotExist(rmErr) {
			return removed, kept, fmt.Errorf("could not remove %s: %w", t.DataDir, rmErr)
		}
	} else if _, statErr := os.Stat(t.DataDir); statErr == nil {
		kept = append(kept, "data kept: "+t.DataDir)
	}
	// Remove the install dir only if it is now empty (never delete a
	// shared bin dir like ~/.local/bin that holds other tools).
	if t.InstallDir != "" {
		if rmErr := os.Remove(t.InstallDir); rmErr == nil {
			removed = append(removed, "empty install dir: "+t.InstallDir)
		}
	}
	return removed, kept, nil
}

// reloadSystemd asks systemd to drop a removed unit. User units do not
// need sudo; system units do, so a failure there is silently ignored.
func reloadSystemd(servicePath string) error {
	if strings.HasPrefix(servicePath, "/etc/") {
		return fmt.Errorf("system unit needs sudo")
	}
	return runCmd("systemctl", "--user", "daemon-reload")
}

// stopExternalWeb stops a web UI running in another process (e.g. the
// one the installer started). It reads ~/.muse99/web.pid and, if that
// is missing, tries the HTTP status endpoint on the default port.
//
// v9: when the PID file is missing it also looks for a muse99 web
// process by its command line (Linux /proc scan). Without this, a
// web UI started from another install folder — or whose PID file was
// lost — could not be stopped at all: Restart/Update would start a
// new process that cannot bind, while the old one keeps answering,
// and the caller would report success.
func stopExternalWeb() error {
	if pid := readWebPID(); pid > 0 {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Signal(syscall.SIGTERM)
			for i := 0; i < 20; i++ {
				time.Sleep(100 * time.Millisecond)
				if !processAlive(pid) {
					break
				}
			}
			if processAlive(pid) {
				_ = p.Signal(syscall.SIGKILL)
			}
		}
		_ = os.Remove(webPIDPath())
		return nil
	}
	for _, pid := range findMuse99WebPIDs() {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Signal(syscall.SIGTERM)
		}
	}
	for i := 0; i < 20; i++ {
		time.Sleep(100 * time.Millisecond)
		anyLeft := false
		for _, pid := range findMuse99WebPIDs() {
			if processAlive(pid) {
				anyLeft = true
			}
		}
		if !anyLeft {
			break
		}
	}
	for _, pid := range findMuse99WebPIDs() {
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Signal(syscall.SIGKILL)
		}
	}
	return nil
}

// findMuse99WebPIDs returns the PIDs of muse99 processes running a
// web UI, by scanning /proc (Linux only). It never returns this
// process itself. Used only when the PID file is missing.
func findMuse99WebPIDs() []int {
	if runtime.GOOS != "linux" {
		return nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 || pid == os.Getpid() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		args := strings.Split(string(raw), "\x00")
		isMuse99, isWeb := false, false
		for _, a := range args {
			if strings.Contains(a, "muse99") {
				isMuse99 = true
			}
			if a == "--web" {
				isWeb = true
			}
		}
		if isMuse99 && isWeb {
			out = append(out, pid)
		}
	}
	return out
}

func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 checks existence without killing. On Unix a zombie
	// still answers, which is good enough here (we only wait briefly).
	return p.Signal(syscall.Signal(0)) == nil
}

func readWebPID() int {
	data, err := os.ReadFile(webPIDPath())
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// confirmUninstall asks the user to type YES. Returns false on
// anything else (including EOF / empty input).
func confirmUninstall(scanner *bufio.Scanner, what string) bool {
	fmt.Printf("Type YES to %s: ", what)
	if !scanner.Scan() {
		return false
	}
	return strings.TrimSpace(scanner.Text()) == "YES"
}

// runUninstallCLI is the `muse99 --uninstall` / `muse99 uninstall`
// path. With --yes it is fully non-interactive and removes data too;
// without it, it asks first and keeps data by default.
func runUninstallCLI(yes bool) error {
	t := defaultUninstallTarget()
	if !yes {
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		plan := planUninstall(t, false)
		fmt.Println("Uninstall Muse99 — this will remove:")
		for _, r := range plan.Remove {
			fmt.Println("  -", r)
		}
		for _, k := range plan.Keep {
			fmt.Println("  (kept)", k)
		}
		if !confirmUninstall(scanner, "uninstall Muse99") {
			fmt.Println("Uninstall cancelled — nothing was removed.")
			return nil
		}
		removeData := false
		fmt.Print("Also delete your Muse99 data (~/.muse99: config, chats, memory, usage)? [y/N]: ")
		if scanner.Scan() {
			a := strings.ToLower(strings.TrimSpace(scanner.Text()))
			removeData = a == "y" || a == "yes"
		}
		removed, kept, err := runUninstall(t, removeData)
		if err != nil {
			return err
		}
		printUninstallResult(removed, kept)
		return nil
	}
	removed, kept, err := runUninstall(t, true)
	if err != nil {
		return err
	}
	printUninstallResult(removed, kept)
	return nil
}

func printUninstallResult(removed, kept []string) {
	fmt.Println("Muse99 uninstalled.")
	if len(removed) > 0 {
		fmt.Println("Removed:")
		for _, r := range removed {
			fmt.Println("  -", r)
		}
	}
	if len(kept) > 0 {
		fmt.Println("Kept:")
		for _, k := range kept {
			fmt.Println("  -", k)
		}
	}
	fmt.Println("To install again, run the one-line installer:")
	fmt.Println("  curl -fsSL https://raw.githubusercontent.com/sahandfarshad/muse99/main/install.sh | bash")
}

// runCmd runs a command with output discarded (used for best-effort
// systemd reloads). It lives here so uninstall stays self-contained.
func runCmd(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdout = nil
	cmd.Stderr = nil
	return cmd.Run()
}
