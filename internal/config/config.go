// Package config handles application configuration loading and validation.
package config

// Config holds the complete application configuration.
type Account struct {
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	APIKey   string `json:"api_key"`
	Model    string `json:"model,omitempty"`
	Disabled bool   `json:"disabled,omitempty"`
}

type Config struct {
	ConfigPath      string                   `json:"-"`
	Accounts        []Account                `json:"accounts"`
	SessionBindings string                   `json:"session_bindings"`
	APIKey          string                   `json:"api_key"`
	Host            string                   `json:"host"`
	Port            int                      `json:"port"`
	Models          map[string]ModelConfig   `json:"models"`
	Fallbacks       map[string][]ModelConfig `json:"fallbacks"`
	Upstream        UpstreamConfig           `json:"upstream"`
	Logging         LoggingConfig            `json:"logging"`
	ModelMapping    map[string]string        `json:"model_mapping"`
}

// ModelConfig defines routing rules for a specific model.
type ModelConfig struct {
	Provider         string  `json:"provider"`
	ModelID          string  `json:"model_id"`
	Temperature      float64 `json:"temperature"`
	MaxTokens        int     `json:"max_tokens"`
	ContextThreshold int     `json:"context_threshold"`
}

// UpstreamConfig holds the upstream API settings.
type UpstreamConfig struct {
	BaseURL          string `json:"base_url"`
	AnthropicBaseURL string `json:"anthropic_base_url"`
	TimeoutMs        int    `json:"timeout_ms"`
}

// LoggingConfig controls application logging behavior.
type LoggingConfig struct {
	Level    string `json:"level"`
	Requests bool   `json:"requests"`
}
