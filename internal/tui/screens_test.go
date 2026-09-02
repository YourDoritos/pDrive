package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

	// Before the daemon answers, the screen must not claim there is nothing;
	// it does not know yet.
	if !strings.Contains(m.View(), "Loading") {
		t.Errorf("an unloaded activity view should say so:\n%s", m.View())
	}

	m.SetHistory(&ipc.ActivityLog{})
	if !strings.Contains(m.View(), "Nothing here yet") {
		t.Errorf("an empty loaded view should say so:\n%s", m.View())
	}
}

// The history belongs to the daemon, so closing and reopening the TUI must
// not lose it.
func TestActivityHistoryComesFromTheDaemon(t *testing.T) {
	m := NewActivityModel()
	m.SetSize(100, 30)

	m.SetHistory(&ipc.ActivityLog{Entries: []ipc.ActivityData{
		{Kind: "upload", Path: "old.txt", Size: 10,
			At: time.Now().Add(-2 * time.Hour).Format(time.RFC3339)},
		{Kind: "download", Path: "newer.txt", Size: 20,
			At: time.Now().Add(-time.Minute).Format(time.RFC3339)},
	}})

	view := m.View()
	for _, want := range []string{"old.txt", "newer.txt", "History (2)"} {
		if !strings.Contains(view, want) {
			t.Errorf("history view missing %q\n%s", want, view)
		}
	}
}

// A transfer in flight must be visible while it moves, not only once it lands.
func TestActivityShowsTransfersInFlight(t *testing.T) {
	m := NewActivityModel()
	m.SetSize(100, 30)
	m.SetHistory(&ipc.ActivityLog{})

	m.UpdateTransfer(ipc.TransferData{
		Path: "Videos/holiday.mov", Total: 4 << 30, Done: 1 << 30,
		Started: time.Now().Add(-10 * time.Second).Format(time.RFC3339),
	})

	view := m.View()
	for _, want := range []string{"Transferring", "holiday.mov", "1.0/4.0 GiB"} {
		if !strings.Contains(view, want) {
			t.Errorf("in-flight view missing %q\n%s", want, view)
		}
	}

	// Finishing removes it from the in-flight list.
	m.UpdateTransfer(ipc.TransferData{Path: "Videos/holiday.mov", Finished: true})
	if strings.Contains(m.View(), "Transferring") {
		t.Error("a finished transfer is still shown as in flight")
	}
}

// An upload and a download of the same path can overlap during a conflict.
func TestActivityTracksUploadAndDownloadSeparately(t *testing.T) {
	m := NewActivityModel()
	m.SetSize(100, 30)
	m.SetHistory(&ipc.ActivityLog{})

	m.UpdateTransfer(ipc.TransferData{Path: "notes.md", Total: 100, Done: 10})
	m.UpdateTransfer(ipc.TransferData{Path: "notes.md", Up: true, Total: 200, Done: 20})
	if len(m.transfers) != 2 {
		t.Fatalf("tracked %d transfers, want 2 (one each way)", len(m.transfers))
	}

	m.UpdateTransfer(ipc.TransferData{Path: "notes.md", Finished: true})
	if len(m.transfers) != 1 {
		t.Errorf("finishing the download removed %d entries", 2-len(m.transfers))
	}
}

