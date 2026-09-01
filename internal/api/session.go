package api

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/nacl/secretbox"
)

const (
	argonTime    = 3
	argonMemory  = 64 * 1024
	argonThreads = 4
	argonKeyLen  = 32

	nonceSize = 24
)

// SessionStore persists the session encrypted at rest.
//
// The key derives from /etc/machine-id, so the file is useless if copied to
// another machine. It is NOT protection against someone who already has read
// access to this user's home directory — Proton offers no app passwords or
// scoped tokens, so there is no weaker credential to store instead. See
// docs/PROJECT_SPEC.md § Known Limitations.
type SessionStore struct {
	path string
	key  [32]byte
}

// NewSessionStore creates a store backed by the given path.
func NewSessionStore(path string) (*SessionStore, error) {
	key, err := deriveKey()
	if err != nil {
		return nil, fmt.Errorf("derive encryption key: %w", err)
	}
	s := &SessionStore{path: path}
	copy(s.key[:], key)
	return s, nil
}

// Save encrypts and atomically writes the session.
func (s *SessionStore) Save(session *Session) error {
	plaintext, err := json.Marshal(session)
	if err != nil {
		return fmt.Errorf("marshal session: %w", err)
	}

	var nonce [nonceSize]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return fmt.Errorf("generate nonce: %w", err)
	}
	encrypted := secretbox.Seal(nonce[:], plaintext, &nonce, &s.key)

	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return fmt.Errorf("create session dir: %w", err)
	}

	tmp := s.path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("open session file: %w", err)
	}
	if _, err := f.Write(encrypted); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write session file: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("sync session file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename session file: %w", err)
	}
	return nil
}

// Load decrypts the stored session. Returns (nil, nil) when no session exists.
func (s *SessionStore) Load() (*Session, error) {
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read session file: %w", err)
	}
	if len(data) < nonceSize {
		return nil, fmt.Errorf("session file too short")
	}

	var nonce [nonceSize]byte
	copy(nonce[:], data[:nonceSize])

	plaintext, ok := secretbox.Open(nil, data[nonceSize:], &nonce, &s.key)
	if !ok {
		return nil, fmt.Errorf("decryption failed (machine-id changed or file corrupted)")
	}

	var session Session
	if err := json.Unmarshal(plaintext, &session); err != nil {
		return nil, fmt.Errorf("unmarshal session: %w", err)
	}
	return &session, nil
}

// Delete removes the stored session.
func (s *SessionStore) Delete() error {
	err := os.Remove(s.path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Exists reports whether a session file is present.
func (s *SessionStore) Exists() bool {
	_, err := os.Stat(s.path)
	return err == nil
}

func deriveKey() ([]byte, error) {
	machineID, err := os.ReadFile("/etc/machine-id")
	if err != nil {
		return nil, fmt.Errorf("read /etc/machine-id: %w (is this a Linux system?)", err)
	}
	salt := []byte("pdrive-session-encryption-v1")
	return argon2.IDKey(machineID, salt, argonTime, argonMemory, argonThreads, argonKeyLen), nil
}

// DecodeKeyPass decodes the base64 key passphrase stored in a session.
// The passphrase is 31 raw bytes and is not valid UTF-8, so it is always
// carried base64-encoded through JSON.
func DecodeKeyPass(encoded string) ([]byte, error) {
	if encoded == "" {
		return nil, fmt.Errorf("session holds no key passphrase — log in again")
	}
	return base64.StdEncoding.DecodeString(encoded)
}
