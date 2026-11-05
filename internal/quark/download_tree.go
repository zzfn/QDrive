package quark

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Directory downloads run in two phases:
//
//  1. plan: walk the remote tree breadth-first by file ID (never by path, so
//     duplicate names cannot redirect the walk), validating every name the
//     server returns before it is ever turned into a local path;
//  2. fetch: create directories one level at a time with os.Mkdir (never
//     MkdirAll), re-checking with Lstat that every component below the root
//     is a real directory, then download files one at a time through the same
//     resumable, never-overwriting path as DownloadFile.
//
// Nothing local is ever deleted or replaced. A rerun stops on any existing
// final file rather than assuming equal size proves equal content.
const (
	treeMaxItems = 100000
	treeMaxDepth = 64
)

var (
	// ErrUnsafeRemoteName reports a server-provided name that cannot be used
	// as a single local path component.
	ErrUnsafeRemoteName = errors.New("quark: unsafe remote file name")
	// ErrRemoteTree reports a remote listing that loops, collides or is too big.
	ErrRemoteTree = errors.New("quark: unusable remote directory tree")
	// ErrUnsafeLocalPath reports a local path component that is not a plain
	// directory (e.g. a symlink) inside the download root.
	ErrUnsafeLocalPath = errors.New("quark: unsafe local path")
)

// DirDownloadError describes where a directory download stopped. Completed
// lists files (slash-separated, relative to the root) finished in this call;
// Failed is the file or directory being handled when Err occurred. Partial
// data is kept. A rerun stops at any already completed local file.
type DirDownloadError struct {
	Completed []string
	Failed    string
	Err       error
}

func (e *DirDownloadError) Error() string {
	return fmt.Sprintf("quark: download dir: %d file(s) completed, stopped at %q: %v", len(e.Completed), e.Failed, e.Err)
}

func (e *DirDownloadError) Unwrap() error { return e.Err }

type treeItem struct {
	f   File
	rel string // slash-separated, relative to the download root
}

// DownloadDir downloads the remote directory remotePath into localPath,
// which is the destination directory itself (its contents mirror the remote
// directory's contents). localPath must either not exist, in which case it is
// created (its parent must already exist), or be an existing real directory
// (not a symlink). Files are fetched one at a time.
//
// An existing local file is never overwritten or assumed to match the remote
// content: any existing file stops the download with ErrLocalExists.
//
// progress (may be nil) receives the slash-separated relative path of the
// current file with its bytes held and total size.
func (c *Client) DownloadDir(ctx context.Context, remotePath, localPath string, progress func(path string, done, total int64)) error {
	if strings.TrimSpace(localPath) == "" {
		return errors.New("quark: empty local path")
	}
	root, err := filepath.Abs(localPath)
	if err != nil {
		return fmt.Errorf("quark: local path: %w", err)
	}
	top, err := c.resolve(ctx, remotePath)
	if err != nil {
		return err
	}
	if !top.IsDir {
		return fmt.Errorf("%w: %s", ErrNotDir, remotePath)
	}
	items, err := c.planTree(ctx, top)
	if err != nil {
		return err
	}
	if err := prepareRoot(root); err != nil {
		return err
	}

	var done []string
	fail := func(rel string, err error) error {
		return &DirDownloadError{Completed: done, Failed: rel, Err: err}
	}
	for _, it := range items {
		if err := ctx.Err(); err != nil {
			return fail(it.rel, err)
		}
		local, err := localUnder(root, it.rel)
		if err != nil {
			return fail(it.rel, err)
		}
		if it.f.IsDir {
			if err := makeDirOnce(local); err != nil {
				return fail(it.rel, err)
			}
			continue
		}
		var cb func(int64, int64)
		if progress != nil {
			rel := it.rel
			cb = func(d, t int64) { progress(rel, d, t) }
		}
		p, err := newDownloadPaths(local)
		if err != nil {
			return fail(it.rel, err)
		}
		if err := c.downloadResolved(ctx, it.f, joinPath(remotePath, it.rel), p, cb); err != nil {
			return fail(it.rel, err)
		}
		done = append(done, it.rel)
	}
	return nil
}

