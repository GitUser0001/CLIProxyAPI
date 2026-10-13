package config

// OpenAIDecisionsConfig is independent of OAuth and chat-provider credentials.
// The key is read at request time, so replacing its private file rotates it.
type OpenAIDecisionsConfig struct {
	Enabled    bool     `yaml:"enabled" json:"enabled"`
	APIKeyFile string   `yaml:"api-key-file" json:"api-key-file"`
	BaseURL    string   `yaml:"base-url,omitempty" json:"base-url,omitempty"`
	Models     []string `yaml:"models,omitempty" json:"models,omitempty"`
}
