package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the pDrive configuration. Field layout mirrors
// docs/PROJECT_SPEC.md § Configuration.
type Config struct {
	Sync      SyncConfig      `toml:"sync"`
	Freshness FreshnessConfig `toml:"freshness"`
	Limits    LimitsConfig    `toml:"limits"`
	Selective SelectiveConfig `toml:"selective"`
	Account   AccountConfig   `toml:"account"`
}

type SyncConfig struct {
	// Root is the synced folder. "~" is expanded at load time.
	Root string `toml:"root"`
	// DeletionGuardPercent aborts any pass that would delete more than this
	// share of tracked nodes on either side. See PROJECT_SPEC guard 1.
	DeletionGuardPercent int `toml:"deletion_guard_percent"`
	// TrashRetentionDays controls how long locally-trashed files are kept.
	TrashRetentionDays int `toml:"trash_retention_days"`
	// MaxAutoDownloadSize is a byte count; files larger than this are not
	// downloaded automatically and land as a .pdrive-stub instead.
	// 0 means unlimited.
	MaxAutoDownloadSize int64 `toml:"max_auto_download_size"`
}

type FreshnessConfig struct {
	// Gate enables the fanotify blocking path (requires pdrive-gate as root).
	// When false, or when the gate is unreachable, pDrive falls back to
	// inotify hints plus adaptive polling.
	Gate bool `toml:"gate"`
	// MaxBlockMS is the hard ceiling on how long a directory listing may be
	// held. Enforced by the gate itself so a stuck pdrived cannot wedge the
	// filesystem.
	MaxBlockMS int `toml:"max_block_ms"`
	// FreshWindow skips the network entirely if the event cursor was polled
	// this recently. Keeps `ls` in a loop from hammering the API.
	FreshWindow Duration `toml:"fresh_window"`
	// Fallback-tier cadence.
	IdleInterval   Duration `toml:"idle_interval"`
	ActiveInterval Duration `toml:"active_interval"`
	ActiveWindow   Duration `toml:"active_window"`
}

type LimitsConfig struct {
	UploadKbps           int `toml:"upload_kbps"`
	DownloadKbps         int `toml:"download_kbps"`
	MaxParallelTransfers int `toml:"max_parallel_transfers"`
}

type SelectiveConfig struct {
	Exclude []string `toml:"exclude"`
}

type AccountConfig struct {
	// Email of the last successful login, shown on the login screen so the
	// user does not have to retype it. No secret material here — tokens live
	// in the encrypted session file.
	Email string `toml:"email"`
}

// Duration is a time.Duration that round-trips through TOML as a string
// ("30s", "5m").
type Duration time.Duration

func (d Duration) String() string { return time.Duration(d).String() }

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Duration) UnmarshalText(text []byte) error {
	parsed, err := time.ParseDuration(string(text))
	if err != nil {
		return fmt.Errorf("parse duration %q: %w", text, err)
	}
	*d = Duration(parsed)
	return nil
}

// MarshalText implements encoding.TextMarshaler.
func (d Duration) MarshalText() ([]byte, error) {
	return []byte(time.Duration(d).String()), nil
}

// DefaultConfig returns the shipped defaults.
func DefaultConfig() *Config {
	return &Config{
		Sync: SyncConfig{
			Root:                 "~/pdrive",
			DeletionGuardPercent: 25,
			TrashRetentionDays:   30,
			MaxAutoDownloadSize:  0,
		},
		Freshness: FreshnessConfig{
			Gate:           true,
			MaxBlockMS:     400,
			FreshWindow:    Duration(2 * time.Second),
			IdleInterval:   Duration(60 * time.Second),
			ActiveInterval: Duration(5 * time.Second),
			ActiveWindow:   Duration(5 * time.Minute),
		},
		Limits: LimitsConfig{
			UploadKbps:           0,
			DownloadKbps:         0,
			MaxParallelTransfers: 4,
		},
	}
}

// SyncRoot returns the sync root with "~" expanded to the user's home.
func (c *Config) SyncRoot() string { return ExpandPath(c.Sync.Root) }

// ExpandPath expands a leading "~" to the user's home directory.
func ExpandPath(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
		}
	}
	return p
}

// Validate clamps nonsensical values rather than refusing to start. A bad
// config should never be the reason a user's files stop syncing.
func (c *Config) Validate() {
	if c.Sync.Root == "" {
		c.Sync.Root = "~/pdrive"
	}
	if c.Sync.DeletionGuardPercent < 1 || c.Sync.DeletionGuardPercent > 100 {
		c.Sync.DeletionGuardPercent = 25
	}
	if c.Sync.TrashRetentionDays < 0 {
		c.Sync.TrashRetentionDays = 30
	}
	if c.Sync.MaxAutoDownloadSize < 0 {
		c.Sync.MaxAutoDownloadSize = 0
	}
	// A listing may never be held for longer than a second: past that the
	// user notices, and freshness is not worth a visible stall.
	if c.Freshness.MaxBlockMS < 1 || c.Freshness.MaxBlockMS > 1000 {
		c.Freshness.MaxBlockMS = 400
	}
	if c.Freshness.FreshWindow <= 0 {
		c.Freshness.FreshWindow = Duration(2 * time.Second)
	}
	if c.Freshness.IdleInterval <= 0 {
		c.Freshness.IdleInterval = Duration(60 * time.Second)
	}
	if c.Freshness.ActiveInterval <= 0 {
		c.Freshness.ActiveInterval = Duration(5 * time.Second)
	}
	if c.Freshness.ActiveWindow <= 0 {
		c.Freshness.ActiveWindow = Duration(5 * time.Minute)
	}
	if c.Limits.MaxParallelTransfers < 1 {
		c.Limits.MaxParallelTransfers = 4
	}
	if c.Limits.UploadKbps < 0 {
		c.Limits.UploadKbps = 0
	}
	if c.Limits.DownloadKbps < 0 {
		c.Limits.DownloadKbps = 0
	}
}

// Load reads config.toml, falling back to defaults when it does not exist.
func Load() (*Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(ConfigFile())
	if os.IsNotExist(err) {
		cfg.Validate()
		return cfg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	if _, err := toml.Decode(string(data), cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	cfg.Validate()
	return cfg, nil
}

// Save writes config.toml atomically, mode 0600.
func (c *Config) Save() error {
	if err := EnsureDirs(); err != nil {
		return err
	}

	path := ConfigFile()
	tmp := path + ".tmp"

	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("open config: %w", err)
	}
	if err := toml.NewEncoder(f).Encode(c); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("encode config: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("sync config: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename config: %w", err)
	}
	return nil
}

// Reload re-reads config.toml, picking up edits from another process.
func (c *Config) Reload() {
	fresh, err := Load()
	if err != nil {
		return
	}
	*c = *fresh
}
