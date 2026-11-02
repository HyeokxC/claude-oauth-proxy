package main

import (
	"os"
	"path/filepath"
)

func loadCredentialFile(source credentialRecord) (*credentialRecord, error) {
	lookup := source.filePath
	if lookup == "" {
		lookup = source.path
	}
	target, err := filepath.EvalSymlinks(lookup)
	if err != nil {
		return nil, errStoreWrite
	}
	data, err := readCredentialFile(target)
	if err != nil {
		return nil, err
	}
	source.filePath, source.path = lookup, target
	return recordWithData(source, data)
}

func (s *sourceCredentialStore) replaceCredentialFile(current *credentialRecord, data []byte) error {
	// Stage and rename within the canonical target's directory. The original
	// lookup path (including any symlink) remains intact and is rechecked below.
	file, err := os.CreateTemp(filepath.Dir(current.path), ".cc-proxy-credentials-*")
	if err != nil {
		return errStoreWrite
	}
	name := file.Name()
	defer os.Remove(name)
	defer file.Close()
	if err := file.Chmod(0600); err != nil {
		return errStoreWrite
	}
	if _, err := file.Write(data); err != nil {
		return errStoreWrite
	}
	if err := file.Sync(); err != nil {
		return errStoreWrite
	}
	if err := file.Close(); err != nil {
		return errStoreWrite
	}
	// Includes active-source selection, canonical target and raw data. As with
	// external CLI writes, retargeting after this comparison remains a race.
	if !s.sourceStillCurrent(current) {
		return errStoreWrite
	}
	if err := os.Rename(name, current.path); err != nil {
		return errStoreWrite
	}
	return s.syncFileDirectory(current.path)
}

func (s *sourceCredentialStore) syncFileDirectory(path string) error {
	syncDir := s.syncDirectory
	if syncDir == nil {
		syncDir = syncCredentialDirectory
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return errStoreWrite
	}
	return nil
}

func syncCredentialDirectory(path string) (err error) {
	dir, err := os.Open(path)
	if err != nil {
		return errStoreWrite
	}
	defer func() {
		if closeErr := dir.Close(); closeErr != nil {
			err = errStoreWrite
		}
	}()
	if err := dir.Sync(); err != nil {
		return errStoreWrite
	}
	return nil
}
