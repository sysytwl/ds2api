package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestConfigElmUpstreamJSONRoundTrip(t *testing.T) {
	raw := `{"upstream":"ELM","elm":{"base_url":" https://elm.edina.ac.uk/ ","model":" Qwen/Qwen3.5-397B-A17B-FP8 "}}`
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	cfg.NormalizeCredentials()
	if cfg.Upstream != "elm" {
		t.Fatalf("upstream = %q", cfg.Upstream)
	}
	if cfg.Elm.BaseURL != "https://elm.edina.ac.uk" {
		t.Fatalf("base_url = %q", cfg.Elm.BaseURL)
	}
	if cfg.Elm.Model != "Qwen/Qwen3.5-397B-A17B-FP8" {
		t.Fatalf("model = %q", cfg.Elm.Model)
	}
	out, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("decode round trip: %v", err)
	}
	if decoded["upstream"] != "elm" {
		t.Fatalf("round trip upstream = %#v", decoded["upstream"])
	}
	elm, ok := decoded["elm"].(map[string]any)
	if !ok {
		t.Fatalf("round trip elm = %#v", decoded["elm"])
	}
	if elm["model"] != "Qwen/Qwen3.5-397B-A17B-FP8" {
		t.Fatalf("round trip model = %#v", elm["model"])
	}
}

func TestValidateUpstreamConfig(t *testing.T) {
	if err := ValidateUpstreamConfig("", ElmConfig{}); err != nil {
		t.Fatalf("default: %v", err)
	}
	if err := ValidateUpstreamConfig("deepseek", ElmConfig{}); err != nil {
		t.Fatalf("deepseek: %v", err)
	}
	if err := ValidateUpstreamConfig("elm", ElmConfig{BaseURL: "https://elm.edina.ac.uk", Model: "Qwen/Qwen3.5-397B-A17B-FP8"}); err != nil {
		t.Fatalf("elm: %v", err)
	}
	if err := ValidateUpstreamConfig("nope", ElmConfig{}); err == nil {
		t.Fatal("expected invalid upstream to be rejected")
	}
	if err := ValidateUpstreamConfig("elm", ElmConfig{BaseURL: "elm.edina.ac.uk"}); err == nil {
		t.Fatal("expected relative base_url to be rejected")
	}
}

func TestStoreElmAccessors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	content := `{"keys":["k"],"upstream":"elm","elm":{"model":"meta-llama/Llama-3.3-70B-Instruct"}}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DS2API_CONFIG_PATH", path)
	store, err := LoadStoreWithError()
	if err != nil {
		t.Fatalf("load store: %v", err)
	}
	if got := store.Upstream(); got != "elm" {
		t.Fatalf("Upstream() = %q", got)
	}
	if got := store.ElmModel(); got != "meta-llama/Llama-3.3-70B-Instruct" {
		t.Fatalf("ElmModel() = %q", got)
	}
	if got := store.ElmBaseURL(); got != "https://elm.edina.ac.uk" {
		t.Fatalf("ElmBaseURL() = %q", got)
	}
}

func TestStoreUpstreamDefaultsToDeepSeek(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"keys":["k"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DS2API_CONFIG_PATH", path)
	store, err := LoadStoreWithError()
	if err != nil {
		t.Fatalf("load store: %v", err)
	}
	if got := store.Upstream(); got != "deepseek" {
		t.Fatalf("Upstream() = %q", got)
	}
}
