//go:build !windows

package main

import (
	"os/exec"
	"runtime"
)

func openBrowserURL(url string) error {
	command := "xdg-open"
	if runtime.GOOS == "darwin" {
		command = "open"
	}
	return exec.Command(command, url).Start()
}
