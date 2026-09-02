package tui

import (
	"testing"

	"github.com/YourDoritos/pdrive/internal/config"
)

// useTestPaths points pDrive's directories at a temp tree, so a test that
// saves configuration cannot touch the developer's own.
func useTestPaths(t *testing.T, dir string) func() {
	t.Helper()
	return config.UseTestDirs(dir)
}
