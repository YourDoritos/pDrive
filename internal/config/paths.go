package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// pdrive is a per-user application: everything lives under the XDG base
// directories, owned by the user, mode 0700. Unlike pVPN there is no
// privileged daemon here and therefore no shared group.
//
// The one exception is pdrive-gate (see docs/PROJECT_SPEC.md), which runs as
// root but stores nothing — it holds only a fanotify fd and a socket.

// Overridable in tests.
var (
	configDir string
	stateDir  string
	dataDir   string
	cacheDir  string
)

func init() {
	configDir = filepath.Join(xdg("XDG_CONFIG_HOME", ".config"), "pdrive")
	stateDir = filepath.Join(xdg("XDG_STATE_HOME", ".local/state"), "pdrive")
	dataDir = filepath.Join(xdg("XDG_DATA_HOME", ".local/share"), "pdrive")
	cacheDir = filepath.Join(xdg("XDG_CACHE_HOME", ".cache"), "pdrive")
}

// xdg returns $env if set, else $HOME/fallback.
func xdg(env, fallback string) string {
	if v := os.Getenv(env); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, fallback)
}

// ConfigDir returns ~/.config/pdrive.
func ConfigDir() string { return configDir }

// StateDir returns ~/.local/state/pdrive (session, state DB, logs).
func StateDir() string { return stateDir }

// DataDir returns ~/.local/share/pdrive (local trash).
func DataDir() string { return dataDir }

// CacheDir returns ~/.cache/pdrive.
func CacheDir() string { return cacheDir }

// ConfigFile returns the path to config.toml.
func ConfigFile() string { return filepath.Join(configDir, "config.toml") }

// SessionFile returns the path to the encrypted session file.
func SessionFile() string { return filepath.Join(stateDir, "session.enc") }

// StateDB returns the path to the sqlite sync state database.
func StateDB() string { return filepath.Join(stateDir, "state.db") }

// LogFile returns the path to the daemon log.
func LogFile() string { return filepath.Join(stateDir, "pdrive.log") }

// TrashDir returns the local trash directory used for remote deletions.
func TrashDir() string { return filepath.Join(dataDir, "trash") }

// SocketPath returns the pdrived IPC socket path.
func SocketPath() string {
	if run := os.Getenv("XDG_RUNTIME_DIR"); run != "" {
		return filepath.Join(run, "pdrive.sock")
	}
	return filepath.Join(stateDir, "pdrive.sock")
}

// GateSocketPath returns the pdrive-gate socket path (owned by root).
func GateSocketPath() string { return "/run/pdrive-gate.sock" }

// EnsureDirs creates every directory pdrive needs, mode 0700.
func EnsureDirs() error {
	for _, dir := range []string{configDir, stateDir, dataDir, cacheDir, TrashDir()} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return nil
}

// DebugPaths reports the resolved paths, for troubleshooting.
func DebugPaths() string {
	return fmt.Sprintf("config=%s state=%s data=%s cache=%s", configDir, stateDir, dataDir, cacheDir)
}
