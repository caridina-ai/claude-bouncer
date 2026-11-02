//go:build !windows

package main

import (
	"errors"
	"syscall"
)

// processAlive reports whether the process with the given ID is still running.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
