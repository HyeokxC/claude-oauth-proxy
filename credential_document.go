package main

import (
	"encoding/json"
	"errors"
)

var errCredentialDocument = errors.New("invalid credential document")

// Raw values retain unknown metadata, including numbers too large for float64.
type credentialDocument struct {
	top    map[string]json.RawMessage
	oauth  map[string]json.RawMessage
	nested bool
	auth   Credentials
}

func decodeCredentialDocument(data []byte) (*credentialDocument, error) {
	var top map[string]json.RawMessage
	if json.Unmarshal(data, &top) != nil || top == nil {
		return nil, errCredentialDocument
	}
	doc := &credentialDocument{top: top, oauth: top}
	if raw, ok := top["claudeAiOauth"]; ok {
		doc.nested = true
		doc.oauth = nil
		if json.Unmarshal(raw, &doc.oauth) != nil || doc.oauth == nil {
			return nil, errCredentialDocument
		}
	}
	raw := data
	if doc.nested {
		raw = top["claudeAiOauth"]
	}
	if json.Unmarshal(raw, &doc.auth) != nil || !validOAuthToken(doc.auth.AccessToken) || doc.auth.ExpiresAt <= 0 {
		return nil, errCredentialDocument
	}
	return doc, nil
}

func parseCredentials(data []byte) (*Credentials, error) {
	doc, err := decodeCredentialDocument(data)
	if err != nil {
		return nil, err
	}
	return &doc.auth, nil
}

func (d *credentialDocument) rotated(next Credentials) ([]byte, error) {
	// Marshal known, typed fields only; unknown fields remain raw JSON.
	values, err := json.Marshal(next)
	if err != nil {
		return nil, errCredentialDocument
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(values, &fields) != nil {
		return nil, errCredentialDocument
	}
	for key, value := range fields {
		d.oauth[key] = value
	}
	if d.nested {
		raw, err := json.Marshal(d.oauth)
		if err != nil {
			return nil, errCredentialDocument
		}
		d.top["claudeAiOauth"] = raw
	}
	data, err := json.Marshal(d.top)
	if err != nil {
		return nil, errCredentialDocument
	}
	return data, nil
}
