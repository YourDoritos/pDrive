package tui

import (
	"strings"
	"testing"

	"github.com/YourDoritos/pdrive/internal/config"
	"github.com/YourDoritos/pdrive/internal/ipc"
)

func TestActivityDropsSkipEvents(t *testing.T) {
	m := NewActivityModel()
	m.SetSize(100, 30)

	// "skip" means a file was already correct. On a steady tree it is the
	// overwhelming majority of events and would bury everything that matters.
	for i := 0; i < 50; i++ {
		m.Add(ipc.ActivityData{Kind: "skip", Path: "unchanged.txt"})
	}
	if len(m.lines) != 0 {
		t.Errorf("skip events were recorded: %d lines", len(m.lines))
	}

	m.Add(ipc.ActivityData{Kind: "upload", Path: "real.txt", Size: 100})
	if len(m.lines) != 1 {
		t.Fatalf("lines = %d, want 1", len(m.lines))
	}
	if !strings.Contains(m.View(), "real.txt") {
		t.Error("the upload is not shown")
	}
}

// The TUI is expected to stay open for days; the log must not grow forever.
func TestActivityIsBounded(t *testing.T) {
	m := NewActivityModel()
	m.SetSize(100, 30)

	for i := 0; i < maxActivityLines*3; i++ {
		m.Add(ipc.ActivityData{Kind: "upload", Path: "f.txt"})
	}
	if len(m.lines) > maxActivityLines {
		t.Errorf("lines = %d, want at most %d", len(m.lines), maxActivityLines)
	}
}

func TestActivityScrolling(t *testing.T) {
	m := NewActivityModel()
	m.SetSize(100, 30)
	for i := 0; i < 100; i++ {
		m.Add(ipc.ActivityData{Kind: "upload", Path: "f.txt"})
	}

	m.ScrollUp(10)
	if m.offset != 10 {
		t.Errorf("offset = %d, want 10", m.offset)
	}
	// Cannot scroll past the start or beyond the newest line.
	m.ScrollUp(10000)
	if m.offset > len(m.lines) {
		t.Errorf("offset ran past the history: %d", m.offset)
	}
	m.ScrollDown(10000)
	if m.offset != 0 {
		t.Errorf("offset = %d, want 0", m.offset)
	}
}

func TestActivityEmptyView(t *testing.T) {
	m := NewActivityModel()
	m.SetSize(100, 30)
	if !strings.Contains(m.View(), "Nothing yet") {
		t.Error("empty activity view should say so")
	}
}

// A conflicts screen must make it unmistakable that nothing was lost.
func TestConflictsViewExplainsNothingWasLost(t *testing.T) {
	m := NewConflictsModel("/home/u/pdrive")
	m.SetSize(100, 30)
	m.SetConflicts([]ipc.ConflictEntry{{
		Path:      "notes.md",
		KeptLocal: "notes (conflict 2026-09-02 10-00-00 laptop).md",
	}})

	view := m.View()
	for _, want := range []string{"notes.md", "conflict", "both versions were kept", "diff"} {
		if !strings.Contains(view, want) {
			t.Errorf("conflicts view missing %q\n%s", want, view)
		}
	}
}

func TestConflictsEmptyView(t *testing.T) {
	m := NewConflictsModel("/home/u/pdrive")
	m.SetSize(100, 30)
	if !strings.Contains(m.View(), "No conflicts") {
		t.Error("empty conflicts view should say so")
	}
}

func TestConflictsCursorStaysInRange(t *testing.T) {
	m := NewConflictsModel("/home/u/pdrive")
	m.SetSize(100, 30)
	m.SetConflicts([]ipc.ConflictEntry{{Path: "a"}, {Path: "b"}})

	m.MoveCursor(-5)
	if m.cursor != 0 {
		t.Errorf("cursor = %d, want 0", m.cursor)
	}
	m.MoveCursor(50)
	if m.cursor != 1 {
		t.Errorf("cursor = %d, want 1", m.cursor)
	}

	// Shrinking the list must not leave the cursor dangling.
	m.SetConflicts(nil)
	if m.Selected() != nil {
		t.Error("Selected() on an empty list should be nil")
	}
}

func TestSettingsCycleAndSave(t *testing.T) {
	dir := t.TempDir()
	restore := useTestPaths(t, dir)
	defer restore()

	cfg := config.DefaultConfig()
	cfg.Validate()

	m := NewSettingsModel(cfg)
	m.SetSize(100, 30)

	if cfg.Sync.MaxAutoDownloadSize != 0 {
		t.Fatalf("precondition: size cap = %d", cfg.Sync.MaxAutoDownloadSize)
	}
	m.Cycle() // first row is the size cap
	if cfg.Sync.MaxAutoDownloadSize == 0 {
		t.Error("cycling the size cap did not change it")
	}
	if !m.Dirty() {
		t.Error("a change should mark the settings dirty")
	}

	if err := m.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if m.Dirty() {
		t.Error("saving should clear the dirty flag")
	}

	loaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Sync.MaxAutoDownloadSize != cfg.Sync.MaxAutoDownloadSize {
		t.Errorf("saved %d, loaded %d", cfg.Sync.MaxAutoDownloadSize, loaded.Sync.MaxAutoDownloadSize)
	}
}

// Every setting must cycle back to where it started, so no value can be
// reached once and then become unreachable.
func TestEverySettingCyclesBackToItsStart(t *testing.T) {
	for i, row := range settingRows {
		cfg := config.DefaultConfig()
		cfg.Validate()

		start := row.value(cfg)
		seen := map[string]bool{start: true}

		for step := 0; step < 20; step++ {
			row.cycle(cfg)
			cfg.Validate()
			v := row.value(cfg)
			if v == start && step > 0 {
				break
			}
			if seen[v] && v != start {
				t.Errorf("row %d (%s): value %q repeats without returning to %q",
					i, row.label, v, start)
				break
			}
			seen[v] = true
			if step == 19 {
				t.Errorf("row %d (%s): never cycled back to %q", i, row.label, start)
			}
		}
	}
}

// Validate() must never reject a value the settings screen can produce, or a
// setting would silently snap back the moment it was chosen.
func TestSettingsValuesSurviveValidation(t *testing.T) {
	for _, row := range settingRows {
		cfg := config.DefaultConfig()
		cfg.Validate()

		for step := 0; step < 8; step++ {
			row.cycle(cfg)
			before := row.value(cfg)
			cfg.Validate()
			if after := row.value(cfg); after != before {
				t.Errorf("%s: Validate() changed %q to %q", row.label, before, after)
			}
		}
	}
}

func TestSettingsView(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Validate()
	m := NewSettingsModel(cfg)
	m.SetSize(100, 30)

	view := m.View()
	for _, want := range []string{"Sync folder", "Size cap", "Deletion guard", "Blocking ls"} {
		if !strings.Contains(view, want) {
			t.Errorf("settings view missing %q", want)
		}
	}
}
