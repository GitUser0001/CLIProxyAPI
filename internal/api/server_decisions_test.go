package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
)

const decisionsTestRequest = `{"model":"gpt-6-luna","input":"The sky is blue.","questions":[{"type":"predicate","name":"blue","instructions":"Is the sky blue?"}]}`
const decisionsTestResponse = `{"model":"gpt-6-luna","answers":[{"type":"predicate","name":"blue","probability":0.99}],"usage":{"input_tokens":42,"input_tokens_details":{"cached_tokens":12,"cache_write_tokens":0},"output_tokens":0,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":42}}`

func decisionsTestServer(t *testing.T, upstream string) (*Server, string) {
	t.Helper()
	keyPath := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyPath, []byte("sk-decisions-private-test"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{SDKConfig: config.SDKConfig{APIKeys: []string{"test-key"}}, OpenAIDecisions: config.OpenAIDecisionsConfig{Enabled: true, BaseURL: upstream + "/v1", APIKeyFile: keyPath}}
	return newTestServerWithConfig(t, cfg), keyPath
}

func decisionsCall(s *Server, body string, key string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(body))
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Chatgpt-Account-Id", "must-not-forward")
	req.Header.Set("X-Session-ID", "decisions-test-session")
	rr := httptest.NewRecorder()
	s.engine.ServeHTTP(rr, req)
	return rr
}

type decisionsUsageCapture struct{ records chan usage.Record }

func (p *decisionsUsageCapture) HandleUsage(_ context.Context, record usage.Record) {
	if record.Provider == "openai-decisions" {
		select {
		case p.records <- record:
		default:
		}
	}
}

func TestDecisionsPassthroughAndUsage(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/v1/decisions" || r.Method != http.MethodPost || string(body) != decisionsTestRequest {
			t.Errorf("wrong upstream request: %s %s %s", r.Method, r.URL.Path, body)
		}
		if r.Header.Get("Authorization") != "Bearer sk-decisions-private-test" || r.Header.Get("Chatgpt-Account-Id") != "" {
			t.Error("upstream credential isolation failed")
		}
		w.Header().Set("X-Request-ID", "decision-request-1")
		w.Header().Set("Set-Cookie", "must-not-forward")
		_, _ = io.WriteString(w, decisionsTestResponse)
	}))
	defer upstream.Close()
	s, _ := decisionsTestServer(t, upstream.URL)
	capture := &decisionsUsageCapture{records: make(chan usage.Record, 16)}
	usage.RegisterPlugin(capture)
	rr := decisionsCall(s, decisionsTestRequest, "test-key")
	if rr.Code != 200 || rr.Body.String() != decisionsTestResponse || calls.Load() != 1 || rr.Header().Get("X-Request-ID") != "decision-request-1" || rr.Header().Get("Set-Cookie") != "" {
		t.Fatalf("passthrough failed: status=%d body=%s calls=%d", rr.Code, rr.Body, calls.Load())
	}
	select {
	case record := <-capture.records:
		if record.APIKey != "test-key" || record.AuthType != "apikey" || record.AuthID != "openai-decisions" || record.Failed || record.Detail.InputTokens != 42 || record.Detail.OutputTokens != 0 || record.Detail.CacheReadTokens != 12 || record.Detail.TotalTokens != 42 || !record.Detail.TokenBreakdown.Valid() || record.ResponseModel != "gpt-6-luna" {
			t.Fatalf("wrong usage: %+v", record)
		}
		if strings.Contains(record.Source, "sk-decisions") || record.AccessTokenSHA256 != "" {
			t.Error("upstream secret entered usage metadata")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("usage not delivered")
	}
}

func TestDecisionsValidationAndAuthentication(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, decisionsTestResponse)
	}))
	defer upstream.Close()
	s, path := decisionsTestServer(t, upstream.URL)
	for _, tc := range []struct {
		name, body, key string
		status          int
	}{
		{"anonymous", decisionsTestRequest, "", 401},
		{"wrong-key", decisionsTestRequest, "wrong", 401},
		{"bad-json", "{", "test-key", 400},
		{"array", "[]", "test-key", 400},
		{"missing-model", `{}`, "test-key", 400},
		{"chat-model", strings.Replace(decisionsTestRequest, "gpt-6-luna", "gpt-6.1-sol", 1), "test-key", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rr := decisionsCall(s, tc.body, tc.key); rr.Code != tc.status {
				t.Fatalf("status=%d want=%d", rr.Code, tc.status)
			}
		})
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if rr := decisionsCall(s, decisionsTestRequest, "test-key"); rr.Code != 503 {
		t.Fatalf("public key file: %d", rr.Code)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if rr := decisionsCall(s, decisionsTestRequest, "test-key"); rr.Code != 503 {
		t.Fatalf("missing key: %d", rr.Code)
	}
	if calls.Load() != 0 {
		t.Fatal("invalid requests reached upstream")
	}
	if rr := decisionsCall(newTestServer(t), decisionsTestRequest, "test-key"); rr.Code != 503 {
		t.Fatalf("disabled: %d", rr.Code)
	}
}

