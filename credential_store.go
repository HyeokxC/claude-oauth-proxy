package main

import (
	"bytes"
	"errors"
	"path/filepath"
)

var (
	errSourceChanged    = errors.New("credential source changed")
	errStoreWrite       = errors.New("credential store write failed")
	errKeychainNotFound = errors.New("keychain item not found")
)

type credentialRecord struct {
	path     string
	filePath string // original lookup path; path is its canonical target
	service  string
	account  string
	data     []byte
	auth     Credentials
}

func sameCredentialSource(a, b *credentialRecord) bool {
	return a.path == b.path && a.filePath == b.filePath && a.service == b.service && a.account == b.account
}

type credentialStore interface {
	Lock() (func(), error)
	Load(source *credentialRecord) (*credentialRecord, error)
	Save(expected *credentialRecord, next Credentials) (*credentialRecord, error)
}

type keychainWriter func(service, account string, expected, next []byte) error

type sourceCredentialStore struct {
	nativeWrite   keychainWriter
	syncDirectory func(string) error
}

func recordWithData(source credentialRecord, data []byte) (*credentialRecord, error) {
	if len(data) > maxCredentialDocument {
		return nil, errCredentialDocument
	}
	doc, err := decodeCredentialDocument(data)
	if err != nil {
		return nil, err
	}
	source.data, source.auth = data, doc.auth
	return &source, nil
}

// Re-evaluate Keychain-first selection on every lifecycle reload. A pending
// record retains its original destination independently of this active source.
func (s *sourceCredentialStore) Load(_ *credentialRecord) (*credentialRecord, error) {
	service, account, err := keychainIdentity()
	if err == nil {
		data, readErr := readKeychainData(service, account)
		if readErr == nil {
			record, parseErr := recordWithData(credentialRecord{service: service, account: account}, data)
			if parseErr == nil {
				return record, nil
			}
		}
	}
	dir, _, err := effectiveConfigDir()
	if err != nil {
		return nil, errors.New("credential directory unavailable")
	}
	return loadCredentialFile(credentialRecord{path: filepath.Join(dir, ".credentials.json")})
}

// The caller holds the proxy advisory lock across load, refresh and save.
// This is NOT atomic CAS against a noncooperating external CLI: it can write
// between our last comparison and rename/native update. No CLI lock is used.
func (s *sourceCredentialStore) Save(expected *credentialRecord, next Credentials) (*credentialRecord, error) {
	current, err := s.Load(nil)
	if err != nil {
		return nil, errStoreWrite
	}
	if current.auth == next {
		if !sameCredentialSource(current, expected) {
			return current, errSourceChanged
		}
		// A previous rename may have succeeded before directory sync failed.
		// Retry durability instead of overwriting or refreshing the rotation.
		if current.path != "" {
			if err := s.syncFileDirectory(current.path); err != nil {
				return nil, errStoreWrite
			}
		}
		return current, nil
	}
	if current.auth != expected.auth {
		return current, errSourceChanged
	}
	if !sameCredentialSource(current, expected) {
		// Identical stale auth in a different source is not a newer login.
		// Retain the pending rotation and original destination without writing
		// to an inactive file or downgrading a failed Keychain write.
		return nil, errStoreWrite
	}
	doc, err := decodeCredentialDocument(current.data)
	if err != nil {
		return nil, errStoreWrite
	}
	data, err := doc.rotated(next)
	if err != nil || len(data) > maxCredentialDocument {
		return nil, errStoreWrite
	}
	if current.path != "" {
		err = s.replaceCredentialFile(current, data)
	} else {
		if !s.sourceStillCurrent(current) {
			err = errStoreWrite
		} else {
			err = s.nativeWrite(current.service, current.account, current.data, data)
		}
	}
	if err != nil {
		// A native comparison or the final file comparison may observe a login.
		latest, readErr := s.Load(nil)
		if readErr == nil && latest.auth != expected.auth && (latest.auth != next || !sameCredentialSource(latest, expected)) {
			return latest, errSourceChanged
		}
		return nil, errStoreWrite
	}
	return recordWithData(*current, data)
}

func (s *sourceCredentialStore) sourceStillCurrent(expected *credentialRecord) bool {
	latest, err := s.Load(nil)
	return err == nil && sameCredentialSource(latest, expected) && bytes.Equal(latest.data, expected.data)
}
