package credential

// Upload resume state storage.
//
// Contract (for package quark):
//
//	s, err := OpenUploadState(key) // key: exactly 64 lowercase hex chars
//	data, err := s.Load()          // os.ErrNotExist (via errors.Is) if absent
//	err = s.Save(data)             // len(data) <= MaxUploadStateSize
//	err = s.Clear()
//	err = s.Close()                // releases the lock; always call it
//
// Layout: os.UserConfigDir()/qdrive/upload-state/<key>.json, guarded by
// <key>.lock. Directories are created/verified via ensureDir (0700 on Unix,
// protected current-user-only DACL on Windows); temporary files come from
// createPrivateTemp so they are private from creation.
//
// Safety rules:
//   - The lock is created with O_CREATE|O_EXCL. A leftover lock is reported
//     as ErrUploadStateLocked and is never removed automatically.
//   - Symlinks and non-regular files are rejected; reads and writes are
//     capped at MaxUploadStateSize.
//   - The first state file is published with a hard link (no overwrite); if
//     a file appears concurrently, Save fails instead of replacing it.
//   - Later Save/Clear only act on the file this store itself loaded or
//     saved, verified with os.SameFile against the recorded identity. An
//     unknown or replaced file causes an error, never a silent removal.
//   - Close only removes the lock if it is still the same file with the same
//     random token this store wrote.
//   - Stored bytes are never logged or included in errors. Content
//     validation (JSON) is the caller's responsibility.
//
// Limitations: identity checks and the subsequent rename/remove are not a
// single atomic operation, so a process running as the same user that
// deliberately races the store can still interfere; the lock only excludes
// cooperating QDrive processes. Publishing a new state requires hard-link
// support (NTFS/most Unix filesystems); otherwise Save returns an error and
// keeps nothing published. Directory sync is best effort (no-op on Windows).

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

const (
	uploadStateDirName = "upload-state"
	// MaxUploadStateSize bounds the size of a stored upload state.
	MaxUploadStateSize = 4 << 20
	lockTokenSize      = 32
)

var (
	// ErrUploadStateLocked means another store (or a stale lock) holds the key.
	ErrUploadStateLocked = errors.New("credential: upload state is locked (remove the lock file manually if no QDrive process is running)")
	// ErrUploadStateKey means the key is not 64 lowercase hex characters.
	ErrUploadStateKey = errors.New("credential: invalid upload state key")
	// ErrUploadStateTooLarge means the data exceeds MaxUploadStateSize.
	ErrUploadStateTooLarge = errors.New("credential: upload state too large")
	// ErrUploadStateChanged means the on-disk file is not the one this store owns.
	ErrUploadStateChanged = errors.New("credential: upload state file changed or unknown; refusing to modify it")
	// ErrUploadStateClosed means the store was already closed.
	ErrUploadStateClosed = errors.New("credential: upload state store is closed")
)

// UploadStateStore is an exclusive handle on one upload state file.
type UploadStateStore struct {
	mu       sync.Mutex
	dir      string
	dirInfo  fs.FileInfo
	path     string
	lockPath string
	lockInfo fs.FileInfo
	token    []byte
	// known is the identity of the state file this store last loaded/saved;
	// nil means the store does not own any existing file.
	known  fs.FileInfo
	closed bool
}

