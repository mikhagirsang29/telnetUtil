package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is loaded from environment variables (a .env file is loaded first
// if present, see main.go).
type Config struct {
	ListenAddr string

	DBHost     string
	DBPort     string
	DBUser     string
	DBPassword string
	DBName     string
	DBSSLMode  string

	AgentPort      int
	AgentTimeout   time.Duration
	MaxConcurrency int

	APIKey string
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) (int, error) {
	v := env(key, "")
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer, got %q", key, v)
	}
	return n, nil
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	v := env(key, "")
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration such as 15s, got %q", key, v)
	}
	return d, nil
}

func LoadConfig() (*Config, error) {
	cfg := &Config{
		ListenAddr: env("LISTEN_ADDR", ":8080"),
		DBHost:     env("DB_HOST", "localhost"),
		DBPort:     env("DB_PORT", "5432"),
		DBUser:     env("DB_USER", "postgres"),
		DBPassword: os.Getenv("DB_PASSWORD"),
		DBName:     env("DB_NAME", "telnet_central"),
		DBSSLMode:  env("DB_SSLMODE", "disable"),
		APIKey:     os.Getenv("API_KEY"),
	}

	var err error
	if cfg.AgentPort, err = envInt("AGENT_PORT", 29900); err != nil {
		return nil, err
	}
	if cfg.AgentTimeout, err = envDuration("AGENT_TIMEOUT", 15*time.Second); err != nil {
		return nil, err
	}
	if cfg.MaxConcurrency, err = envInt("MAX_CONCURRENCY", 50); err != nil {
		return nil, err
	}

	if cfg.AgentPort < 1 || cfg.AgentPort > 65535 {
		return nil, fmt.Errorf("AGENT_PORT must be between 1 and 65535")
	}
	if cfg.MaxConcurrency < 1 {
		return nil, fmt.Errorf("MAX_CONCURRENCY must be at least 1")
	}
	return cfg, nil
}

// DSN builds a libpq key/value connection string. Values are quoted so
// passwords containing spaces, quotes or backslashes work.
func (c *Config) DSN() string {
	q := func(s string) string {
		s = strings.ReplaceAll(s, `\`, `\\`)
		s = strings.ReplaceAll(s, `'`, `\'`)
		return "'" + s + "'"
	}
	return fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		q(c.DBHost), q(c.DBPort), q(c.DBUser), q(c.DBPassword), q(c.DBName), q(c.DBSSLMode))
}
