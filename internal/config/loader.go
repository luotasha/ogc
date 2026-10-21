package config

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

const (
	defaultConfigPath       = "~/.config/ogc/config.json"
	defaultHost             = "127.0.0.1"
	defaultPort             = 3456
	defaultBaseURL          = "https://opencode.ai/zen/go/v1"
	defaultAnthropicBaseURL = "https://opencode.ai/zen/go/v1"
	defaultTimeoutMs        = 300000
	defaultLogLevel         = "info"
)

// envVarPattern matches ${ENV_VAR} placeholders in config values.
var envVarPattern = regexp.MustCompile(`\$\{([A-Za-z0-9_]+)\}`)

// Load reads configuration from a JSON file and applies environment variable overrides.
// Config path resolution:
//  1. OGC_CONFIG env var (explicit override)
//  2. ~/.config/ogc/config.json (default)
func Load() (*Config, error) {
	configPath := resolveConfigPath()

	cfg, err := loadJSON(configPath)
	if err != nil {
		return nil, fmt.Errorf("loading config from %s: %w", configPath, err)
	}
	cfg.ConfigPath = configPath

	applyEnvOverrides(cfg)
	applyDefaults(cfg)

	if err := validate(cfg); err != nil {
		return nil, fmt.Errorf("validating config: %w", err)
	}

	return cfg, nil
}

// resolveConfigPath determines which config file to load.
func resolveConfigPath() string {
	if path := os.Getenv("OGC_CONFIG"); path != "" {
		return path
	}
	return expandHome(defaultConfigPath)
}

// expandHome replaces a leading ~ with the user's home directory.
func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		return filepath.Join(home, path[2:])
	}
	return path
}

// loadJSON reads and parses the configuration file.
func loadJSON(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	// Interpolate environment variables before parsing.
	data = []byte(interpolateEnvVars(string(data)))

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing JSON: %w", err)
	}

	return &cfg, nil
}

// interpolateEnvVars replaces ${ENV_VAR} patterns with their actual values.
func interpolateEnvVars(s string) string {
	return envVarPattern.ReplaceAllStringFunc(s, func(match string) string {
		// Extract variable name from ${VAR}
		varName := match[2 : len(match)-1]
		if val := os.Getenv(varName); val != "" {
			return val
		}
		// Leave unchanged if env var is not set
		return match
	})
}

// applyEnvOverrides applies environment variable overrides to the config.
func applyEnvOverrides(cfg *Config) {
	if raw := os.Getenv("OGC_API_KEYS"); raw != "" {
		cfg.Accounts = nil
		for _, key := range strings.Split(raw, ",") {
			key = strings.TrimSpace(key)
			sum := sha256.Sum256([]byte(key))
			cfg.Accounts = append(cfg.Accounts, Account{ID: fmt.Sprintf("key-%x", sum), APIKey: key})
		}
	}
	if path := os.Getenv("OGC_SESSION_BINDINGS"); path != "" {
		cfg.SessionBindings = path
	}
	if len(cfg.Accounts) > 0 && cfg.SessionBindings == "" {
		cfg.SessionBindings = filepath.Join(filepath.Dir(resolveConfigPath()), "session-bindings.jsonl")
	}

	if v := os.Getenv("OGC_API_KEY"); v != "" {
		cfg.APIKey = v
	}
	if v := os.Getenv("OGC_HOST"); v != "" {
		cfg.Host = v
	}
	if v := os.Getenv("OGC_PORT"); v != "" {
		if port, err := strconv.Atoi(v); err == nil {
			cfg.Port = port
		}
	}
	if v := os.Getenv("OGC_OPENAI_BASE"); v != "" {
		cfg.Upstream.BaseURL = v
	}
	if v := os.Getenv("OGC_ANTHROPIC_BASE"); v != "" {
		cfg.Upstream.AnthropicBaseURL = v
	}
	if v := os.Getenv("OGC_LOG_LEVEL"); v != "" {
		cfg.Logging.Level = v
	}
}

// applyDefaults fills in missing configuration values with sensible defaults.
func applyDefaults(cfg *Config) {
	if cfg.Host == "" {
		cfg.Host = defaultHost
	}
	if cfg.Port == 0 {
		cfg.Port = defaultPort
	}
	if cfg.Upstream.BaseURL == "" {
		cfg.Upstream.BaseURL = defaultBaseURL
	}
	if cfg.Upstream.AnthropicBaseURL == "" {
		cfg.Upstream.AnthropicBaseURL = defaultAnthropicBaseURL
	}
	if cfg.Upstream.TimeoutMs == 0 {
		cfg.Upstream.TimeoutMs = defaultTimeoutMs
	}
	if cfg.Logging.Level == "" {
		cfg.Logging.Level = defaultLogLevel
	}
}

// validate checks that all required configuration fields are present.
func validate(cfg *Config) error {
	if len(cfg.Accounts) > 0 {
		seen := map[string]bool{}
		enabled := 0
		modelIDs := make([]string, 0, len(cfg.Models))
		for id := range cfg.Models {
			modelIDs = append(modelIDs, id)
		}
		sort.Strings(modelIDs)
		defaultModel := DefaultAccountModel(cfg.Models)
		for i := range cfg.Accounts {
			a := &cfg.Accounts[i]
			if a.ID == "" || a.APIKey == "" || strings.Contains(a.APIKey, "${") || seen[a.ID] {
				return fmt.Errorf("accounts require unique IDs and resolved nonempty keys")
			}
			if a.Model == "" && defaultModel != "" {
				a.Model = defaultModel
			}
			if _, ok := cfg.Models[a.Model]; !ok {
				return fmt.Errorf("account %q references unknown model %q", a.ID, a.Model)
			}
			seen[a.ID] = true
			if !a.Disabled {
				enabled++
			}
		}
		if enabled == 0 {
			return fmt.Errorf("at least one account must be enabled")
		}
		if cfg.SessionBindings == "" {
			return fmt.Errorf("session_bindings path is required for accounts")
		}
		cfg.SessionBindings = expandHome(cfg.SessionBindings)
		return nil
	}
	if cfg.APIKey == "" {
		return fmt.Errorf("api_key is required (set via config file or OGC_API_KEY env var)")
	}
	return nil
}

// DefaultAccountModel prefers DeepSeek V4.1 Flash for OpenCode Go accounts.
// It returns the configured display key because account models reference keys,
// rather than upstream model IDs.
func DefaultAccountModel(models map[string]ModelConfig) string {
	for key, model := range models {
		if model.ModelID == "deepseek-v4.1-flash" {
			return key
		}
	}
	keys := make([]string, 0, len(models))
	for key := range models {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) > 0 {
		return keys[0]
	}
	return ""
}
