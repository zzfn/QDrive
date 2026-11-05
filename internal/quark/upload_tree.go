package quark

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Directory uploads run in two phases:
//
//  1. plan: walk the local tree with Lstat/ReadDir only (symlinks are never
//     followed), validating every name, rejecting symlinks, special files and
//     empty files, and enforcing item and depth limits. Nothing remote is
//     touched until the whole plan is valid.
//  2. apply: create/merge directories parent-first, then upload files with
//     UploadProgress. Existing remote names observed in listings are counted
//     as conflicts and the walk continues. The server has no verified atomic
//     no-overwrite operation, so concurrent remote writes can race the check.
//     Any other error, including an unconfirmed outcome, stops immediately.
//
// Directories are re-checked by identity and names; files by identity, mode,
// size and mtime. Each file is opened once, then the opened handle is checked
// against the plan and used for the upload. Remaining TOCTOU windows still
// exist for directory traversal and concurrent in-place file modifications.
const (
	upTreeMaxItems = 100000
	upTreeMaxDepth = 64
)

// DirUploadError reports the outcome of an UploadDir call that did not
// fully succeed. Uploaded/CreatedDirs count work completed in this call.
// If Err is nil-equivalent (only conflicts occurred), Err is the first
// conflict, which wraps ErrExists. Otherwise Err is the stopping error and
// FirstConflictErr (if any) is also reachable through errors.Is.
type DirUploadError struct {
	Uploaded         int
	CreatedDirs      int
	Conflicts        int
	FirstConflict    string // remote path of the first conflict
	FirstConflictErr error
	Failed           string // relative path where processing stopped, if any
	Err              error
}

func (e *DirUploadError) Error() string {
	if e.Failed != "" || e.Err != e.FirstConflictErr {
		return fmt.Sprintf("quark: upload dir: %d file(s) uploaded, %d dir(s) created, %d conflict(s) (first %q); stopped at %q: %v",
			e.Uploaded, e.CreatedDirs, e.Conflicts, e.FirstConflict, e.Failed, e.Err)
	}
	return fmt.Sprintf("quark: upload dir: %d file(s) uploaded, %d dir(s) created, %d conflict(s) not overwritten; first at %q: %v",
		e.Uploaded, e.CreatedDirs, e.Conflicts, e.FirstConflict, e.Err)
}

func (e *DirUploadError) Unwrap() []error {
	out := []error{e.Err}
	if e.FirstConflictErr != nil && e.FirstConflictErr != e.Err {
		out = append(out, e.FirstConflictErr)
	}
	return out
}

type upNode struct {
	rel   string // slash-separated, relative to the uploaded root ("" = root)
	local string // local filesystem path
	isDir bool
	info  os.FileInfo
	names []string // sorted child names (dirs only)
	kids  []*upNode
	par   *upNode
	depth int
}

