package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// pDrive is a per-user application: everything lives under the XDG base
// directories, owned by the user, mode 0700. Unlike pVPN there is no
// privileged daemon here and therefore no shared group.
//
// The one exception is pdrive-gate (see docs/PROJECT_SPEC.md), which runs as
// root but stores nothing — it holds only a fanotify fd and a socket.

// Paths are resolved on every call rather than cached at init.
//
// Caching them made the package untestable in the worst way: a test that set
// the XDG variables got the real directories anyway, because they had already
// been resolved. One such test wrote to the developer's own config file. A
// few string joins per call is a small price for a package that cannot
// quietly escape its sandbox.
//
// These overrides exist for tests that need to point somewhere specific.
var (
	configDirOverride string
	stateDirOverride  string
	dataDirOverride   string
	cacheDirOverride  string
)

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
func ConfigDir() string {
	if configDirOverride != "" {
		return configDirOverride
	}
	return filepath.Join(xdg("XDG_CONFIG_HOME", ".config"), "pdrive")
}

// StateDir returns ~/.local/state/pdrive (session, state DB, logs).
func StateDir() string {
	if stateDirOverride != "" {
		return stateDirOverride
	}
	return filepath.Join(xdg("XDG_STATE_HOME", ".local/state"), "pdrive")
}

// DataDir returns ~/.local/share/pdrive (local trash).
func DataDir() string {
	if dataDirOverride != "" {
		return dataDirOverride
	}
	return filepath.Join(xdg("XDG_DATA_HOME", ".local/share"), "pdrive")
}

// CacheDir returns ~/.cache/pdrive.
func CacheDir() string {
	if cacheDirOverride != "" {
		return cacheDirOverride
	}
	return filepath.Join(xdg("XDG_CACHE_HOME", ".cache"), "pdrive")
}

// UseTestDirs points every path under dir and returns a function restoring
// the previous values. Exported so tests in other packages cannot accidentally
// write to the developer's own configuration.
func UseTestDirs(dir string) func() {
	oldConfig, oldState := configDirOverride, stateDirOverride
	oldData, oldCache := dataDirOverride, cacheDirOverride

	configDirOverride = filepath.Join(dir, "config")
	stateDirOverride = filepath.Join(dir, "state")
	dataDirOverride = filepath.Join(dir, "data")
	cacheDirOverride = filepath.Join(dir, "cache")

	return func() {
		configDirOverride, stateDirOverride = oldConfig, oldState
		dataDirOverride, cacheDirOverride = oldData, oldCache
	}
}

// ConfigFile returns the path to config.toml.
func ConfigFile() string { return filepath.Join(ConfigDir(), "config.toml") }

// SessionFile returns the path to the encrypted session file.
func SessionFile() string { return filepath.Join(StateDir(), "session.enc") }

// StateDB returns the path to the sqlite sync state database.
func StateDB() string { return filepath.Join(StateDir(), "state.db") }

// LogFile returns the path to the daemon log.
func LogFile() string { return filepath.Join(StateDir(), "pdrive.log") }

// TrashDir returns the local trash directory used for remote deletions.
func TrashDir() string { return filepath.Join(DataDir(), "trash") }

// SocketPath returns the pdrived IPC socket path.
func SocketPath() string {
	if run := os.Getenv("XDG_RUNTIME_DIR"); run != "" {
		return filepath.Join(run, "pdrive.sock")
	}
	return filepath.Join(StateDir(), "pdrive.sock")
}

// GateSocketPath returns the pdrive-gate socket path (owned by root).
func GateSocketPath() string { return "/run/pdrive-gate.sock" }

// EnsureDirs creates every directory pDrive needs, mode 0700.
func EnsureDirs() error {
	for _, dir := range []string{ConfigDir(), StateDir(), DataDir(), CacheDir(), TrashDir()} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}
	return nil
}

// DebugPaths reports the resolved paths, for troubleshooting.
func DebugPaths() string {
	return fmt.Sprintf("config=%s state=%s data=%s cache=%s",
		ConfigDir(), StateDir(), DataDir(), CacheDir())
}
