//go:build darwin || linux || freebsd || openbsd || netbsd || dragonfly

package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

func (s *sourceCredentialStore) Lock() (func(), error) {
	dir, _, err := effectiveConfigDir()
	if err != nil {
		return nil, errors.New("credential lock unavailable")
	}
	// This proxy-owned file is never removed: replacing it would split locks.
	fd, err := syscall.Open(filepath.Join(dir, ".cc-proxy-credentials.lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, errors.New("credential lock unavailable")
	}
	file := os.NewFile(uintptr(fd), "proxy credential lock")
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, errors.New("credential store busy")
	}
	return func() { file.Close() }, nil // closing releases the advisory lock
}
