package swh

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const (
	defaultTimeout  = 30 * time.Minute
	defaultPoll     = 2 * time.Second
	defaultMaxBytes = 4 << 30
	DefaultAPI      = "https://archive.softwareheritage.org/api/1/"
)

type Config struct {
	API, Cache, Token string
	Timeout, Poll     time.Duration
	MaxBytes          int64
}

func ConfigFromEnv() (Config, error) {
	cache := os.Getenv("SWH_CACHE")
	if cache == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return Config{}, err
		}
		cache = filepath.Join(base, "swh-git")
	}
	cfg := Config{API: DefaultAPI, Cache: cache, Token: os.Getenv("SWH_TOKEN"), Timeout: defaultTimeout, Poll: defaultPoll, MaxBytes: defaultMaxBytes}
	if value := os.Getenv("SWH_API"); value != "" {
		cfg.API = value
	}
	for name, target := range map[string]*time.Duration{"SWH_TIMEOUT": &cfg.Timeout, "SWH_POLL": &cfg.Poll} {
		if value := os.Getenv(name); value != "" {
			parsed, err := time.ParseDuration(value)
			if err != nil {
				return Config{}, fmt.Errorf("%s: %w", name, err)
			}
			*target = parsed
		}
	}
	if value := os.Getenv("SWH_MAX_BYTES"); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return Config{}, fmt.Errorf("SWH_MAX_BYTES: %w", err)
		}
		cfg.MaxBytes = parsed
	}
	return cfg, nil
}
