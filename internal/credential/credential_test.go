package credential

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// isolate points os.UserConfigDir at a temp dir on every platform:
// XDG_CONFIG_HOME (Linux/BSD), HOME (macOS), AppData (Windows).
func isolate(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	t.Setenv("HOME", root)
	t.Setenv("AppData", root)
	base, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(base, dirName)
}

func TestPathUsesConfigDir(t *testing.T) {
	dir := isolate(t)
	p, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if p != filepath.Join(dir, fileName) {
		t.Fatalf("Path = %q", p)
	}
}

func TestSaveLoadClear(t *testing.T) {
	isolate(t)
	if _, err := Load(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load before save: %v", err)
	}
	const c1, c2 = "sid=abc; token=x\"y", "sid=second"
	if err := Save(c1); err != nil {
		t.Fatal(err)
	}
	if got, err := Load(); err != nil || got != c1 {
		t.Fatalf("Load = %q, %v", got, err)
	}
	if err := Save(c2); err != nil {
		t.Fatal(err)
	}
	if got, err := Load(); err != nil || got != c2 {
		t.Fatalf("Load after overwrite = %q, %v", got, err)
	}
	if err := Clear(); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Load after clear: %v", err)
	}
	if err := Clear(); err != nil {
		t.Fatalf("second Clear: %v", err)
	}
}

func TestSaveRejectsInvalid(t *testing.T) {
	dir := isolate(t)
	cases := map[string]error{
		"":            ErrEmpty,
		"   ":         ErrEmpty,
		"a\nb":        ErrInvalid,
		"a\rb":        ErrInvalid,
		"trailing\n":  ErrInvalid,
		"nul\x00byte": ErrInvalid,
	}
	for in, want := range cases {
		err := Save(in)
		if !errors.Is(err, want) {
			t.Errorf("Save(%q) = %v, want %v", in, err, want)
		}
		if err != nil && in != "" && strings.Contains(err.Error(), in) {
			t.Errorf("error leaks value: %v", err)
		}
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("invalid input should not create dir: %v", err)
	}
}

func TestPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits not applicable")
	}
	dir := isolate(t)
	// Pre-existing directory and file with loose permissions get tightened.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, fileName)
	if err := os.WriteFile(path, []byte(`{"cookie":"old"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Save("sid=secret"); err != nil {
		t.Fatal(err)
	}
	assertPerm(t, dir, 0o700)
	assertPerm(t, path, 0o600)

	// No temp files left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != fileName {
		t.Fatalf("unexpected dir contents: %v", entries)
	}

	// Load tightens a file loosened externally.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
	assertPerm(t, path, 0o600)
}

func TestRejectsSymlinkAndMalformed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	dir := isolate(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, fileName)
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, []byte(`{"cookie":"x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if err := Save("sid=1"); err == nil {
		t.Fatal("Save through symlink should fail")
	}
	if _, err := Load(); err == nil {
		t.Fatal("Load through symlink should fail")
	}
	os.Remove(path)
	if err := os.WriteFile(path, []byte("sid=leak not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load()
	if err == nil || strings.Contains(err.Error(), "leak") {
		t.Fatalf("malformed Load err = %v", err)
	}
}

func assertPerm(t *testing.T, p string, want os.FileMode) {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %o, want %o", p, got, want)
	}
}
