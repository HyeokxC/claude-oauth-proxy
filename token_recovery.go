package main

import (
	"log"
	"time"
)

// Caller holds refreshing. Pin the original source: a Keychain failure must not
// silently replace a rotated token with credentials from an unrelated fallback file.
func (tm *TokenManager) reloadChangedCredentials(rejected string) bool {
	if tm.sourceReader == nil {
		return false
	}
	fresh, err := tm.sourceReader()
	if err != nil || fresh == nil {
		log.Printf("[token] credential source reload unavailable; retaining cached credentials")
		return false
	}
	// Unchanged CLI storage may predate our in-memory refresh-token rotation.
	if *fresh == tm.sourceCreds || fresh.AccessToken == "" || fresh.AccessToken == rejected ||
		time.Now().UnixMilli() >= fresh.ExpiresAt-5*60*1000 {
		return false
	}
	tm.sourceCreds = *fresh
	tm.mu.Lock()
	tm.creds = fresh
	tm.mu.Unlock()
	log.Printf("[token] adopted updated credentials from original source")
	return true
}
