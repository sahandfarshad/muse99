//go:build windows

package main

import (
	"os"
	"os/exec"
	"strings"
)

func canPickFolder() bool { return true }

// pickFolder shows the Windows folder dialog on top of other windows and
// returns the chosen folder, or "" when the user cancels.
func pickFolder(start string) (string, error) {
	ps := `[Console]::OutputEncoding=[Text.Encoding]::UTF8
Add-Type -AssemblyName System.Windows.Forms
$f=New-Object System.Windows.Forms.FolderBrowserDialog
$f.Description='Muse99 work folder'
$f.ShowNewFolderButton=$true
$f.SelectedPath=$env:MUSE_PICK_START
$o=New-Object System.Windows.Forms.Form
$o.TopMost=$true
if($f.ShowDialog($o) -eq [System.Windows.Forms.DialogResult]::OK){[Console]::Out.Write($f.SelectedPath)}`
	cmd := exec.Command("powershell.exe", "-NoProfile", "-STA", "-NonInteractive", "-Command", ps)
	cmd.Env = append(os.Environ(), "MUSE_PICK_START="+start)
	hideWindow(cmd)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