func TestDecisionsTypesAndKeyRotation(t *testing.T) {
	var wantKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+wantKey {
			t.Error("rotated key not used")
		}
		body, _ := io.ReadAll(r.Body)
		if !gjson.GetBytes(body, "input").IsArray() || len(gjson.GetBytes(body, "questions").Array()) != 3 || gjson.GetBytes(body, "tools").Exists() || gjson.GetBytes(body, "stream").Exists() {
			t.Error("multimodal decisions mutated")
		}
		_, _ = io.WriteString(w, `{"model":"gpt-6-luna","answers":[{"type":"predicate","probability":0.99},{"type":"choice","choice":"a","probabilities":[0.99,0.01]},{"type":"score","score":1.9}]}`)
	}))
	defer upstream.Close()
	s, path := decisionsTestServer(t, upstream.URL)
	body := `{"model":"gpt-6-luna","input":[{"role":"user","content":[{"type":"input_text","text":"hello"},{"type":"input_image","image_url":"data:image/png;base64,AAAA"}]}],"questions":[{"type":"predicate","instructions":"yes?"},{"type":"choice","instructions":"pick","choices":["a","b"]},{"type":"score","instructions":"score","levels":["bad","good"]}]}`
	for _, key := range []string{"sk-decisions-private-test", "sk-rotated-test"} {
		wantKey = key
		if err := os.WriteFile(path, []byte(key), 0o600); err != nil {
			t.Fatal(err)
		}
		if rr := decisionsCall(s, body, "test-key"); rr.Code != 200 {
			t.Fatalf("status=%d body=%s", rr.Code, rr.Body)
		}
	}
}

func TestDecisionsUpstreamErrorsNoRetry(t *testing.T) {
	for _, status := range []int{400, 401, 403, 429, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.Header().Set("Retry-After", "5")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"error":{"code":"test_error","message":"sk-decisions-private-test sk-partial***"}}`)
			}))
			defer upstream.Close()
			s, _ := decisionsTestServer(t, upstream.URL)
			rr := decisionsCall(s, decisionsTestRequest, "test-key")
			if rr.Code != status || calls.Load() != 1 || rr.Header().Get("Retry-After") != "5" || !gjson.Valid(rr.Body.String()) || strings.Contains(rr.Body.String(), "sk-") {
				t.Fatalf("status=%d body=%s calls=%d", rr.Code, rr.Body, calls.Load())
			}
		})
	}
}

func TestDecisionsRedirectBlocked(t *testing.T) {
	var redirected atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { redirected.Add(1) }))
	defer target.Close()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer upstream.Close()
	s, _ := decisionsTestServer(t, upstream.URL)
	if rr := decisionsCall(s, decisionsTestRequest, "test-key"); rr.Code != 307 || redirected.Load() != 0 {
		t.Fatalf("redirect followed: status=%d calls=%d", rr.Code, redirected.Load())
	}
}

func TestDecisionsInvalidUpstreamResponse(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "<html>not decisions</html>") }))
	defer upstream.Close()
	s, _ := decisionsTestServer(t, upstream.URL)
	if rr := decisionsCall(s, decisionsTestRequest, "test-key"); rr.Code != 502 {
		t.Fatalf("status=%d", rr.Code)
	}
}

func TestDecisionsPayloadFinalBarrier(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if gjson.GetBytes(body, "questions.0.instructions").String() != "configured question" || gjson.GetBytes(body, "safety_identifier").Exists() {
			t.Errorf("payload rules lost: %s", body)
		}
		_, _ = io.WriteString(w, decisionsTestResponse)
	}))
	defer upstream.Close()
	s, _ := decisionsTestServer(t, upstream.URL)
	s.cfg.Payload = config.PayloadConfig{
		Override: []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "gpt-6-luna", Protocol: "decisions"}}, Params: map[string]any{"questions.0.instructions": "configured question"}}},
		Filter:   []config.PayloadFilterRule{{Models: []config.PayloadModelRule{{Name: "gpt-6-luna", Protocol: "decisions"}}, Params: []string{"safety_identifier"}}},
	}
	body := strings.TrimSuffix(decisionsTestRequest, "}") + `,"safety_identifier":"remove-me"}`
	if rr := decisionsCall(s, body, "test-key"); rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body)
	}
}

func TestDecisionsCancellation(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		close(started)
		<-r.Context().Done()
		close(stopped)
	}))
	defer upstream.Close()
	s, _ := decisionsTestServer(t, upstream.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(decisionsTestRequest)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer test-key")
	done := make(chan struct{})
	go func() { s.engine.ServeHTTP(httptest.NewRecorder(), req); close(done) }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream not reached")
	}
	cancel()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream not cancelled")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("handler not cancelled")
	}
}

func TestDecisionsBaseURLSafety(t *testing.T) {
	for _, url := range []string{"http://example.com/v1", "https://user:password@example.com/v1", "https://example.com/v1?q=1", "https://example.com/v1#fragment", "/v1"} {
		if _, err := decisionsBaseURL(url); err == nil {
			t.Errorf("accepted %s", url)
		}
	}
}

func TestDecisionsKeyCannotServeChatOrResponses(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1) }))
	defer upstream.Close()
	s, _ := decisionsTestServer(t, upstream.URL)
	for _, tc := range []struct{ path, body string }{
		{"/v1/chat/completions", `{"model":"gpt-6-luna","messages":[{"role":"user","content":"hello"}]}`},
		{"/v1/responses", `{"model":"gpt-6-luna","input":"hello"}`},
	} {
		req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
		req.Header.Set("Authorization", "Bearer test-key")
		rr := httptest.NewRecorder()
		s.engine.ServeHTTP(rr, req)
		if rr.Code < 400 {
			t.Fatalf("empty chat pool accepted %s", tc.path)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("chat route used Decisions-only key")
	}
}
