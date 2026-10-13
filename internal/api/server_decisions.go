package api

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/openai"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
)

const decisionsBodyLimit = 64 << 20

var decisionsKeyPattern = regexp.MustCompile(`sk-[A-Za-z0-9_.*-]+`)

// openAIDecisions bypasses generative translators and the shared credential pool.
// It makes exactly one upstream attempt; callers own retry and fallback policy.
func (s *Server) openAIDecisions(c *gin.Context) {
	s.cfgMu.RLock()
	cfg := s.cfg
	s.cfgMu.RUnlock()
	if cfg == nil || !cfg.OpenAIDecisions.Enabled {
		decisionsError(c, http.StatusServiceUnavailable, "decisions_not_configured", "OpenAI Decisions is not enabled")
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, decisionsBodyLimit)
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			decisionsError(c, http.StatusRequestEntityTooLarge, "request_too_large", "Decisions request exceeds 64 MiB")
		} else {
			decisionsError(c, http.StatusBadRequest, "invalid_request", "Unable to read Decisions request")
		}
		return
	}
	if !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
		decisionsError(c, http.StatusBadRequest, "invalid_request", "Decisions request must be a JSON object")
		return
	}
	model := gjson.GetBytes(body, "model").String()
	if !decisionsModelAllowed(cfg.OpenAIDecisions, model) {
		decisionsError(c, http.StatusBadRequest, "unsupported_model", "Model is not enabled for OpenAI Decisions")
		return
	}
	// User payload rules are the final semantic barrier. No translation, tool
	// injection, thinking normalization, or model rewriting follows this step.
	body = helps.ApplyPayloadConfigWithRequestForExecutor(cfg, "openai-decisions", model, "decisions", "decisions", "", body, body, model, "/v1/decisions", c.Request.Header)
	servedModel := gjson.GetBytes(body, "model").String()
	if !decisionsModelAllowed(cfg.OpenAIDecisions, servedModel) {
		decisionsError(c, http.StatusBadRequest, "unsupported_model", "Configured payload selected an unsupported Decisions model")
		return
	}
	baseURL, err := decisionsBaseURL(cfg.OpenAIDecisions.BaseURL)
	if err != nil {
		decisionsError(c, http.StatusServiceUnavailable, "decisions_configuration_error", "Invalid OpenAI Decisions upstream URL")
		return
	}
	key, err := decisionsReadKey(cfg.OpenAIDecisions.APIKeyFile)
	if err != nil {
		decisionsError(c, http.StatusServiceUnavailable, "decisions_key_unavailable", "Private OpenAI Decisions API key is unavailable")
		return
	}
	ctx, cancel := s.handlers.GetContextWithCancel(openai.NewOpenAIAPIHandler(s.handlers), c, c.Request.Context())
	defer cancel()
	ctx = usage.WithRequestedModelAlias(ctx, model)
	// Usage metadata contains a label, never the upstream API key.
	credential := &auth.Auth{ID: "openai-decisions", Provider: "openai-decisions", Attributes: map[string]string{"base_url": baseURL}, Metadata: map[string]any{"auth_kind": "api_key"}}
	reporter := helps.NewUsageReporter(ctx, "openai-decisions", servedModel, credential)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/decisions", bytes.NewReader(body))
	if err != nil {
		decisionsError(c, http.StatusBadGateway, "upstream_error", "Unable to create Decisions request")
		return
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	client := reporter.TrackHTTPClient(helps.NewProxyAwareHTTPClient(ctx, cfg, nil, 0))
	// A redirect must never move this feature's key or request to another endpoint.
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		reporter.PublishFailure(ctx, &decisionsUpstreamError{http.StatusBadGateway})
		decisionsError(c, http.StatusBadGateway, "upstream_error", "OpenAI Decisions request failed")
		return
	}
	defer func() { _ = response.Body.Close() }()
	result, err := io.ReadAll(io.LimitReader(response.Body, decisionsBodyLimit+1))
	if err != nil || len(result) > decisionsBodyLimit {
		reporter.PublishFailure(ctx, &decisionsUpstreamError{http.StatusBadGateway})
		decisionsError(c, http.StatusBadGateway, "upstream_error", "Unable to read OpenAI Decisions response")
		return
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		result = bytes.ReplaceAll(result, []byte(key), []byte("[REDACTED]"))
		result = decisionsKeyPattern.ReplaceAll(result, []byte("[REDACTED]"))
		decisionsResponseHeaders(c, response.Header)
		logging.SetResponseHeaders(ctx, c.Writer.Header())
		reporter.PublishFailure(ctx, &decisionsUpstreamError{response.StatusCode})
		if !gjson.ValidBytes(result) {
			decisionsError(c, response.StatusCode, "upstream_error", "OpenAI Decisions upstream rejected the request")
			return
		}
		c.Data(response.StatusCode, "application/json", result)
		return
	}
	if !gjson.ValidBytes(result) || !gjson.GetBytes(result, "answers").IsArray() {
		reporter.PublishFailure(ctx, &decisionsUpstreamError{http.StatusBadGateway})
		decisionsError(c, http.StatusBadGateway, "invalid_upstream_response", "OpenAI Decisions returned an invalid response")
		return
	}
	decisionsResponseHeaders(c, response.Header)
	logging.SetResponseHeaders(ctx, c.Writer.Header())
	reporter.ObserveResponseModel(result)
	reporter.Publish(ctx, helps.ParseOpenAIUsage(result))
	c.Data(response.StatusCode, "application/json", result)
}

func decisionsModelAllowed(cfg config.OpenAIDecisionsConfig, model string) bool {
	if model == "" {
		return false
	}
	if len(cfg.Models) == 0 {
		return model == "gpt-6-luna"
	}
	for _, allowed := range cfg.Models {
		if model == allowed {
			return true
		}
	}
	return false
}

func decisionsBaseURL(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "https://api.openai.com/v1", nil
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", errors.New("invalid decisions URL")
	}
	loopback := u.Hostname() == "localhost"
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		loopback = ip.IsLoopback()
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return "", errors.New("decisions requires HTTPS or loopback HTTP")
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func decisionsReadKey(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("decisions key requires an absolute path")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	stat, err := f.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Mode().Perm()&0o077 != 0 {
		return "", errors.New("decisions key must be a private regular file")
	}
	data, err := io.ReadAll(io.LimitReader(f, 8193))
	key := strings.TrimSpace(string(data))
	if err != nil || len(data) > 8192 || key == "" || strings.ContainsAny(key, " \t\r\n") {
		return "", errors.New("invalid decisions key file")
	}
	return key, nil
}

func decisionsResponseHeaders(c *gin.Context, headers http.Header) {
	for name, values := range headers {
		lower := strings.ToLower(name)
		if lower == "x-request-id" || lower == "retry-after" || strings.HasPrefix(lower, "x-ratelimit-") {
			c.Writer.Header()[name] = append([]string(nil), values...)
		}
	}
}

func decisionsError(c *gin.Context, status int, code, message string) {
	errorType := "invalid_request_error"
	if status >= http.StatusInternalServerError {
		errorType = "server_error"
	}
	c.JSON(status, gin.H{"error": gin.H{"type": errorType, "code": code, "message": message}})
}

type decisionsUpstreamError struct{ status int }

func (e *decisionsUpstreamError) Error() string {
	return fmt.Sprintf("OpenAI Decisions upstream status %d", e.status)
}
func (e *decisionsUpstreamError) StatusCode() int { return e.status }
