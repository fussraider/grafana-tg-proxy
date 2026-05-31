package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all configuration parameters for the proxy service.
type Config struct {
	Port               int
	MetricsPort        int
	ProxyListFile      string
	ProxyListEnv       string
	LogLevel           string
	LogFormat          string
	LogColor           bool
	RetryCheckInterval time.Duration
	DirectFallback     bool
	Proxies            []string
}

// Load loads config from environment variables and parses SOCKS5 proxy lists.
func Load() (*Config, error) {
	port, err := getEnvInt("PORT", 8080)
	if err != nil {
		return nil, err
	}

	metricsPort, err := getEnvInt("METRICS_PORT", 9090)
	if err != nil {
		return nil, err
	}

	retryInterval, err := getEnvDuration("RETRY_CHECK_INTERVAL", 10*time.Second)
	if err != nil {
		return nil, err
	}

	cfg := &Config{
		Port:               port,
		MetricsPort:        metricsPort,
		ProxyListFile:      getEnv("PROXY_LIST_FILE", "proxies.txt"),
		ProxyListEnv:       getEnv("PROXY_LIST_ENV", ""),
		LogLevel:           strings.ToLower(getEnv("LOG_LEVEL", "info")),
		LogFormat:          strings.ToLower(getEnv("LOG_FORMAT", "plain")),
		LogColor:           getEnvBool("LOG_COLOR", true),
		RetryCheckInterval: retryInterval,
		DirectFallback:     getEnvBool("DIRECT_FALLBACK", true),
	}

	cfg.Proxies = cfg.parseProxies()
	return cfg, nil
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

func getEnvBool(key string, defaultVal bool) bool {
	if val := os.Getenv(key); val != "" {
		if b, err := strconv.ParseBool(val); err == nil {
			return b
		}
	}
	return defaultVal
}

func getEnvInt(key string, defaultVal int) (int, error) {
	val := os.Getenv(key)
	if val == "" {
		return defaultVal, nil
	}
	i, err := strconv.Atoi(val)
	if err != nil {
		return 0, fmt.Errorf("invalid %s environment variable: %w", key, err)
	}
	return i, nil
}

func getEnvDuration(key string, defaultVal time.Duration) (time.Duration, error) {
	val := os.Getenv(key)
	if val == "" {
		return defaultVal, nil
	}
	d, err := time.ParseDuration(val)
	if err != nil {
		return 0, fmt.Errorf("invalid %s environment variable: %w", key, err)
	}
	return d, nil
}

// parseProxies retrieves the list of SOCKS5 proxies from the file or environment variable.
func (cfg *Config) parseProxies() []string {
	var proxies []string

	// 1. Try loading from file
	if cfg.ProxyListFile != "" {
		if file, err := os.Open(cfg.ProxyListFile); err == nil {
			defer file.Close()
			scanner := bufio.NewScanner(file)
			for scanner.Scan() {
				line := strings.TrimSpace(scanner.Text())
				if line != "" && !strings.HasPrefix(line, "#") {
					proxies = append(proxies, line)
				}
			}
		}
	}

	// 2. Fallback to environment variable if no proxies were found in file
	if len(proxies) == 0 && cfg.ProxyListEnv != "" {
		parts := strings.Split(cfg.ProxyListEnv, ",")
		for _, part := range parts {
			trimmed := strings.TrimSpace(part)
			if trimmed != "" {
				proxies = append(proxies, trimmed)
			}
		}
	}

	// Sanitize and ensure socks5:// prefix
	var sanitized []string
	for _, proxy := range proxies {
		p := strings.TrimSpace(proxy)
		if p == "" {
			continue
		}
		if !strings.HasPrefix(p, "socks5://") {
			p = "socks5://" + p
		}
		sanitized = append(sanitized, p)
	}

	return sanitized
}
