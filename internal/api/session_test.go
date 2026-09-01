package api

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) (*SessionStore, string) {
	t.Helper()
	if _, err := os.Stat("/etc/machine-id"); err != nil {
		t.Skip("no /etc/machine-id on this system")
	}
	path := filepath.Join(t.TempDir(), "session.enc")
	store, err := NewSessionStore(path)
	if err != nil {
		t.Fatalf("NewSessionStore() error: %v", err)
	}
	return store, path
}

func TestSessionRoundTrip(t *testing.T) {
	store, path := newTestStore(t)

	// The key passphrase is 31 bytes of raw entropy and is not valid UTF-8.
	// Carrying it base64-encoded is what keeps JSON from mangling it.
	raw := []byte{0xff, 0xfe, 0x00, 0x80, 0xc3, 0x28, 0x01}
	want := &Session{
		UID:           "uid-123",
		AccessToken:   "access",
		RefreshToken:  "refresh",
		LoginEmail:    "someone@proton.me",
		SaltedKeyPass: base64.StdEncoding.EncodeToString(raw),
	}

	if err := store.Save(want); err != nil {
		t.Fatalf("Save() error: %v", err)
	}

	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if got == nil {
		t.Fatal("Load() returned nil after Save()")
	}
	if *got != *want {
		t.Errorf("round trip mismatch:\n got %+v\nwant %+v", *got, *want)
	}

	decoded, err := DecodeKeyPass(got.SaltedKeyPass)
	if err != nil {
		t.Fatalf("DecodeKeyPass() error: %v", err)
	}
	if string(decoded) != string(raw) {
		t.Errorf("key passphrase corrupted through storage: %v want %v", decoded, raw)
	}

	// Must not be readable as plaintext on disk.
	onDisk, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile error: %v", err)
	}
	for _, secret := range []string{"access", "refresh", "someone@proton.me"} {
		if containsBytes(onDisk, secret) {
			t.Errorf("session file contains plaintext %q", secret)
		}
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0077 != 0 {
		t.Errorf("session file mode = %o, want no group/other access", perm)
	}
}

func TestSessionLoadMissingIsNotAnError(t *testing.T) {
	store, _ := newTestStore(t)

	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load() on missing file returned error: %v", err)
	}
	if got != nil {
		t.Errorf("Load() on missing file = %+v, want nil", got)
	}
	if store.Exists() {
		t.Error("Exists() true for a missing session")
	}
}

func TestSessionLoadCorrupted(t *testing.T) {
	store, path := newTestStore(t)

	if err := os.WriteFile(path, []byte("not an encrypted session at all"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); err == nil {
		t.Error("expected an error loading a corrupted session")
	}

	// Truncated below the nonce size.
	if err := os.WriteFile(path, []byte("short"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load(); err == nil {
		t.Error("expected an error loading a truncated session")
	}
}

func TestSessionDelete(t *testing.T) {
	store, _ := newTestStore(t)

	if err := store.Save(&Session{UID: "x"}); err != nil {
		t.Fatal(err)
	}
	if !store.Exists() {
		t.Fatal("session should exist after Save()")
	}
	if err := store.Delete(); err != nil {
		t.Fatalf("Delete() error: %v", err)
	}
	if store.Exists() {
		t.Error("session still exists after Delete()")
	}
	// Deleting an absent session is a no-op, not an error.
	if err := store.Delete(); err != nil {
		t.Errorf("second Delete() error: %v", err)
	}
}

func TestDecodeKeyPassEmpty(t *testing.T) {
	if _, err := DecodeKeyPass(""); err == nil {
		t.Error("expected an error decoding an empty passphrase")
	}
}

func containsBytes(haystack []byte, needle string) bool {
	n := []byte(needle)
	for i := 0; i+len(n) <= len(haystack); i++ {
		match := true
		for j := range n {
			if haystack[i+j] != n[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
