package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api/internal/auth"
	"workbuddy2api/internal/session"
	"workbuddy2api/internal/upstream"
)

func TestCacheFixSessionSurvivesCompaction(t *testing.T) {
	for _, mode := range []string{"header", "metadata"} {
		t.Run(mode, func(t *testing.T) {
			p := testPoolWith(&auth.Auth{UID: "uid-a", AccessToken: "token-a", ExpiresAt: 9999999999}, &auth.Auth{UID: "uid-b", AccessToken: "token-b", ExpiresAt: 9999999999})
			sess := session.New(session.Config{Available: p.AvailableUIDs})
			var keys, accounts, convs []string
			up := &upstream.Client{ChatBaseCN: "https://fixture.invalid", HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				keys = append(keys, body["prompt_cache_key"].(string))
				accounts = append(accounts, r.Header.Get("Authorization"))
				convs = append(convs, r.Header.Get("X-Conversation-ID"))
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sseOK))}, nil
			})}}
			h := NewHandler(Config{Pool: p, Upstream: up, Session: sess})
			for _, first := range []string{"original first user", "compacted summary"} {
				body := map[string]any{"model": "deepseek-flash", "messages": []any{map[string]any{"role": "user", "content": first}}}
				if mode == "metadata" {
					body["metadata"] = map[string]any{"session_id": "session-fixture"}
				}
				raw, _ := json.Marshal(body)
				req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(raw)))
				if mode == "header" {
					req.Header.Set("X-Session-ID", "session-fixture")
				}
				rr := httptest.NewRecorder()
				h.ServeHTTP(rr, req)
				if rr.Code != 200 {
					t.Fatalf("code=%d body=%s", rr.Code, rr.Body.String())
				}
			}
			if len(keys) != 2 || keys[0] != keys[1] || accounts[0] != accounts[1] || convs[0] != "session-fixture" || convs[1] != "session-fixture" {
				t.Fatalf("compaction drift: cache=%v account=%v conv=%v", keys, accounts, convs)
			}
			if sess.Count() != 1 {
				t.Fatalf("bindings=%d want 1", sess.Count())
			}
		})
	}
}

func TestCacheFixFallbackOnlyChangesCacheNamespace(t *testing.T) {
	p := testPoolWith(&auth.Auth{UID: "uid-a", AccessToken: "token-a", ExpiresAt: 9999999999})
	sess := session.New(session.Config{Available: p.AvailableUIDs})
	var keys, convs []string
	up := &upstream.Client{ChatBaseCN: "https://fixture.invalid", HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		keys = append(keys, body["prompt_cache_key"].(string))
		convs = append(convs, r.Header.Get("X-Conversation-ID"))
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sseOK))}, nil
	})}}
	h := NewHandler(Config{Pool: p, Upstream: up, Session: sess})
	for _, first := range []string{"session A first user", "session A first user", "session B first user"} {
		body, _ := json.Marshal(map[string]any{"model": "deepseek-flash", "messages": []any{map[string]any{"role": "user", "content": first}}})
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(string(body))))
		if rr.Code != 200 {
			t.Fatalf("code=%d", rr.Code)
		}
	}
	if keys[0] != keys[1] || keys[0] == keys[2] {
		t.Fatalf("session cache namespaces=%v", keys)
	}
	// 内容兜底只改缓存命名空间：会话头由上游 ConversationIDForHeaders 派生（issue #193），
	// 不能被本兜底键顶替。断言它与兜底同样保持「同会话稳定、跨会话不同」。
	if convs[0] != convs[1] || convs[0] == convs[2] {
		t.Fatalf("conversation headers=%v", convs)
	}
}
