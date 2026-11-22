//go:build !windows

package main

import "errors"

func canPickFolder() bool { return false }

func pickFolder(start string) (string, error) {
	return "", errors.New("type the folder path instead")
}