// UploadDir uploads the local directory localPath into remoteParent as a
// directory with the same base name, merging into it if it already exists.
// Existing remote files or name clashes found in the listing are reported in
// a *DirUploadError (errors.Is(err, ErrExists)) while other entries continue.
// Listing and upload are not atomic; do not concurrently modify the remote
// destination. progress receives per-file, slash-separated relative paths.
func (c *Client) UploadDir(ctx context.Context, localPath, remoteParent string, progress func(relative string, done, total int64)) error {
	root, err := planUploadTree(ctx, localPath)
	if err != nil {
		return err
	}
	rootName := filepath.Base(root.local)
	if err := recheckDir(root); err != nil {
		return err
	}

	parent, err := c.resolveDir(ctx, remoteParent)
	if err != nil {
		return err
	}
	parentPath := "/" + strings.Join(mustSplit(remoteParent), "/")

	res := &DirUploadError{}
	conflict := func(remotePath string, cerr error) {
		res.Conflicts++
		if res.FirstConflictErr == nil {
			res.FirstConflict = remotePath
			res.FirstConflictErr = cerr
		}
	}
	stop := func(rel string, err error) error {
		res.Failed = displayRel(rel)
		res.Err = err
		return res
	}

	type dirJob struct {
		n          *upNode
		remoteID   string
		remotePath string
	}
	var queue []dirJob

	// Root directory: create or merge.
	rootRemote := joinPath(parentPath, rootName)
	id, created, ok, err := c.ensureRemoteDir(ctx, parent.ID, parentPath, rootName)
	if err != nil {
		return stop("", err)
	}
	if !ok {
		conflict(rootRemote, fmt.Errorf("%w: %s is not a directory", ErrExists, rootRemote))
		res.Err = res.FirstConflictErr
		return res
	}
	if created {
		res.CreatedDirs++
	}
	queue = append(queue, dirJob{root, id, rootRemote})

	for len(queue) > 0 {
		j := queue[0]
		queue = queue[1:]
		if err := ctx.Err(); err != nil {
			return stop(j.n.rel, err)
		}
		if err := recheckDir(j.n); err != nil {
			return stop(j.n.rel, err)
		}
		existing, err := c.listDir(ctx, j.remoteID)
		if err != nil {
			return stop(j.n.rel, err)
		}
		byName := map[string][]File{}
		for _, e := range existing {
			byName[e.Name] = append(byName[e.Name], e)
		}
		for _, k := range j.n.kids {
			if err := ctx.Err(); err != nil {
				return stop(k.rel, err)
			}
			rp := joinPath(j.remotePath, filepath.Base(k.local))
			name := filepath.Base(k.local)
			m := byName[name]
			if k.isDir {
				switch {
				case len(m) > 1:
					return stop(k.rel, fmt.Errorf("%w: %s", ErrAmbiguous, rp))
				case len(m) == 1 && m[0].IsDir:
					queue = append(queue, dirJob{k, m[0].ID, rp})
					continue
				case len(m) == 1:
					conflict(rp, fmt.Errorf("%w: %s is a file", ErrExists, rp))
					continue // skip whole subtree
				}
				id, created, ok, err := c.ensureRemoteDir(ctx, j.remoteID, j.remotePath, name)
				if err != nil {
					return stop(k.rel, err)
				}
				if !ok {
					conflict(rp, fmt.Errorf("%w: %s is not a directory", ErrExists, rp))
					continue
				}
				if created {
					res.CreatedDirs++
				}
				queue = append(queue, dirJob{k, id, rp})
				continue
			}
			if len(m) > 0 {
				conflict(rp, fmt.Errorf("%w: %s", ErrExists, rp))
				continue
			}
			if err := recheckFile(k); err != nil {
				return stop(k.rel, err)
			}
			rel := k.rel
			var cb func(done, total int64)
			if progress != nil {
				cb = func(done, total int64) { progress(rel, done, total) }
			}
			f, err := os.Open(k.local)
			if err != nil {
				return stop(k.rel, fmt.Errorf("%w: open %s: %v", ErrFileChanged, k.rel, err))
			}
			err = c.uploadOpened(ctx, f, k.local, j.remotePath, j.remoteID, k.info, cb)
			_ = f.Close()
			if err != nil {
				// A pre-existing name found by UploadProgress itself happens
				// before any remote change; treat it as a conflict.
				if errors.Is(err, ErrExists) && !errors.Is(err, ErrUnconfirmed) {
					conflict(rp, err)
					continue
				}
				return stop(k.rel, err)
			}
			res.Uploaded++
			// Post-commit check: the remote file exists, but if the local
			// file was swapped around the upload we cannot vouch for it.
			if err := recheckFile(k); err != nil {
				return stop(k.rel, fmt.Errorf("%w: remote %s was already committed and may not match the planned file: %w", ErrUnconfirmed, rp, err))
			}
		}
	}
	if res.Conflicts > 0 {
		res.Err = res.FirstConflictErr
		return res
	}
	return nil
}

func mustSplit(p string) []string {
	parts, _ := splitPath(p) // already validated by resolveDir
	return parts
}

// ensureRemoteDir returns the ID of directory name inside parentID, creating
// it with mkdirIn if absent. ok=false means the name is taken by a file.
func (c *Client) ensureRemoteDir(ctx context.Context, parentID, parentPath, name string) (id string, created, ok bool, err error) {
	p := joinPath(parentPath, name)
	check := func() (string, bool, bool, error) {
		m, err := c.children(ctx, parentID, name)
		if err != nil {
			return "", false, false, err
		}
		switch {
		case len(m) == 0:
			return "", false, false, nil
		case len(m) > 1:
			return "", true, false, fmt.Errorf("%w: %s", ErrAmbiguous, p)
		case m[0].IsDir:
			return m[0].ID, true, true, nil
		default:
			return "", true, false, nil
		}
	}
	id, found, isDir, err := check()
	if err != nil {
		return "", false, false, err
	}
	if found {
		return id, false, isDir, nil
	}
	f, err := c.mkdirIn(ctx, parentID, parentPath, name)
	if err == nil {
		if f.Name != name || !f.IsDir || f.ID == "" {
			return "", false, false, fmt.Errorf("%w: mkdir %s returned unexpected entry", ErrUnconfirmed, p)
		}
		return f.ID, true, true, nil
	}
	if errors.Is(err, ErrExists) && !errors.Is(err, ErrUnconfirmed) {
		// Lost a race with a concurrent creator: merge if it is a directory.
		id, found, isDir, err2 := check()
		if err2 != nil {
			return "", false, false, err2
		}
		if found {
			return id, false, isDir, nil
		}
	}
	return "", false, false, err
}

