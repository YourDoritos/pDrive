package api

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/argon2"
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

// Regression test for the delayed-logout bug.
//
// Proton invalidates a refresh token the moment it is used. Discarding the
// replacement leaves a spent token on disk: the running process keeps working
// from memory, and the next start dies with "Invalid refresh token"
// (Code=10013) far from the actual cause.
func TestUpdateTokensPersistsRotation(t *testing.T) {
	store, _ := newTestStore(t)

	raw := base64.StdEncoding.EncodeToString([]byte{0xff, 0x00, 0x80})
	original := &Session{
		UID: "uid-1", AccessToken: "access-1", RefreshToken: "refresh-1",
		LoginEmail: "someone@proton.me", SaltedKeyPass: raw,
	}
	if err := store.Save(original); err != nil {
		t.Fatal(err)
	}

	if err := store.UpdateTokens("uid-1", "access-2", "refresh-2"); err != nil {
		t.Fatalf("UpdateTokens: %v", err)
	}

	got, err := store.Load()
	if err != nil || got == nil {
		t.Fatalf("Load: %v", err)
	}
	if got.AccessToken != "access-2" || got.RefreshToken != "refresh-2" {
		t.Errorf("tokens = %s/%s, want access-2/refresh-2", got.AccessToken, got.RefreshToken)
	}
	// The rotation must not cost us the things a rotation does not carry.
	if got.LoginEmail != "someone@proton.me" {
		t.Errorf("login email lost through rotation: %q", got.LoginEmail)
	}
	if got.SaltedKeyPass != raw {
		t.Errorf("key passphrase lost through rotation: %q", got.SaltedKeyPass)
	}
}

// Rotations arrive repeatedly over a long-running session; each must land.
func TestUpdateTokensRepeatedly(t *testing.T) {
	store, _ := newTestStore(t)
	if err := store.Save(&Session{UID: "u", AccessToken: "a0", RefreshToken: "r0"}); err != nil {
		t.Fatal(err)
	}

	for i := 1; i <= 5; i++ {
		if err := store.UpdateTokens("u", fmt.Sprintf("a%d", i), fmt.Sprintf("r%d", i)); err != nil {
			t.Fatalf("rotation %d: %v", i, err)
		}
	}

	got, _ := store.Load()
	if got.RefreshToken != "r5" {
		t.Errorf("refresh token = %q, want r5", got.RefreshToken)
	}
}

// A rotation arriving with no session on disk must still be saved: losing it
// would strand the running process with a token nothing else knows about.
func TestUpdateTokensWithNoExistingSession(t *testing.T) {
	store, _ := newTestStore(t)

	if err := store.UpdateTokens("uid-x", "access-x", "refresh-x"); err != nil {
		t.Fatalf("UpdateTokens with no prior session: %v", err)
	}
	got, err := store.Load()
	if err != nil || got == nil {
		t.Fatalf("Load: %v", err)
	}
	if got.RefreshToken != "refresh-x" {
		t.Errorf("refresh token = %q", got.RefreshToken)
	}
}

// The Argon2 salt is key-derivation input, not a label. If it ever changes,
// every stored session becomes undecryptable and every user is silently
// logged out. Pin it.
func TestSessionSaltIsStable(t *testing.T) {
	key, err := deriveKey()
	if err != nil {
		t.Skipf("cannot derive key here: %v", err)
	}
	if len(key) != argonKeyLen {
		t.Fatalf("key length = %d, want %d", len(key), argonKeyLen)
	}

	// Derived from the pinned salt; a change to either input moves this.
	machineID, err := os.ReadFile("/etc/machine-id")
	if err != nil {
		t.Skip("no /etc/machine-id")
	}
	want := argon2.IDKey(machineID, []byte("pdrive-session-encryption-v1"),
		argonTime, argonMemory, argonThreads, argonKeyLen)
	if string(key) != string(want) {
		t.Error("the session encryption salt changed — every existing session " +
			"file is now undecryptable and every user must log in again")
	}
}
