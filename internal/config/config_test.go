package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLoadMissingConfigFileIsNotAnError verifies that running without a
// config file (e.g. from a different working directory, relying purely on
// CLI flags) works instead of failing on the default path.
func TestLoadMissingConfigFileIsNotAnError(t *testing.T) {
	for name, load := range map[string]func(string) error{
		"broker":   func(p string) error { _, err := LoadBrokerConfig(p); return err },
		"consumer": func(p string) error { _, err := LoadConsumerConfig(p); return err },
		"provider": func(p string) error { _, err := LoadProviderConfig(p); return err },
	} {
		t.Run(name, func(t *testing.T) {
			missing := filepath.Join(t.TempDir(), "does-not-exist.yaml")
			if err := load(missing); err != nil {
				t.Fatalf("missing config file should not be an error, got: %v", err)
			}
		})
	}
}

// TestLoadMalformedConfigFileStillErrors verifies that a file that exists
// but cannot be parsed is a hard error — that is a typo the operator must
// see, not a condition to silently default past.
func TestLoadMalformedConfigFileStillErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "broker.yaml")
	if err := os.WriteFile(path, []byte("server: [not valid yaml"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBrokerConfig(path); err == nil {
		t.Fatal("expected an error for a malformed config file")
	}
}

// TestLoadValidConfigStillWorks is a smoke test that the refactor kept
// normal config loading intact (durations, nested keys).
func TestLoadValidConfigStillWorks(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "broker.yaml")
	body := "server:\n  listen: \":9999\"\ntunnel:\n  poll_timeout: \"7s\"\nlogging:\n  level: debug\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadBrokerConfig(path)
	if err != nil {
		t.Fatalf("LoadBrokerConfig: %v", err)
	}
	if cfg.Server.Listen != ":9999" {
		t.Errorf("listen = %q, want %q", cfg.Server.Listen, ":9999")
	}
	if cfg.Tunnel.PollTimeout != 7*time.Second {
		t.Errorf("poll_timeout = %v, want 7s", cfg.Tunnel.PollTimeout)
	}
	if cfg.Logging.Level != "debug" {
		t.Errorf("level = %q, want debug", cfg.Logging.Level)
	}
}

// TestNewLoggerLevels checks the level parsing contract: empty defaults to
// info, valid levels work, anything else is an error (a typo must be
// visible, not silently downgraded).
func TestNewLoggerLevels(t *testing.T) {
	// zap's level parsing is case-insensitive and accepts a few aliases;
	// mirror that so the test tracks the library contract.
	for _, level := range []string{"", "debug", "info", "warn", "error", "INFO", "warning", "dpanic", "panic", "fatal"} {
		if _, err := NewLogger(level); err != nil {
			t.Errorf("NewLogger(%q): unexpected error: %v", level, err)
		}
	}

	for _, level := range []string{"verbose", "InfoLevel", " "} {
		_, err := NewLogger(level)
		if err == nil {
			t.Errorf("NewLogger(%q): expected an error", level)
		} else if !strings.Contains(err.Error(), "invalid logging.level") {
			t.Errorf("NewLogger(%q): unexpected error message: %v", level, err)
		}
	}
}
