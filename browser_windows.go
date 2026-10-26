//go:build windows

package main

import (
	"fmt"
	"syscall"
	"unsafe"
)

// ShellExecuteW avoids putting the one-time bootstrap URL in a child process's
// command line. Windows passes the URL directly to the registered browser.
func openBrowserURL(url string) error {
	verb, err := syscall.UTF16PtrFromString("open")
	if err != nil {
		return err
	}
	target, err := syscall.UTF16PtrFromString(url)
	if err != nil {
		return err
	}
	proc := syscall.NewLazyDLL("shell32.dll").NewProc("ShellExecuteW")
	result, _, callErr := proc.Call(0, uintptr(unsafe.Pointer(verb)), uintptr(unsafe.Pointer(target)), 0, 0, 1)
	if result <= 32 {
		return fmt.Errorf("open browser failed: %w", callErr)
	}
	return nil
}
