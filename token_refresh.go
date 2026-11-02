package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

const maxOAuthResponse = 64 * 1024

type oauthFailure struct {
	status int
	code   string
}

func (e *oauthFailure) Error() string {
	if e.code != "" {
		return fmt.Sprintf("OAuth refresh failed: status %d (%s)", e.status, e.code)
	}
	return fmt.Sprintf("OAuth refresh failed: status %d", e.status)
}

func standardOAuthCode(code string) string {
	switch code {
	case "invalid_grant", "invalid_client", "invalid_request", "unauthorized_client",
		"unsupported_grant_type", "invalid_scope", "temporarily_unavailable", "server_error":
		return code
	default:
		return ""
	}
}

func (tm *TokenManager) requestRefresh(previous Credentials) (Credentials, error) {
	if previous.RefreshToken == "" {
		return Credentials{}, errors.New("refresh token unavailable")
	}
	form := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {previous.RefreshToken}, "client_id": {OAuthClientID}}
	resp, err := tm.httpClient.PostForm(TokenRefreshURL, form)
	if err != nil {
		// Transport errors may contain request details supplied by a transport.
		return Credentials{}, errors.New("OAuth refresh request failed")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxOAuthResponse+1))
	if resp.StatusCode != http.StatusOK {
		var body struct {
			Error string `json:"error"`
		}
		code := ""
		if err == nil && len(data) <= maxOAuthResponse && json.Unmarshal(data, &body) == nil {
			code = standardOAuthCode(body.Error)
		}
		return Credentials{}, &oauthFailure{resp.StatusCode, code}
	}
	if err != nil || len(data) > maxOAuthResponse {
		return Credentials{}, errors.New("invalid OAuth refresh response")
	}
	var body struct {
		AccessToken  string  `json:"access_token"`
		RefreshToken *string `json:"refresh_token"`
		ExpiresIn    int64   `json:"expires_in"`
	}
	if json.Unmarshal(data, &body) != nil || !validOAuthToken(body.AccessToken) || body.ExpiresIn <= 0 || body.ExpiresIn > 365*24*60*60 {
		return Credentials{}, errors.New("invalid OAuth refresh response")
	}
	refreshToken := previous.RefreshToken
	if body.RefreshToken != nil {
		if !validOAuthToken(*body.RefreshToken) {
			return Credentials{}, errors.New("invalid OAuth refresh response")
		}
		refreshToken = *body.RefreshToken
	}
	return Credentials{body.AccessToken, refreshToken, tm.now().UnixMilli() + body.ExpiresIn*1000}, nil
}

func validOAuthToken(token string) bool {
	if token == "" {
		return false
	}
	for _, char := range token {
		if char <= ' ' || char >= 127 {
			return false
		}
	}
	return true
}
