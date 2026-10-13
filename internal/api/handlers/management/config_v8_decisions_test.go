package management

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestConfigV8DecisionsMutationPreservesInodeAndAccess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("config-version: 8\naccess:\n  api-keys: [unchanged-client]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: cfg, configFilePath: path}
	router := gin.New()
	router.PUT("/v8/management/config/*path", h.ConfigV8)
	req := httptest.NewRequest(http.MethodPut, "/v8/management/config/upstream/openai-decisions", strings.NewReader(`{"enabled":true,"api-key-file":"/run/credentials/cliproxyapi.service/openai-decisions-key","models":["gpt-6-luna"]}`))
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	if rr.Code != 200 {
		t.Fatalf("status=%d body=%s", rr.Code, rr.Body)
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("config inode changed: %v", err)
	}
	reloaded, err := config.LoadConfig(path)
	if err != nil || !reloaded.OpenAIDecisions.Enabled || reloaded.APIKeys[0] != "unchanged-client" || len(reloaded.OpenAICompatibility) != 0 {
		t.Fatalf("config mutation lost isolation: %v", err)
	}
}
