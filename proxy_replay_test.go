package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProxyRetry_replaysTransformedMessageBodyWithNewAuthorization(t *testing.T) {
	attempts := 0
	var first []byte
	handler := benchmarkHandler(t, func(request *http.Request) (*http.Response, error) {
		attempts++
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		if len(body) == 0 || !strings.Contains(string(body), `"max_tokens":16384`) || request.ContentLength != int64(len(body)) {
			t.Error("each attempt needs the complete transformed message body")
		}
		want := "Bearer synthetic-token"
		if attempts == 1 {
			first = body
		} else {
			want = "Bearer synthetic-new-access"
			if string(body) != string(first) {
				t.Error("transformed retry body differs")
			}
		}
		if request.Header.Get("Authorization") != want {
			t.Error("retry authorization did not rotate")
		}
		status := http.StatusUnauthorized
		if attempts == 2 {
			status = http.StatusOK
		}
		return refreshResponse(request, status, `{"ok":true}`), nil
	})
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"test","max_tokens":20000,"messages":[{"role":"user","content":"synthetic-replay-message"}]}`))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK || attempts != 2 {
		t.Fatalf("status=%d attempts=%d", recorder.Code, attempts)
	}
}