func validUploadKey(key string) bool {
	if len(key) != 64 {
		return false
	}
	for i := 0; i < len(key); i++ {
		c := key[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// OpenUploadState creates the private directory if needed and acquires the
// exclusive lock for key.
func OpenUploadState(key string) (*UploadStateStore, error) {
	if !validUploadKey(key) {
		return nil, ErrUploadStateKey
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return nil, fmt.Errorf("credential: locate config dir: %w", err)
	}
	root := filepath.Join(base, dirName)
	if err := ensureDir(root); err != nil {
		return nil, err
	}
	dir := filepath.Join(root, uploadStateDirName)
	if err := ensureDir(dir); err != nil {
		return nil, err
	}
	dirInfo, err := os.Lstat(dir)
	if err != nil {
		return nil, fmt.Errorf("credential: stat upload state dir: %w", err)
	}
	s := &UploadStateStore{
		dir:      dir,
		dirInfo:  dirInfo,
		path:     filepath.Join(dir, key+".json"),
		lockPath: filepath.Join(dir, key+".lock"),
	}
	if err := s.acquireLock(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *UploadStateStore) acquireLock() (err error) {
	token := make([]byte, lockTokenSize)
	if _, err := rand.Read(token); err != nil {
		return fmt.Errorf("credential: generate lock token: %w", err)
	}
	// O_EXCL: never follows or reuses an existing entry (including symlinks).
	// On Windows the file inherits the private DACL of the directory.
	f, err := os.OpenFile(s.lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, filePerm)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%w: %s", ErrUploadStateLocked, s.lockPath)
		}
		return fmt.Errorf("credential: create upload state lock: %w", err)
	}
	created := true
	defer func() {
		if err != nil && created {
			// Only remove the lock we just created and still identify.
			if fi, e := os.Lstat(s.lockPath); e == nil && s.lockInfo != nil && os.SameFile(fi, s.lockInfo) {
				_ = os.Remove(s.lockPath)
			}
		}
	}()
	info, statErr := f.Stat()
	if statErr != nil {
		_ = f.Close()
		created = false // cannot identify it; leave it and report
		return fmt.Errorf("credential: stat upload state lock: %w", statErr)
	}
	s.lockInfo = info
	if _, err = f.Write(token); err != nil {
		_ = f.Close()
		return fmt.Errorf("credential: write upload state lock: %w", err)
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("credential: sync upload state lock: %w", err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("credential: close upload state lock: %w", err)
	}
	if err = restrictPath(s.lockPath); err != nil {
		return fmt.Errorf("credential: restrict lock ACL: %w", err)
	}
	s.token = token
	syncDir(s.dir)
	return nil
}

// checkDir verifies the state directory is still the same real directory.
func (s *UploadStateStore) checkDir() error {
	fi, err := os.Lstat(s.dir)
	if err != nil {
		return fmt.Errorf("credential: stat upload state dir: %w", err)
	}
	if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() || !os.SameFile(fi, s.dirInfo) {
		return fmt.Errorf("credential: upload state dir %s changed", s.dir)
	}
	return nil
}

// checkLock verifies we still hold our own lock file.
func (s *UploadStateStore) checkLock() error {
	fi, err := os.Lstat(s.lockPath)
	if err != nil {
		return fmt.Errorf("credential: upload state lock lost: %w", err)
	}
	if !fi.Mode().IsRegular() || !os.SameFile(fi, s.lockInfo) {
		return errors.New("credential: upload state lock replaced")
	}
	return nil
}

func (s *UploadStateStore) precheck() error {
	if s.closed {
		return ErrUploadStateClosed
	}
	if err := s.checkDir(); err != nil {
		return err
	}
	return s.checkLock()
}

// Load returns the stored bytes, or an error matching os.ErrNotExist if no
// state exists. On success the file becomes owned by this store.
func (s *UploadStateStore) Load() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.precheck(); err != nil {
		return nil, err
	}
	fi, err := os.Lstat(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		s.known = nil
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, fmt.Errorf("credential: stat upload state: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("credential: %s is not a regular file", s.path)
	}
	if fi.Size() > MaxUploadStateSize {
		return nil, ErrUploadStateTooLarge
	}
	f, err := os.Open(s.path)
	if err != nil {
		return nil, fmt.Errorf("credential: open upload state: %w", err)
	}
	defer f.Close()
	ofi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("credential: stat upload state: %w", err)
	}
	// Detect a swap (e.g. to a symlink target) between Lstat and Open.
	if !ofi.Mode().IsRegular() || !os.SameFile(fi, ofi) {
		return nil, ErrUploadStateChanged
	}
	if runtime.GOOS != "windows" && ofi.Mode().Perm()&0o077 != 0 {
		// Via the handle, so it applies to the verified file only.
		if err := f.Chmod(filePerm); err != nil {
			return nil, fmt.Errorf("credential: restrict upload state permissions: %w", err)
		}
	}
	if runtime.GOOS == "windows" {
		if err := restrictPath(s.path); err != nil {
			return nil, fmt.Errorf("credential: restrict upload state ACL: %w", err)
		}
		if again, err := os.Lstat(s.path); err != nil || !os.SameFile(again, ofi) {
			return nil, ErrUploadStateChanged
		}
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxUploadStateSize+1))
	if err != nil {
		clear(data)
		return nil, fmt.Errorf("credential: read upload state: %w", err)
	}
	if len(data) > MaxUploadStateSize {
		clear(data)
		return nil, ErrUploadStateTooLarge
	}
	s.known = ofi
	return data, nil
}

// Save writes data atomically. The first save publishes without overwriting;
// later saves only replace the file this store loaded or saved.
func (s *UploadStateStore) Save(data []byte) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(data) > MaxUploadStateSize {
		return ErrUploadStateTooLarge
	}
	if err := s.precheck(); err != nil {
		return err
	}
	// Verify the current target before writing anything.
	fi, lerr := os.Lstat(s.path)
	switch {
	case lerr == nil:
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("credential: %s is not a regular file", s.path)
		}
		if s.known == nil || !os.SameFile(fi, s.known) {
			return ErrUploadStateChanged
		}
	case errors.Is(lerr, fs.ErrNotExist):
		if s.known != nil {
			// Our file vanished; do not guess, let the caller decide.
			return ErrUploadStateChanged
		}
	default:
		return fmt.Errorf("credential: stat upload state: %w", lerr)
	}

	tmp, err := createPrivateTemp(s.dir)
	if err != nil {
		return fmt.Errorf("credential: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	tmpOpen := true
	tmpExists := true
	defer func() {
		if tmpOpen {
			_ = tmp.Close()
		}
		if tmpExists {
			_ = os.Remove(tmpName) // our own exclusively created temp file
		}
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
	tmpInfo, err := tmp.Stat()
	if err != nil {
		return fmt.Errorf("credential: stat temp file: %w", err)
	}
	tmpOpen = false
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("credential: close temp file: %w", err)
	}

	if s.known == nil {
		// No-overwrite publish: link fails if the target exists.
		if err = os.Link(tmpName, s.path); err != nil {
			if errors.Is(err, fs.ErrExist) {
				return ErrUploadStateChanged
			}
			return fmt.Errorf("credential: publish upload state: %w", err)
		}
		pfi, perr := os.Lstat(s.path)
		if perr != nil || !os.SameFile(pfi, tmpInfo) {
			return ErrUploadStateChanged
		}
		s.known = pfi
		syncDir(s.dir)
		if rerr := os.Remove(tmpName); rerr != nil {
			// State is published; the extra link holds the same private data.
			tmpExists = false
			return fmt.Errorf("credential: remove temp link %s: %w", tmpName, rerr)
		}
		tmpExists = false
	} else {
		// Re-check right before replacing to keep the race window small.
		cur, cerr := os.Lstat(s.path)
		if cerr != nil || !cur.Mode().IsRegular() || !os.SameFile(cur, s.known) {
			return ErrUploadStateChanged
		}
		if err = os.Rename(tmpName, s.path); err != nil {
			return fmt.Errorf("credential: replace upload state: %w", err)
		}
		tmpExists = false
		pfi, perr := os.Lstat(s.path)
		if perr != nil || !os.SameFile(pfi, tmpInfo) {
			s.known = nil
			return ErrUploadStateChanged
		}
		s.known = pfi
	}
	if err = restrictPath(s.path); err != nil {
		return fmt.Errorf("credential: restrict upload state ACL: %w", err)
	}
	syncDir(s.dir)
	return nil
}

// Clear removes the state file only if it is the one this store loaded or
// saved. It is not an error if no file exists and the store owns none.
func (s *UploadStateStore) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.precheck(); err != nil {
		return err
	}
	fi, err := os.Lstat(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		if s.known != nil {
			return ErrUploadStateChanged
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("credential: stat upload state: %w", err)
	}
	if !fi.Mode().IsRegular() || s.known == nil || !os.SameFile(fi, s.known) {
		return ErrUploadStateChanged
	}
	if err := os.Remove(s.path); err != nil {
		return fmt.Errorf("credential: remove upload state: %w", err)
	}
	s.known = nil
	syncDir(s.dir)
	return nil
}

// Close releases the lock if it is still the file and token this store wrote.
// A replaced or foreign lock is left in place and reported.
func (s *UploadStateStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.known = nil
	if err := s.checkDir(); err != nil {
		return err
	}
	if err := s.checkLock(); err != nil {
		return err
	}
	f, err := os.Open(s.lockPath)
	if err != nil {
		return fmt.Errorf("credential: open upload state lock: %w", err)
	}
	ofi, serr := f.Stat()
	buf, rerr := io.ReadAll(io.LimitReader(f, lockTokenSize+1))
	_ = f.Close() // close before Remove (Windows sharing)
	if serr != nil || rerr != nil || !os.SameFile(ofi, s.lockInfo) || !bytes.Equal(buf, s.token) {
		return errors.New("credential: upload state lock not owned by this store; left in place")
	}
	if err := s.checkLock(); err != nil {
		return err
	}
	if err := os.Remove(s.lockPath); err != nil {
		return fmt.Errorf("credential: remove upload state lock: %w", err)
	}
	syncDir(s.dir)
	return nil
}
