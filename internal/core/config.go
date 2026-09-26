package core

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/andreygrehov/range/internal/bytesize"
)

// Config holds everything that tunes how an artifact is read and cached.
type Config struct {
	BlockSize    int64
	MemoryCache  int64
	DiskCache    int64
	CacheDir     string
	Prefetch     bool
	MaxRangeSize int64
	s3Endpoint   string
	// Ceiling on a single remote request. A read that stalls must fail rather
	// than hang the NBD worker, the filesystem and every process behind it.
	RequestTimeout time.Duration

	ProfileMode   string // off, record or auto
	PrefetchLimit int64  // ceiling on bytes pulled from a working-set profile
}

// DefaultConfig returns the settings used when nothing overrides them.
func DefaultConfig() Config {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}
	return Config{
		BlockSize:      1 << 20,
		MemoryCache:    64 << 20,
		DiskCache:      10 << 30,
		CacheDir:       filepath.Join(home, ".cache", "range"),
		Prefetch:       true,
		MaxRangeSize:   8 << 20,
		RequestTimeout: 30 * time.Second,
		ProfileMode:    ProfileAuto,
		PrefetchLimit:  256 << 20,
	}
}

// Working-set profile modes.
const (
	ProfileOff    = "off"    // no recording, no profile prefetch
	profileRecord = "record" // record demand reads, write a profile on exit
	ProfileAuto   = "auto"   // prefetch from an existing profile and keep recording
)

// ParseProfileMode accepts off, record or auto.
func ParseProfileMode(value string) (string, error) {
	switch mode := strings.ToLower(strings.TrimSpace(value)); mode {
	case ProfileOff, profileRecord, ProfileAuto:
		return mode, nil
	default:
		return "", fmt.Errorf("profile mode must be off, record or auto, got %q", value)
	}
}

// ReadConfig applies the RANGE_* environment variables to DefaultConfig.
func ReadConfig() (Config, error) {
	c := DefaultConfig()
	sizes := []struct {
		env    string
		target *int64
		min    int64
	}{
		{"RANGE_BLOCK_SIZE", &c.BlockSize, 4096},
		{"RANGE_MEMORY_CACHE_SIZE", &c.MemoryCache, 0},
		{"RANGE_DISK_CACHE_SIZE", &c.DiskCache, 0},
		{"RANGE_MAX_RANGE_SIZE", &c.MaxRangeSize, 4096},
		{"RANGE_PREFETCH_LIMIT", &c.PrefetchLimit, 0},
	}
	for _, s := range sizes {
		value := os.Getenv(s.env)
		if value == "" {
			continue
		}
		n, err := bytesize.Parse(value)
		if err != nil {
			return c, fmt.Errorf("%s: %w", s.env, err)
		}
		if n < s.min {
			return c, fmt.Errorf("%s must be at least %s", s.env, bytesize.Format(s.min))
		}
		*s.target = n
	}
	if value := os.Getenv("RANGE_CACHE_DIR"); value != "" {
		c.CacheDir = value
	}
	if value := os.Getenv("RANGE_REQUEST_TIMEOUT"); value != "" {
		d, err := time.ParseDuration(value)
		if err != nil {
			return c, fmt.Errorf("RANGE_REQUEST_TIMEOUT: %w", err)
		}
		if d <= 0 {
			return c, errors.New("RANGE_REQUEST_TIMEOUT must be positive")
		}
		c.RequestTimeout = d
	}
	if value := os.Getenv("RANGE_PREFETCH"); value != "" {
		on, err := ParsePrefetch(value)
		if err != nil {
			return c, err
		}
		c.Prefetch = on
	}
	if value := os.Getenv("RANGE_S3_ENDPOINT"); value != "" {
		c.s3Endpoint = value
	}
	if value := os.Getenv("RANGE_PROFILE"); value != "" {
		mode, err := ParseProfileMode(value)
		if err != nil {
			return c, err
		}
		c.ProfileMode = mode
	}
	if c.MaxRangeSize < c.BlockSize {
		c.MaxRangeSize = c.BlockSize
	}
	return c, nil
}

// ParsePrefetch accepts on/off and the usual boolean spellings.
func ParsePrefetch(value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "off", "false", "0", "no":
		return false, nil
	case "on", "true", "1", "yes":
		return true, nil
	}
	return false, fmt.Errorf("prefetch must be on or off, got %q", value)
}
