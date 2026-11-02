//go:build !darwin || !cgo

package main

import "errors"

func nativeKeychainUpdate(service, account string, expected, next []byte) error {
	return errors.New("native keychain updates unavailable")
}