// planUploadTree walks the local tree without following symlinks.
func planUploadTree(ctx context.Context, localPath string) (*upNode, error) {
	if strings.TrimSpace(localPath) == "" {
		return nil, fmt.Errorf("%w: empty local path", ErrUnsafeLocalPath)
	}
	abs, err := filepath.Abs(localPath)
	if err != nil {
		return nil, fmt.Errorf("quark: upload dir: %w", err)
	}
	abs = filepath.Clean(abs)
	st, err := os.Lstat(abs)
	if err != nil {
		return nil, fmt.Errorf("quark: upload dir: %w", err)
	}
	if st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
		return nil, fmt.Errorf("%w: not a plain directory: %s", ErrUnsafeLocalPath, abs)
	}
	name := filepath.Base(abs)
	if err := uploadName(name); err != nil {
		return nil, err
	}
	root := &upNode{local: abs, isDir: true, info: st}
	count := 0
	queue := []*upNode{root}
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		d := queue[0]
		queue = queue[1:]
		ents, err := os.ReadDir(d.local)
		if err != nil {
			return nil, fmt.Errorf("quark: upload dir: %w", err)
		}
		for _, e := range ents {
			n := e.Name()
			if err := uploadName(n); err != nil {
				return nil, fmt.Errorf("%w (at %s)", err, displayRel(relJoin(d.rel, n)))
			}
			count++
			if count > upTreeMaxItems {
				return nil, fmt.Errorf("%w: more than %d entries", ErrUnsupported, upTreeMaxItems)
			}
			lp := filepath.Join(d.local, n)
			if filepath.Dir(lp) != d.local {
				return nil, fmt.Errorf("%w: %s", ErrUnsafeLocalPath, lp)
			}
			fi, err := os.Lstat(lp)
			if err != nil {
				return nil, fmt.Errorf("quark: upload dir: %w", err)
			}
			rel := relJoin(d.rel, n)
			k := &upNode{rel: rel, local: lp, info: fi, depth: d.depth + 1, par: d}
			mode := fi.Mode()
			switch {
			case mode&os.ModeSymlink != 0:
				return nil, fmt.Errorf("%w: symlink %s", ErrUnsafeLocalPath, rel)
			case mode.IsDir():
				if k.depth > upTreeMaxDepth {
					return nil, fmt.Errorf("%w: deeper than %d levels at %s", ErrUnsupported, upTreeMaxDepth, rel)
				}
				k.isDir = true
				queue = append(queue, k)
			case mode.IsRegular():
				if fi.Size() == 0 {
					return nil, fmt.Errorf("%w: empty file %s", ErrUnsupported, rel)
				}
			default:
				return nil, fmt.Errorf("%w: not a regular file: %s", ErrUnsupported, rel)
			}
			d.kids = append(d.kids, k)
			d.names = append(d.names, n)
		}
		sort.Strings(d.names)
	}
	return root, nil
}

// uploadName applies validName plus restrictions needed because uploads go
// through path-based remote resolution (splitPath trims and cleans paths).
func uploadName(n string) error {
	if err := validName(n); err != nil {
		return err
	}
	if strings.TrimSpace(n) != n {
		return fmt.Errorf("%w: %q has leading/trailing whitespace", ErrInvalidName, n)
	}
	return nil
}

func relJoin(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

func sameLocal(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.Mode() == b.Mode() && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

// recheckDir verifies a planned directory is still the same directory with
// the same set of names.
func recheckDir(n *upNode) error {
	st, err := os.Lstat(n.local)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrFileChanged, displayRel(n.rel), err)
	}
	if st.Mode()&os.ModeSymlink != 0 || !st.IsDir() || !os.SameFile(st, n.info) {
		return fmt.Errorf("%w: directory %s was replaced", ErrFileChanged, displayRel(n.rel))
	}
	ents, err := os.ReadDir(n.local)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrFileChanged, displayRel(n.rel), err)
	}
	if len(ents) != len(n.names) {
		return fmt.Errorf("%w: directory %s contents changed", ErrFileChanged, displayRel(n.rel))
	}
	for i, e := range ents { // ReadDir returns sorted names
		if e.Name() != n.names[i] {
			return fmt.Errorf("%w: directory %s contents changed", ErrFileChanged, displayRel(n.rel))
		}
	}
	return nil
}

// recheckFile verifies a planned file and its parent directory are unchanged.
func recheckFile(n *upNode) error {
	pst, err := os.Lstat(filepath.Dir(n.local))
	if err != nil || pst.Mode()&os.ModeSymlink != 0 || !pst.IsDir() || (n.par != nil && !os.SameFile(pst, n.par.info)) {
		return fmt.Errorf("%w: parent of %s changed", ErrFileChanged, n.rel)
	}
	st, err := os.Lstat(n.local)
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrFileChanged, n.rel, err)
	}
	if !st.Mode().IsRegular() || !sameLocal(st, n.info) {
		return fmt.Errorf("%w: %s", ErrFileChanged, n.rel)
	}
	return nil
}
