package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Update / roll back (SSH menu option 8).
//
// Every update keeps the binary it replaces next to it as
// <binary>.prev (plus <binary>.prev.version with its version), so
// "Roll back" can swap back in one step. Rolling back swaps the two,
// so rolling back again returns to the newer version. Any release can
// also be installed by tag.

const releaseRepo = "sahandfarshad/muse99"

// releaseURL is the download URL for a tag ("" = latest).
func releaseURL(tag, arch string) string {
	if tag == "" || tag == "latest" {
		return "https://github.com/" + releaseRepo + "/releases/latest/download/muse99-linux-" + arch
	}
	return "https://github.com/" + releaseRepo + "/releases/download/" + tag + "/muse99-linux-" + arch
}

// normalizeTag accepts "0.7.2" or "v0.7.2".
func normalizeTag(t string) string {
	t = strings.TrimSpace(t)
	if t == "" || t == "latest" {
		return ""
	}
	if !strings.HasPrefix(t, "v") {
		t = "v" + t
	}
	return t
}

func currentExe() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

func prevPath(exe string) string { return exe + ".prev" }

// prevVersion is the version of the kept previous binary, or "".
func prevVersion(exe string) string {
	if _, err := os.Stat(prevPath(exe)); err != nil {
		return ""
	}
	if b, err := os.ReadFile(prevPath(exe) + ".version"); err == nil {
		if v := strings.TrimSpace(string(b)); v != "" {
			return v
		}
	}
	if v := binaryVersion(prevPath(exe)); v != "" {
		return v
	}
	return "older version"
}

// binaryVersion runs `<bin> --version` (v0.8+) and returns the tag,
// or "" for binaries that do not support it.
func binaryVersion(bin string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	if err != nil {
		return ""
	}
	f := strings.Fields(strings.TrimSpace(string(out)))
	if len(f) == 2 && f[0] == "muse99" {
		return f[1]
	}
	return ""
}

// installRelease downloads a release (tag "" = latest), checks that it
// runs, keeps the current binary as .prev, and swaps the new one in.
func installRelease(tag string) error {
	arch := runtime.GOARCH
	if arch != "amd64" && arch != "arm64" {
		return fmt.Errorf("unsupported architecture %s (only amd64/arm64 releases exist)", arch)
	}
	exe, err := currentExe()
	if err != nil {
		return err
	}
	url := releaseURL(tag, arch)
	fmt.Println("Downloading", url)
	// Download next to the binary, so the final rename stays on one
	// filesystem (/tmp is often a separate tmpfs).
	tmpPath := exe + ".new"
	_ = os.Remove(tmpPath)
	defer os.Remove(tmpPath)

	downloaded := false
	if _, err := exec.LookPath("curl"); err == nil {
		cmd := exec.Command("curl", "-fsSL", "-o", tmpPath, url)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err == nil {
			downloaded = true
		} else {
			fmt.Println("curl failed, trying a direct download…")
		}
	}
	if !downloaded {
		if err := downloadFile(url, tmpPath); err != nil {
			if tag != "" {
				return fmt.Errorf("download failed — is %s a real release? %w", tag, err)
			}
			return fmt.Errorf("download failed: %w", err)
		}
	}
	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return err
	}
	// Verify the new binary actually runs before replacing anything.
	if out, err := exec.Command(tmpPath, "--help").CombinedOutput(); err != nil {
		if !strings.Contains(string(out), "muse99") && !strings.Contains(string(out), "Usage") {
			return fmt.Errorf("downloaded binary did not pass --help check: %v (%s)", err, strings.TrimSpace(string(out)))
		}
	}
	newVer := binaryVersion(tmpPath)
	if newVer != "" && newVer == Version && Version != "dev" {
		fmt.Println("You already have", newVer, "— nothing to change.")
		return errNoChange
	}
	// Keep the current binary for roll back.
	if err := os.Rename(exe, prevPath(exe)); err != nil {
		return fmt.Errorf("could not keep the current version as %s (write permission?): %w", prevPath(exe), err)
	}
	_ = os.WriteFile(prevPath(exe)+".version", []byte(versionLabel()+"\n"), 0o644)
	if err := os.Rename(tmpPath, exe); err != nil {
		_ = os.Rename(prevPath(exe), exe) // put the old one back
		return fmt.Errorf("could not replace %s: %w", exe, err)
	}
	if newVer == "" {
		newVer = "new version"
	}
	fmt.Printf("Updated %s: %s → %s (previous kept for roll back)\n", exe, versionLabel(), newVer)
	return nil
}

var errNoChange = fmt.Errorf("no change")

