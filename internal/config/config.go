package config

import (
	"errors"
	"net"
	"os"
)

type Config struct {
	DatabaseURL   string
	HTTPAddr      string
	MigrationsDir string
	AdminToken    string
	RedisURL      string
}

func Load() (Config, error) {
	cfg := Config{
		DatabaseURL:   os.Getenv("DATABASE_URL"),
		HTTPAddr:      getenv("HTTP_ADDR", ":8080"),
		MigrationsDir: getenv("MIGRATIONS_DIR", "migrations"),
		AdminToken:    os.Getenv("ADMIN_TOKEN"),
		RedisURL:      os.Getenv("REDIS_URL"),
	}
	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is required")
	}
	if _, _, err := net.SplitHostPort(cfg.HTTPAddr); err != nil {
		return Config{}, errors.New("HTTP_ADDR must be host:port")
	}
	return cfg, nil
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
