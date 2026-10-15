package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProxyRetry_preservesBodyAndUsesRenewedToken(t *testing.T) {
	// Given: a synthetic upstream that rejects the first request with 401.
	manager, _ := credentialFixture(t, Credentials{"old", "refresh", time.Now().Add(time.Hour).UnixMilli()})
	manager.httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return tokenResponse(200, `{"access_token":"renewed","expires_in":3600}`), nil
	})
	handler := NewProxyHandler(manager)
	var bodies, auth []string
	handler.httpClient.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		bodies = append(bodies, string(body))
		auth = append(auth, r.Header.Get("Authorization"))
		if len(bodies) == 1 {
			return tokenResponse(401, `{}`), nil
		}
		return tokenResponse(200, `{}`), nil
	})
	// When: forwarding a POST through the actual proxy handler and retry path.
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"synthetic"}],"max_tokens":8}`)))
	// Then: exactly one retry retains the full transformed payload and uses a new token.
	if recorder.Code != 200 || len(bodies) != 2 {
		t.Fatalf("status=%d requests=%d", recorder.Code, len(bodies))
	}
	if bodies[0] == "" || bodies[0] != bodies[1] {
		t.Error("retry lost or changed request body")
	}
	if auth[0] != "Bearer old" || auth[1] != "Bearer renewed" {
		t.Error("retry did not use renewed credential")
	}
}
