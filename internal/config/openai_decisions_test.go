package config

import (
	"strings"
	"testing"
)

func TestOpenAIDecisionsV8RoundTripIsolation(t *testing.T) {
	data := []byte("config-version: 8\naccess:\n  api-keys: [client-test]\nupstream:\n  openai-decisions:\n    enabled: true\n    api-key-file: /run/credentials/cliproxyapi.service/openai-decisions-key\n    models: [gpt-6-luna]\n")
	if err := ValidateV8Config(data); err != nil {
		t.Fatal(err)
	}
	cfg, err := ParseConfigBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.OpenAIDecisions.Enabled || cfg.OpenAIDecisions.APIKeyFile != "/run/credentials/cliproxyapi.service/openai-decisions-key" || len(cfg.OpenAIDecisions.Models) != 1 || len(cfg.OpenAICompatibility) != 0 || len(cfg.CodexKey) != 0 || len(cfg.APIKeys) != 1 {
		t.Fatalf("isolated config lost: %+v", cfg.OpenAIDecisions)
	}
	normalized, _, err := NormalizeConfigLayout(data, true)
	if err != nil || !strings.Contains(string(normalized), "openai-decisions:") {
		t.Fatalf("normalize: %s %v", normalized, err)
	}
	reloaded, err := ParseConfigBytes(normalized)
	if err != nil || !reloaded.OpenAIDecisions.Enabled {
		t.Fatalf("reload: %v", err)
	}
	cloned := cfg.CloneForRuntime()
	cloned.OpenAIDecisions.Models[0] = "changed"
	if cfg.OpenAIDecisions.Models[0] != "gpt-6-luna" {
		t.Fatal("runtime clone shares Decisions models")
	}
}