// planTree lists the whole tree under top. Parents always precede children.
func (c *Client) planTree(ctx context.Context, top File) ([]treeItem, error) {
	type dirJob struct {
		f     File
		rel   string
		depth int
	}
	seenDirs := map[string]bool{top.ID: true} // any revisit means a cycle or alias
	seenFiles := map[string]string{}
	var items []treeItem
	queue := []dirJob{{f: top}}
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		d := queue[0]
		queue = queue[1:]
		entries, err := c.listDir(ctx, d.f.ID)
		if err != nil {
			return nil, fmt.Errorf("quark: list %q: %w", displayRel(d.rel), err)
		}
		// Names local filesystems may treat as equal (case-folding). Names that
		// look like our resume state are already refused by checkRemoteName.
		claimed := map[string]string{}
		for _, e := range entries {
			if err := checkRemoteName(e.Name); err != nil {
				return nil, fmt.Errorf("%w (in %q)", err, displayRel(d.rel))
			}
			rel := e.Name
			if d.rel != "" {
				rel = d.rel + "/" + e.Name
			}
			if e.ID == "" {
				return nil, fmt.Errorf("%w: %q has no id", ErrRemoteTree, rel)
			}
			key := strings.ToLower(e.Name)
			if other, ok := claimed[key]; ok {
				return nil, fmt.Errorf("%w: %q and %q map to the same local name", ErrRemoteTree, other, rel)
			}
			claimed[key] = rel
			if len(items) >= treeMaxItems {
				return nil, fmt.Errorf("%w: more than %d entries", ErrRemoteTree, treeMaxItems)
			}
			if e.IsDir {
				if seenDirs[e.ID] {
					return nil, fmt.Errorf("%w: directory %q appears twice (cycle?)", ErrRemoteTree, rel)
				}
				if d.depth+1 > treeMaxDepth {
					return nil, fmt.Errorf("%w: deeper than %d levels at %q", ErrRemoteTree, treeMaxDepth, rel)
				}
				seenDirs[e.ID] = true
				queue = append(queue, dirJob{f: e, rel: rel, depth: d.depth + 1})
			} else {
				if prev, ok := seenFiles[e.ID]; ok {
					return nil, fmt.Errorf("%w: file id listed at %q and %q", ErrRemoteTree, prev, rel)
				}
				seenFiles[e.ID] = rel
			}
			items = append(items, treeItem{f: e, rel: rel})
		}
	}
	return items, nil
}

// checkRemoteName accepts only a single, plain path component.
func checkRemoteName(n string) error {
	bad := func(why string) error { return fmt.Errorf("%w: %q %s", ErrUnsafeRemoteName, n, why) }
	switch {
	case n == "" || strings.TrimSpace(n) == "":
		return bad("is empty")
	case n == "." || n == "..":
		return bad("is a relative path element")
	case len(n) > maxNameBytes:
		return bad("is too long")
	case !utf8.ValidString(n):
		return bad("is not valid UTF-8")
	case strings.ContainsAny(n, "/\\"):
		return bad("contains a path separator")
	case filepath.IsAbs(n) || filepath.VolumeName(n) != "":
		return bad("is an absolute path")
	}
	for _, r := range n {
		if r < 0x20 || r == 0x7f || unicode.IsControl(r) {
			return bad("contains control characters")
		}
	}
	if runtime.GOOS == "windows" {
		if strings.ContainsAny(n, `<>:"|?*`) || strings.HasSuffix(n, ".") || strings.HasSuffix(n, " ") {
			return bad("is not a valid Windows file name")
		}
		base := strings.ToUpper(strings.TrimSpace(strings.SplitN(n, ".", 2)[0]))
		switch base {
		case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
			return bad("is a reserved Windows device name")
		}
		if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '0' && base[3] <= '9' {
			return bad("is a reserved Windows device name")
		}
	}
	// Our own resume-state files must never be a download target.
	if strings.HasPrefix(n, ".") {
		for _, sfx := range []string{partSuffix, metaSuffix, lockSuffix} {
			if strings.HasSuffix(n, sfx) {
				return bad("looks like QDrive resume state")
			}
		}
	}
	return nil
}

// prepareRoot creates root (parent must exist) or accepts an existing real
// directory. A symlink as root is refused rather than followed.
func prepareRoot(root string) error {
	fi, err := os.Lstat(root)
	switch {
	case err == nil && fi.IsDir():
		return nil
	case err == nil:
		return fmt.Errorf("%w: %s is not a directory (%s)", ErrUnsafeLocalPath, root, fi.Mode().Type())
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("quark: local root: %w", err)
	}
	pfi, err := os.Stat(filepath.Dir(root))
	if err != nil {
		return fmt.Errorf("quark: local parent: %w", err)
	}
	if !pfi.IsDir() {
		return fmt.Errorf("quark: local parent is not a directory: %s", filepath.Dir(root))
	}
	if err := os.Mkdir(root, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("quark: create %s: %w", root, err)
	}
	return makeDirOnce(root) // re-verify (another process may have raced)
}

// localUnder maps a validated relative path under root and verifies, with
// Lstat, that every existing intermediate component is a real directory, so a
// symlink planted inside the root cannot redirect writes outside it.
func localUnder(root, rel string) (string, error) {
	clean := path.Clean(rel)
	if rel == "" || clean != rel || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("%w: %q", ErrUnsafeLocalPath, rel)
	}
	target := filepath.Join(root, filepath.FromSlash(clean))
	back, err := filepath.Rel(root, target)
	if err != nil || back == "." || back == ".." || strings.HasPrefix(back, ".."+string(filepath.Separator)) || filepath.IsAbs(back) {
		return "", fmt.Errorf("%w: %q escapes %s", ErrUnsafeLocalPath, rel, root)
	}
	parts := strings.Split(clean, "/")
	cur := root
	for _, part := range parts[:len(parts)-1] {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if err != nil {
			return "", fmt.Errorf("quark: check %s: %w", cur, err)
		}
		if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("%w: %s is not a plain directory", ErrUnsafeLocalPath, cur)
		}
	}
	return target, nil
}

// makeDirOnce creates one directory level or accepts an existing real one.
func makeDirOnce(dir string) error {
	if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("quark: create %s: %w", dir, err)
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("quark: check %s: %w", dir, err)
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s exists and is not a plain directory", ErrUnsafeLocalPath, dir)
	}
	return nil
}

func displayRel(rel string) string {
	if rel == "" {
		return "/"
	}
	return rel
}
