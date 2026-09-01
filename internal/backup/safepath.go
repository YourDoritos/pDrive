package backup

import (
	"fmt"
	"strings"
)

// maxComponent is a conservative filename length limit. Most Linux
// filesystems allow 255 bytes per component.
const maxComponent = 255

// SanitizeComponent makes one Drive filename safe to use as a single local
// path component.
//
// Names come from the server. They are attacker-influenced in the sense that
// anything sharing into the account can choose them, and a name like
// "../../.bashrc" or "/etc/passwd" must never be able to escape the backup
// directory. Every name is therefore validated rather than trusted.
func SanitizeComponent(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("empty name")
	}
	if name == "." || name == ".." {
		return "", fmt.Errorf("reserved name %q", name)
	}
	if strings.ContainsRune(name, 0) {
		return "", fmt.Errorf("name contains a NUL byte")
	}
	if strings.ContainsRune(name, '/') {
		return "", fmt.Errorf("name contains a path separator: %q", name)
	}
	if len(name) > maxComponent {
		return "", fmt.Errorf("name is %d bytes, over the %d-byte limit", len(name), maxComponent)
	}
	return name, nil
}

// SanitizePath validates a slash-separated Drive path and returns its
// components. It rejects anything that could escape the destination root.
func SanitizePath(p string) ([]string, error) {
	if p == "" {
		return nil, fmt.Errorf("empty path")
	}
	if strings.HasPrefix(p, "/") {
		return nil, fmt.Errorf("absolute path %q", p)
	}

	parts := strings.Split(p, "/")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		clean, err := SanitizeComponent(part)
		if err != nil {
			return nil, fmt.Errorf("in path %q: %w", p, err)
		}
		out = append(out, clean)
	}
	return out, nil
}
