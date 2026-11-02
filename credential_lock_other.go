//go:build !darwin && !linux && !freebsd && !openbsd && !netbsd && !dragonfly

package main

import "errors"

func (s *sourceCredentialStore) Lock() (func(), error) {
	return nil, errors.New("credential store locking unsupported")
}
