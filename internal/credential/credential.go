// Package credential stores the QDrive session cookie on local disk.
//
// The cookie is written to os.UserConfigDir()/qdrive/credentials.json with
// directory mode 0700 and file mode 0600 (on Unix-like systems). On Windows
// the directory, temporary file and credentials file instead get a protected
// DACL that grants access only to the current user's SID (see acl_windows.go). Writes go
// through a temporary file in the same directory followed by an atomic rename,
// so readers never observe a partially written file. Cookie values are never
// included in error messages.
package credential

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const (
	dirName  = "qdrive"
	fileName = "credentials.json"
	dirPerm  = 0o700
	filePerm = 0o600
)

var (
	// ErrEmpty is returned when the cookie is empty or whitespace only.
	ErrEmpty = errors.New("credential: cookie is empty")
	// ErrInvalid is returned when the cookie contains forbidden characters.
	ErrInvalid = errors.New("credential: cookie contains newline or control characters")
	// ErrNotFound is returned by Load when no credential is stored.
	ErrNotFound = errors.New("credential: no stored credential")
)

type fileFormat struct {
	Cookie string `json:"cookie"`
}

// Path returns the credentials file location.
func Path() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("credential: locate config dir: %w", err)
	}
	return filepath.Join(base, dirName, fileName), nil
}

func validate(cookie string) error {
	if strings.TrimSpace(cookie) == "" {
		return ErrEmpty
	}
	for _, r := range cookie {
		if r == '\n' || r == '\r' || r == 0 || r == 0x7f || (r < 0x20 && r != '\t') {
			return ErrInvalid
		}
	}
	return nil
}

// ensureDir creates dir with 0700, or verifies an existing one is a real
// directory (not a symlink) and tightens its permissions.
func ensureDir(dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), dirPerm); err != nil {
		return fmt.Errorf("credential: create config dir: %w", err)
	}
	if err := mkdirPrivate(dir); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("credential: create dir: %w", err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("credential: stat dir: %w", err)
	}
	if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("credential: %s is not a regular directory", dir)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != dirPerm {
		if err := os.Chmod(dir, dirPerm); err != nil {
			return fmt.Errorf("credential: restrict dir permissions: %w", err)
		}
	}
	if err := restrictPath(dir); err != nil {
		return fmt.Errorf("credential: restrict dir ACL: %w", err)
	}
	return nil
}

// Save atomically stores cookie, replacing any previous value.
func Save(cookie string) (err error) {
	if err := validate(cookie); err != nil {
		return err
	}
	path, err := Path()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := ensureDir(dir); err != nil {
		return err
	}
	if fi, err := os.Lstat(path); err == nil && !fi.Mode().IsRegular() {
		return fmt.Errorf("credential: %s is not a regular file", path)
	}

	data, err := json.Marshal(fileFormat{Cookie: cookie})
	if err != nil {
		return errors.New("credential: encode failed")
	}
	data = append(data, '\n')

	// CreateTemp uses O_EXCL and mode 0600 with an unpredictable name.
	// On Windows the private DACL is applied at creation time.
	tmp, err := createPrivateTemp(dir)
	if err != nil {
		return fmt.Errorf("credential: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
		clear(data)
	}()
	if runtime.GOOS != "windows" {
		if err = tmp.Chmod(filePerm); err != nil {
			return fmt.Errorf("credential: chmod temp file: %w", err)
		}
	}
	if _, err = tmp.Write(data); err != nil {
		return fmt.Errorf("credential: write temp file: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("credential: sync temp file: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("credential: close temp file: %w", err)
	}
	if err = os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("credential: replace credentials file: %w", err)
	}
	if err = restrictPath(path); err != nil {
		return fmt.Errorf("credential: restrict file ACL: %w", err)
	}
	syncDir(dir)
	return nil
}

// syncDir flushes the directory entry so the rename is durable (best effort).
func syncDir(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// Load returns the stored cookie, or ErrNotFound if none exists.
func Load() (string, error) {
	path, err := Path()
	if err != nil {
		return "", err
	}
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("credential: stat file: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("credential: %s is not a regular file", path)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(path, filePerm); err != nil {
			return "", fmt.Errorf("credential: restrict file permissions: %w", err)
		}
	}
	if err := restrictPath(path); err != nil {
		return "", fmt.Errorf("credential: restrict file ACL: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("credential: read file: %w", err)
	}
	defer clear(data)
	var f fileFormat
	if err := json.Unmarshal(data, &f); err != nil {
		// Do not wrap: the decoder error may echo file contents.
		return "", errors.New("credential: malformed credentials file")
	}
	if err := validate(f.Cookie); err != nil {
		return "", fmt.Errorf("credential: stored value invalid: %w", err)
	}
	return f.Cookie, nil
}

// Clear removes the stored credential. It is not an error if none exists.
func Clear() error {
	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("credential: remove file: %w", err)
	}
	return nil
}