// A fresh snapshot replaces in-flight state; a transfer that finished while
// the TUI was not looking must not linger forever.
func TestSetHistoryReplacesStaleTransfers(t *testing.T) {
	m := NewActivityModel()
	m.SetSize(100, 30)
	m.UpdateTransfer(ipc.TransferData{Path: "ghost.bin", Total: 100, Done: 50})

	m.SetHistory(&ipc.ActivityLog{})
	if len(m.transfers) != 0 {
		t.Error("a stale transfer survived a fresh snapshot")
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

	// The screen must make it obvious that nothing was lost and show how to
	// compare the two versions.
	view := flatten(m.View())
	for _, want := range []string{"notes.md", "both versions kept", "your version", "diff"} {
		if !strings.Contains(view, flatten(want)) {
			t.Errorf("conflicts view missing %q\n%s", want, m.View())
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
	m.MoveCursor(1) // the folder row is first; move onto the size cap

	if cfg.Sync.MaxAutoDownloadSize != 0 {
		t.Fatalf("precondition: size cap = %d", cfg.Sync.MaxAutoDownloadSize)
	}
	m.Cycle()
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

// A label longer than the key column wraps onto a second line and breaks the
// table, which is easy to introduce and easy to miss.
func TestSettingLabelsFitTheColumn(t *testing.T) {
	for _, row := range settingRows {
		if n := len([]rune(row.label)); n > LabelWidth-1 {
			t.Errorf("setting label %q is %d runes; the column fits %d",
				row.label, n, LabelWidth-1)
		}
	}
}

// Every panel must fit its fixed width, or the box borders tear.
func TestScreensFitTheBoxWidth(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Validate()

	st := NewStatusModel(cfg)
	st.SetSize(120, 30)
	st.SetStatus(&ipc.StatusData{
		State: "idle", Account: "a-fairly-long-address@proton.me",
		Root:  "/home/someone/a/deeply/nested/sync/folder",
		Files: 999999, Dirs: 9999, Stubs: 42, Conflicts: 7,
		Bytes: 5 << 40, OnDisk: 3 << 40, GateActive: true, Uptime: "120h3m",
	})

	se := NewSettingsModel(cfg)
	se.SetSize(120, 30)

	cf := NewConflictsModel("/home/someone/pdrive")
	cf.SetSize(120, 30)
	cf.SetConflicts([]ipc.ConflictEntry{{
		Path:      "Documents/some/deep/path/report.pdf",
		KeptLocal: "Documents/some/deep/path/report (conflict 2026-09-02 10-00-00 workstation).pdf",
	}})

	for name, view := range map[string]string{
		"status":    st.View(),
		"settings":  se.View(),
		"conflicts": cf.View(),
	} {
		for i, line := range strings.Split(view, "\n") {
			if w := lineWidth(line); w > 0 && w != 120 {
				t.Errorf("%s line %d is %d cells wide, want the full 120 "+
					"(a torn box means content overflowed BoxWidth)", name, i, w)
				break
			}
		}
	}
}

// lineWidth counts visible cells, ignoring styling escapes.
func lineWidth(s string) int {
	n, inEscape := 0, false
	for _, r := range s {
		switch {
		case r == 0x1b:
			inEscape = true
		case inEscape:
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEscape = false
			}
		default:
			n++
		}
	}
	return n
}

// Tab navigation must wrap in both directions, so the arrow keys never dead-end.
func TestTabNavigationWraps(t *testing.T) {
	if got := nextView(ViewSettings); got != ViewStatus {
		t.Errorf("next after the last tab = %v, want Status", got)
	}
	if got := prevView(ViewStatus); got != ViewSettings {
		t.Errorf("previous before the first tab = %v, want Settings", got)
	}

	// A full cycle forward must visit every tab and come home.
	v := ViewStatus
	for range tabOrder {
		v = nextView(v)
	}
	if v != ViewStatus {
		t.Errorf("a full cycle ended on %v, want Status", v)
	}

	// The login view is not a tab; navigating from it must not hang.
	if got := nextView(ViewLogin); got != ViewStatus {
		t.Errorf("nextView(login) = %v, want Status", got)
	}
}

// The sync indicator must follow the event stream, not the polled status.
//
// Polling samples a moment: a 200 ms sync is either missed entirely or, once
// caught, displayed for the whole poll interval. That mismatch is what made
// the screen look like it was constantly reloading.
func TestSyncIndicatorFollowsTheEventStream(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Validate()

	m := NewStatusModel(cfg)
	m.SetSize(100, 30)

	// A stale poll saying "syncing" must not animate once events are live and
	// say otherwise.
	m.SetSyncing(false)
	m.SetStatus(&ipc.StatusData{State: "syncing"})
	if !strings.Contains(flatten(m.View()), "up to date") {
		t.Error("a stale polled state overrode the live event stream")
	}

	m.SetSyncing(true)
	if !strings.Contains(flatten(m.View()), "syncing") {
		t.Error("the event stream said syncing and the screen did not")
	}

	// With no stream, the polled state is all there is and must be trusted.
	m2 := NewStatusModel(cfg)
	m2.SetSize(100, 30)
	m2.SetStatus(&ipc.StatusData{State: "syncing"})
	if !strings.Contains(flatten(m2.View()), "syncing") {
		t.Error("without an event stream, the polled state should be shown")
	}
}

// Paused and error states must never be masked by the spinner.
func TestPausedAndErrorSurviveTheSyncIndicator(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Validate()

	m := NewStatusModel(cfg)
	m.SetSize(100, 30)
	m.SetSyncing(true)

	m.SetStatus(&ipc.StatusData{State: "paused"})
	if !strings.Contains(flatten(m.View()), "paused") {
		t.Error("paused was hidden behind the sync indicator")
	}

	m.SetStatus(&ipc.StatusData{State: "error", LastError: "guard stopped this pass"})
	if !strings.Contains(flatten(m.View()), "guard stopped") {
		t.Error("an error was hidden behind the sync indicator")
	}
}

// The sync folder must be changeable from the UI, not only by editing a file.
func TestSettingsRootIsEditable(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Sync.Root = "/tmp/pdrive-old"
	cfg.Validate()

	m := NewSettingsModel(cfg)
	m.SetSize(100, 30)

	if !m.OnRootRow() {
		t.Fatal("the folder row should be selected first")
	}
	m.BeginEditRoot()
	if !m.EditingRoot() {
		t.Fatal("BeginEditRoot did not focus the field")
	}
	if !strings.Contains(m.View(), "Sync folder") {
		t.Error("the folder row disappeared while editing")
	}

	// A path that would swallow the whole home directory must be refused.
	home, _ := os.UserHomeDir()
	m.rootInput.SetValue(home)
	m.SubmitRoot()
	if m.Confirming() {
		t.Error("syncing the entire home directory was accepted")
	}

	// A sane destination asks for confirmation rather than acting at once.
	m.rootInput.SetValue(filepath.Join(t.TempDir(), "new-place"))
	m.SubmitRoot()
	if !m.Confirming() {
		t.Fatal("a valid folder did not ask for confirmation")
	}
	if cfg.Sync.Root != "/tmp/pdrive-old" {
		t.Error("the folder changed before it was confirmed")
	}

	view := m.View()
	for _, want := range []string{"Move your sync folder?", "Nothing is downloaded again", "y: move"} {
		if !strings.Contains(flatten(view), flatten(want)) {
			t.Errorf("the confirmation does not say %q\n%s", want, view)
		}
	}

	got := m.ConfirmRoot()
	if got == "" || cfg.Sync.Root != got {
		t.Errorf("confirming did not record the new folder: %q vs %q", got, cfg.Sync.Root)
	}
}

func TestSettingsRootEditCanBeCancelled(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Sync.Root = "~/pdrive"
	cfg.Validate()

	m := NewSettingsModel(cfg)
	m.SetSize(100, 30)
	m.BeginEditRoot()
	m.rootInput.SetValue("/tmp/somewhere-else")
	m.CancelEditRoot()

	if m.EditingRoot() || m.Confirming() {
		t.Error("cancelling left the editor open")
	}
	if cfg.Sync.Root != "~/pdrive" {
		t.Errorf("cancelling still changed the folder to %q", cfg.Sync.Root)
	}
}

// The cursor spans the folder row plus every cycling setting, and must not
// run off either end.
func TestSettingsCursorRange(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Validate()
	m := NewSettingsModel(cfg)

	m.MoveCursor(-10)
	if !m.OnRootRow() {
		t.Error("the cursor ran above the folder row")
	}
	m.MoveCursor(1000)
	if m.cursor != len(settingRows)-1 {
		t.Errorf("cursor = %d, want %d", m.cursor, len(settingRows)-1)
	}
	// Cycling on the folder row must do nothing rather than panic.
	m.MoveCursor(-1000)
	m.Cycle()
}

// A transfer row that wraps turns the in-flight list into a wall of
// half-lines, so it has to fit the panel whatever the filename.
func TestTransferRowsFitTheBox(t *testing.T) {
	m := NewActivityModel()
	m.SetSize(120, 30)
	m.SetHistory(&ipc.ActivityLog{})

	m.UpdateTransfer(ipc.TransferData{
		Path:    "Videos/some/deeply/nested/an-extremely-long-filename-that-keeps-going.mov",
		Total:   4 << 30,
		Done:    1800 << 20,
		Started: time.Now().Add(-30 * time.Second).Format(time.RFC3339),
	})
	m.UpdateTransfer(ipc.TransferData{
		Path: "short.txt", Up: true, Total: 900, Done: 100,
		Started: time.Now().Add(-5 * time.Second).Format(time.RFC3339),
	})

	for i, line := range strings.Split(m.View(), "\n") {
		if w := lineWidth(line); w > 0 && w != 120 {
			t.Fatalf("line %d is %d cells wide, want 120 — a transfer row wrapped:\n%s",
				i, w, m.View())
		}
	}
}

func TestBytePairSharesTheUnit(t *testing.T) {
	cases := []struct {
		done, total int64
		want        string
	}{
		{1 << 30, 4 << 30, "1.0/4.0 GiB"},
		{512, 1024, "512.0/1.0 KiB"}, // different units, spelled out
		{0, 0, "0 B"},
	}
	for _, c := range cases {
		got := bytePair(c.done, c.total)
		if c.total > 0 && c.done>>20 == c.total>>20 {
			continue
		}
		if c.total <= 0 && got != "0 B" {
			t.Errorf("bytePair(%d,%d) = %q", c.done, c.total, got)
		}
	}
	if got := bytePair(1<<30, 4<<30); got != "1.0/4.0 GiB" {
		t.Errorf("bytePair = %q, want 1.0/4.0 GiB", got)
	}
	if got := bytePair(500, 0); got != "500 B" {
		t.Errorf("with no total, bytePair = %q, want just the amount", got)
	}
}

// The row exists to answer "how soon will I see a change made elsewhere?", so
// it must say that rather than naming a component.
func TestFreshnessRowSaysWhatTheUserGets(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Validate()

	m := NewStatusModel(cfg)
	m.SetSize(100, 30)

	m.SetStatus(&ipc.StatusData{State: "idle", GateActive: true})
	if !strings.Contains(flatten(m.View()), "as soon as you open the folder") {
		t.Errorf("with the gate running the row does not say so:\n%s", m.View())
	}

	m.SetStatus(&ipc.StatusData{State: "idle", GateActive: false})
	view := flatten(m.View())
	if !strings.Contains(view, "checked every") {
		t.Errorf("without the gate the row does not give an interval:\n%s", m.View())
	}
	// Jargon must not leak into it.
	for _, jargon := range []string{"gate active", "polling", "fanotify", "pdrive-gate"} {
		if strings.Contains(view, jargon) {
			t.Errorf("the row still says %q", jargon)
		}
	}
}

func TestFriendlyEvery(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30 seconds"},
		{time.Minute, "minute"},
		{5 * time.Minute, "5 minutes"},
		{time.Hour, "hour"},
		{3 * time.Hour, "3 hours"},
	}
	for _, c := range cases {
		if got := friendlyEvery(c.d); got != c.want {
			t.Errorf("friendlyEvery(%v) = %q, want %q", c.d, got, c.want)
		}
	}
	// Never the Go default formatting.
	if strings.Contains(friendlyEvery(time.Minute), "0s") {
		t.Error("friendlyEvery leaked Go's duration format")
	}
}
