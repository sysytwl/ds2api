package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config is the standalone ELM bridge configuration.
type Config struct {
	Listen           string          `json:"listen"`
	BaseURL          string          `json:"base_url"`
	Sessions         []SessionConfig `json:"sessions"`
	DefaultModel     string          `json:"default_model"`
	MaxConcurrency   int             `json:"max_concurrency"`
	QueueWaitSeconds int             `json:"queue_wait_seconds"`
	APIKeys          []string        `json:"api_keys"`
	ModelListTTL     int             `json:"model_list_ttl_seconds"`
	CheckInterval    int             `json:"check_interval_seconds"`
	StateFile        string          `json:"state_file"`
	// EmptyPrompt selects the account's blank ("Empty Prompt") template before
	// every chat message, so ELM does not prepend its default institution
	// prompt. Defaults to true; set it to false to keep the ELM default.
	EmptyPrompt *bool `json:"empty_prompt"`
}

// emptyPromptEnabled reports whether the blank prompt template should be used.
func (c Config) emptyPromptEnabled() bool { return c.EmptyPrompt == nil || *c.EmptyPrompt }

// SessionConfig is one ELM browser session cookie.
type SessionConfig struct {
	Name   string `json:"name"`
	Cookie string `json:"cookie"`
}

// DefaultConfig returns the localhost defaults.
func DefaultConfig() Config {
	return Config{
		Listen:           "127.0.0.1:8787",
		BaseURL:          "https://elm.edina.ac.uk",
		MaxConcurrency:   4,
		QueueWaitSeconds: 30,
		ModelListTTL:     300,
		CheckInterval:    300,
		StateFile:        "elm2api-state.json",
	}
}

func loadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	if strings.TrimSpace(path) != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("read config %s: %w", path, err)
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return cfg, fmt.Errorf("parse config %s: %w", path, err)
		}
	}
	applyEnv(&cfg)
	normalizeConfig(&cfg)
	return cfg, nil
}

func applyEnv(cfg *Config) {
	if v := strings.TrimSpace(os.Getenv("ELM_LISTEN")); v != "" {
		cfg.Listen = v
	}
	if v := strings.TrimSpace(os.Getenv("ELM_BASE_URL")); v != "" {
		cfg.BaseURL = v
	}
	if v := strings.TrimSpace(os.Getenv("ELM_MODEL")); v != "" {
		cfg.DefaultModel = v
	}
	if v := strings.TrimSpace(os.Getenv("ELM_MAX_CONCURRENCY")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.MaxConcurrency = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("ELM_API_KEYS")); v != "" {
		cfg.APIKeys = splitList(v)
	}
	if v := strings.TrimSpace(os.Getenv("ELM_STATE_FILE")); v != "" {
		cfg.StateFile = v
	}
	if v := strings.TrimSpace(os.Getenv("ELM_CHECK_INTERVAL")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.CheckInterval = n
		}
	}
	if v := strings.TrimSpace(os.Getenv("ELM_EMPTY_PROMPT")); v != "" {
		enabled := !(v == "0" || strings.EqualFold(v, "false") || strings.EqualFold(v, "off") || strings.EqualFold(v, "no"))
		cfg.EmptyPrompt = &enabled
	}
	if v := strings.TrimSpace(os.Getenv("ELM_SESSION")); v != "" {
		cfg.Sessions = []SessionConfig{}
		for i, cookie := range splitList(v) {
			cfg.Sessions = append(cfg.Sessions, SessionConfig{Name: fmt.Sprintf("session-%d", i+1), Cookie: cookie})
		}
	}
}

func normalizeConfig(cfg *Config) {
	cfg.Listen = strings.TrimSpace(cfg.Listen)
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:8787"
	}
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://elm.edina.ac.uk"
	}
	cfg.DefaultModel = strings.TrimSpace(cfg.DefaultModel)
	cfg.StateFile = strings.TrimSpace(cfg.StateFile)
	if cfg.MaxConcurrency <= 0 {
		cfg.MaxConcurrency = 4
	}
	if cfg.QueueWaitSeconds <= 0 {
		cfg.QueueWaitSeconds = 30
	}
	if cfg.ModelListTTL <= 0 {
		cfg.ModelListTTL = 300
	}
	if cfg.CheckInterval < 0 {
		cfg.CheckInterval = 300
	}
	keys := make([]string, 0, len(cfg.APIKeys))
	for _, key := range cfg.APIKeys {
		if key = strings.TrimSpace(key); key != "" {
			keys = append(keys, key)
		}
	}
	cfg.APIKeys = keys
}

func splitList(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool { return r == ',' || r == ';' || r == '\n' || r == '\r' })
	out := make([]string, 0, len(fields))
	for _, field := range fields {
		if field = strings.TrimSpace(field); field != "" {
			out = append(out, field)
		}
	}
	return out
}
