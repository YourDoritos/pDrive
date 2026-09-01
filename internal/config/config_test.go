package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func setTestPaths(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	old := [4]string{configDir, stateDir, dataDir, cacheDir}
	configDir = filepath.Join(dir, "config")
	stateDir = filepath.Join(dir, "state")
	dataDir = filepath.Join(dir, "data")
	cacheDir = filepath.Join(dir, "cache")
	t.Cleanup(func() {
		configDir, stateDir, dataDir, cacheDir = old[0], old[1], old[2], old[3]
	})
}

func TestDefaultsMatchSpec(t *testing.T) {
	c := DefaultConfig()
	if c.Sync.Root != "~/pdrive" {
		t.Errorf("root = %q", c.Sync.Root)
	}
	if c.Sync.DeletionGuardPercent != 25 {
		t.Errorf("deletion_guard_percent = %d, want 25", c.Sync.DeletionGuardPercent)
	}
	if !c.Freshness.Gate {
		t.Error("gate should default to enabled")
	}
	if c.Freshness.MaxBlockMS != 400 {
		t.Errorf("max_block_ms = %d, want 400", c.Freshness.MaxBlockMS)
	}
	if c.Sync.MaxAutoDownloadSize != 0 {
		t.Errorf("max_auto_download_size = %d, want 0 (unlimited)", c.Sync.MaxAutoDownloadSize)
	}
}

func TestValidateClampsRatherThanFailing(t *testing.T) {
	// A bad config must never be the reason files stop syncing.
	c := &Config{}
	c.Sync.DeletionGuardPercent = 900
	c.Freshness.MaxBlockMS = 60000
	c.Limits.MaxParallelTransfers = -3
	c.Sync.MaxAutoDownloadSize = -1
	c.Validate()

	if c.Sync.Root != "~/pdrive" {
		t.Errorf("empty root not defaulted: %q", c.Sync.Root)
	}
	if c.Sync.DeletionGuardPercent != 25 {
		t.Errorf("guard = %d, want clamped to 25", c.Sync.DeletionGuardPercent)
	}
	// A listing may never be held longer than a second.
	if c.Freshness.MaxBlockMS != 400 {
		t.Errorf("max_block_ms = %d, want clamped to 400", c.Freshness.MaxBlockMS)
	}
	if c.Limits.MaxParallelTransfers != 4 {
		t.Errorf("parallel = %d, want 4", c.Limits.MaxParallelTransfers)
	}
	if c.Sync.MaxAutoDownloadSize != 0 {
		t.Errorf("size cap = %d, want 0", c.Sync.MaxAutoDownloadSize)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	setTestPaths(t)

	c := DefaultConfig()
	c.Sync.Root = "/data/drive"
	c.Sync.MaxAutoDownloadSize = 512 * 1024 * 1024
	c.Freshness.Gate = false
	c.Freshness.FreshWindow = Duration(7 * time.Second)
	c.Limits.UploadKbps = 2048
	c.Selective.Exclude = []string{"Photos", "Archive"}
	c.Account.Email = "someone@proton.me"

	if err := c.Save(); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	got, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got.Sync.Root != "/data/drive" {
		t.Errorf("root = %q", got.Sync.Root)
	}
	if got.Sync.MaxAutoDownloadSize != 512*1024*1024 {
		t.Errorf("size cap = %d", got.Sync.MaxAutoDownloadSize)
	}
	if got.Freshness.Gate {
		t.Error("gate should have persisted as false")
	}
	if got.Freshness.FreshWindow.D() != 7*time.Second {
		t.Errorf("fresh_window = %v, want 7s", got.Freshness.FreshWindow.D())
	}
	if len(got.Selective.Exclude) != 2 {
		t.Errorf("exclude = %v", got.Selective.Exclude)
	}
	if got.Account.Email != "someone@proton.me" {
		t.Errorf("email = %q", got.Account.Email)
	}
}

func TestConfigFileHasNoGroupOrWorldAccess(t *testing.T) {
	setTestPaths(t)
	if err := DefaultConfig().Save(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0077 != 0 {
		t.Errorf("config mode = %o, want no group/other access", perm)
	}
}

func TestLoadMissingReturnsValidatedDefaults(t *testing.T) {
	setTestPaths(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if c.Freshness.MaxBlockMS != 400 || c.Sync.DeletionGuardPercent != 25 {
		t.Error("missing config did not come back as validated defaults")
	}
}

func TestLoadMalformed(t *testing.T) {
	setTestPaths(t)
	if err := EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ConfigFile(), []byte("{{ not toml"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Error("expected an error for malformed TOML")
	}
}

func TestPartialConfigKeepsDefaults(t *testing.T) {
	setTestPaths(t)
	if err := EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	partial := "[sync]\nroot = \"/srv/files\"\n"
	if err := os.WriteFile(ConfigFile(), []byte(partial), 0600); err != nil {
		t.Fatal(err)
	}

	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Sync.Root != "/srv/files" {
		t.Errorf("root = %q", c.Sync.Root)
	}
	if c.Sync.DeletionGuardPercent != 25 {
		t.Errorf("guard = %d, want the default 25", c.Sync.DeletionGuardPercent)
	}
	if !c.Freshness.Gate {
		t.Error("gate should still default to true in a partial config")
	}
}

func TestExpandPath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	if got := ExpandPath("~/pdrive"); got != filepath.Join(home, "pdrive") {
		t.Errorf("ExpandPath(~/pdrive) = %q", got)
	}
	if got := ExpandPath("/absolute/path"); got != "/absolute/path" {
		t.Errorf("ExpandPath left absolute path alone? got %q", got)
	}
	// "~user" is not tilde expansion and must be left untouched.
	if got := ExpandPath("~other/files"); got != "~other/files" {
		t.Errorf("ExpandPath(~other/files) = %q, want unchanged", got)
	}
}

func TestEnsureDirsCreatesEverything(t *testing.T) {
	setTestPaths(t)
	if err := EnsureDirs(); err != nil {
		t.Fatalf("EnsureDirs() error: %v", err)
	}
	for _, dir := range []string{ConfigDir(), StateDir(), DataDir(), CacheDir(), TrashDir()} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Errorf("%s not created: %v", dir, err)
			continue
		}
		if perm := info.Mode().Perm(); perm&0077 != 0 {
			t.Errorf("%s mode = %o, want no group/other access", dir, perm)
		}
	}
}
