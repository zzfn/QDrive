//go:build !windows

package credential

import "os"

// mkdirPrivate creates dir with mode 0700 (unchanged Unix behaviour).
func mkdirPrivate(dir string) error { return os.Mkdir(dir, dirPerm) }

// createPrivateTemp creates an exclusive 0600 temp file in dir.
func createPrivateTemp(dir string) (*os.File, error) {
	return os.CreateTemp(dir, ".credentials-*.tmp")
}

// restrictPath is a no-op: Unix permissions are handled via chmod.
func restrictPath(string) error { return nil }