// rollbackRelease swaps the current binary with the kept previous one.
func rollbackRelease() error {
	exe, err := currentExe()
	if err != nil {
		return err
	}
	prev := prevPath(exe)
	if _, err := os.Stat(prev); err != nil {
		return fmt.Errorf("no previous version kept yet — it is kept from the next update on (or install one by tag)")
	}
	pv := prevVersion(exe)
	swap := exe + ".swap"
	_ = os.Remove(swap)
	if err := os.Rename(exe, swap); err != nil {
		return fmt.Errorf("could not move %s (write permission?): %w", exe, err)
	}
	if err := os.Rename(prev, exe); err != nil {
		_ = os.Rename(swap, exe)
		return err
	}
	if err := os.Rename(swap, prev); err != nil {
		return err
	}
	_ = os.WriteFile(prev+".version", []byte(versionLabel()+"\n"), 0o644)
	fmt.Printf("Rolled back: %s → %s. Roll back again to return to %s.\n", versionLabel(), pv, versionLabel())
	return nil
}

// listReleases returns recent release tags, newest first.
func listReleases(n int) ([]string, error) {
	req, _ := http.NewRequest("GET", fmt.Sprintf("https://api.github.com/repos/%s/releases?per_page=%d", releaseRepo, n), nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return nil, fmt.Errorf("GitHub said %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var rel []struct {
		TagName    string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	tags := []string{}
	for _, r := range rel {
		if !r.Draft && r.TagName != "" {
			tags = append(tags, r.TagName)
		}
	}
	return tags, nil
}

// versionsMenu is SSH menu option 8. It returns true when the binary
// changed, so the caller restarts the web UI on the new one.
func versionsMenu(scanner *bufio.Scanner) bool {
	exe, _ := currentExe()
	tags, lerr := listReleases(10)
	latest := ""
	if len(tags) > 0 {
		latest = tags[0]
	}
	fmt.Println("Update / roll back — you are on", versionLabel())
	if latest != "" {
		if latest == Version {
			fmt.Println("  You have the latest version.")
		} else {
			fmt.Println("  Latest release:", latest)
		}
	}
	pv := ""
	if exe != "" {
		pv = prevVersion(exe)
	}
	fmt.Println()
	fmt.Println("  1  Update to the latest version")
	if pv != "" {
		fmt.Println("  2  Roll back to the previous version (" + pv + ")")
	} else {
		fmt.Println("  2  Roll back to the previous version (none kept yet)")
	}
	fmt.Println("  3  Install a specific version")
	fmt.Println("  0  Back")
	fmt.Print("\n  Choose [0-3]: ")
	if !scanner.Scan() {
		return false
	}
	var err error
	switch strings.TrimSpace(scanner.Text()) {
	case "1":
		err = installRelease("")
	case "2":
		err = rollbackRelease()
	case "3":
		if lerr != nil {
			fmt.Println("Could not list releases:", lerr)
		} else if len(tags) > 0 {
			fmt.Println("Releases:", strings.Join(tags, "  "))
		}
		t, ok := prompt(scanner, "Version to install (e.g. v0.7.2, 0 to cancel)", "")
		if !ok || strings.TrimSpace(t) == "" {
			fmt.Println("Cancelled — nothing changed.")
			return false
		}
		err = installRelease(normalizeTag(t))
	default:
		return false
	}
	if err == errNoChange {
		return false
	}
	if err != nil {
		fmt.Println("Failed:", err)
		fmt.Println("You can also use the installer (any version with --version vX.Y.Z):")
		fmt.Println("  curl -fsSL https://raw.githubusercontent.com/" + releaseRepo + "/main/install.sh | bash")
		return false
	}
	return true
}

// askUserName asks "What should I call you?" and saves the answer.
// firstRun allows skipping with Enter (asked again next time).
func askUserName(cfg *Config, scanner *bufio.Scanner, firstRun bool) bool {
	if firstRun {
		fmt.Println("Welcome to Muse99! What should I call you?")
		fmt.Println("(You can change it later with option 17 or in the web UI Settings.)")
	}
	def := ""
	if cfg.NameSet {
		def = cfg.UserName
	}
	label := "Your name"
	if !firstRun {
		label = "Your name (0 to cancel)"
	}
	n, ok := prompt(scanner, label, def)
	n = cleanUserName(n)
	if !ok || n == "" || (cfg.NameSet && n == cfg.UserName) {
		if !firstRun {
			fmt.Println("Nothing changed.")
		}
		return false
	}
	if err := setUserName(cfg, n); err != nil {
		fmt.Println("Could not save your name:", err)
		return false
	}
	fmt.Printf("Nice to meet you, %s!\n", n)
	return true
}

// restartWebAfterUpdate restarts the web UI on the new binary, so the
// new version is the one actually serving.
func restartWebAfterUpdate(cfg *Config, ctrl *webController, system string) {
	if ctrl.IsRunning() {
		if err := ctrl.Restart(*cfg, system); err != nil {
			fmt.Println("Web UI restart failed:", err)
		} else {
			fmt.Println("Web UI restarted with the new version.")
		}
	} else {
		handOverDB()
		if err := restartDetachedWeb(*cfg); err != nil {
			fmt.Println("Web UI restart failed:", err)
			if appDB == nil {
				_, _ = openAppDB()
			}
		} else {
			fmt.Println("Web UI restarted with the new version.")
		}
	}
	fmt.Println("Note: this menu is still the old version — quit (0) and run muse99 again for the new menu.")
}
