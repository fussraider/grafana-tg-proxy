package config

import (
	"os"
	"testing"
)

func TestConfigLoadDefaults(t *testing.T) {
	// Clear relevant env vars
	os.Unsetenv("PORT")
	os.Unsetenv("PROXY_LIST_FILE")
	os.Unsetenv("PROXY_LIST_ENV")
	os.Unsetenv("LOG_LEVEL")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

	if cfg.Port != 8080 {
		t.Errorf("expected default Port to be 8080, got %d", cfg.Port)
	}
	if cfg.LogLevel != "info" {
		t.Errorf("expected default LogLevel to be 'info', got %s", cfg.LogLevel)
	}
}

func TestConfigLoadEnvOverrides(t *testing.T) {
	os.Setenv("PORT", "9999")
	os.Setenv("LOG_LEVEL", "DEBUG")
	os.Setenv("PROXY_LIST_ENV", "1.2.3.4:1080,socks5://5.6.7.8:1080")
	defer func() {
		os.Unsetenv("PORT")
		os.Unsetenv("LOG_LEVEL")
		os.Unsetenv("PROXY_LIST_ENV")
	}()

	cfg, err := Load()
	if err != nil {
		t.Fatalf("failed to load config: %v", err)
	}

	if cfg.Port != 9999 {
		t.Errorf("expected override Port to be 9999, got %d", cfg.Port)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("expected override LogLevel to be 'debug', got %s", cfg.LogLevel)
	}

	if len(cfg.Proxies) != 2 {
		t.Fatalf("expected 2 proxies, got %d", len(cfg.Proxies))
	}

	if cfg.Proxies[0] != "socks5://1.2.3.4:1080" {
		t.Errorf("expected first proxy to be socks5://1.2.3.4:1080, got %s", cfg.Proxies[0])
	}
	if cfg.Proxies[1] != "socks5://5.6.7.8:1080" {
		t.Errorf("expected second proxy to be socks5://5.6.7.8:1080, got %s", cfg.Proxies[1])
	}
}
