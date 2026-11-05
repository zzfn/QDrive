package quark

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
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
//     is a real directory, then download files through the same resumable,
//     never-overwriting path as DownloadFile.
//
// Nothing local is ever deleted or replaced. Existing final files are
// reported as conflicts rather than assumed to match the remote content.
const (
	treeMaxItems        = 100000
	treeMaxDepth        = 64
	maxDirectoryWorkers = 16
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

// DirDownloadError reports the first failure, how many items failed and how
// many files did not start. Completed contains files completed during this
// call, relative to the root. Existing files are never assumed to match.
type DirDownloadError struct {
	Completed   []string
	Failed      string
	FailedCount int
	NotStarted  int
	Err         error
}

func (e *DirDownloadError) Error() string {
	if e.Failed == "" {
		return fmt.Sprintf("quark: download dir: %d file(s) completed, %d item(s) failed, %d file(s) not started: %v", len(e.Completed), e.FailedCount, e.NotStarted, e.Err)
	}
	return fmt.Sprintf("quark: download dir: %d file(s) completed, %d item(s) failed, %d file(s) not started; first failure at %q: %v", len(e.Completed), e.FailedCount, e.NotStarted, e.Failed, e.Err)
}

func (e *DirDownloadError) Unwrap() error { return e.Err }

type treeItem struct {
	f   File
	rel string // slash-separated, relative to the download root
}

// DownloadDir downloads files one at a time. See DownloadDirWorkers for
// directory and conflict handling.
func (c *Client) DownloadDir(ctx context.Context, remotePath, localPath string, progress func(path string, done, total int64)) error {
	return c.DownloadDirWorkers(ctx, remotePath, localPath, 1, progress)
}

// DownloadDirWorkers downloads a remote directory using up to workers (1-16)
// file downloads at once. The destination is the directory itself; its parent
// must already exist. Existing files are not overwritten or counted as done:
// they are reported as failures while other files can continue. Other errors
// stop new downloads and cancel those in flight. Individual partial downloads
// are retained and may be resumed on a later invocation. Progress callbacks
// are serialized across workers and must return quickly.
func (c *Client) DownloadDirWorkers(ctx context.Context, remotePath, localPath string, workers int, progress func(path string, done, total int64)) error {
	if workers < 1 || workers > maxDirectoryWorkers {
		return fmt.Errorf("quark: directory download workers must be between 1 and %d", maxDirectoryWorkers)
	}
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

	var files []treeItem
	for _, it := range items {
		if !it.f.IsDir {
			files = append(files, it)
		}
	}
	for _, it := range items {
		if err := ctx.Err(); err != nil {
			return &DirDownloadError{NotStarted: len(files), Err: err}
		}
		if !it.f.IsDir {
			continue
		}
		local, err := localUnder(root, it.rel)
		if err == nil {
			err = makeDirOnce(local)
		}
		if err != nil {
			return &DirDownloadError{Failed: it.rel, FailedCount: 1, NotStarted: len(files), Err: err}
		}
	}
	if len(files) == 0 {
		return nil
	}

	type result struct {
		rel        string
		err        error
		notStarted bool
	}
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan treeItem)
	results := make(chan result, workers)
	var wg sync.WaitGroup
	var progressMu sync.Mutex
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range jobs {
				if err := workCtx.Err(); err != nil {
					results <- result{rel: it.rel, err: err, notStarted: true}
					continue
				}
				local, err := localUnder(root, it.rel)
				if err == nil {
					var paths downloadPaths
					paths, err = newDownloadPaths(local)
					if err == nil {
						var cb func(int64, int64)
						if progress != nil {
							rel := it.rel
							cb = func(done, total int64) {
								progressMu.Lock()
								defer progressMu.Unlock()
								progress(rel, done, total)
							}
						}
						err = c.downloadResolved(workCtx, it.f, joinPath(remotePath, it.rel), paths, cb)
					}
				}
				results <- result{rel: it.rel, err: err}
				if err != nil && !errors.Is(err, ErrLocalExists) {
					cancel()
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, it := range files {
			if workCtx.Err() != nil {
				return
			}
			select {
			case <-workCtx.Done():
				return
			case jobs <- it:
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()

	var completed []string
	var first *DirDownloadError
	for r := range results {
		if r.notStarted {
			continue
		}
		if r.err == nil {
			completed = append(completed, r.rel)
		} else if first == nil {
			first = &DirDownloadError{Failed: r.rel, FailedCount: 1, Err: r.err}
		} else {
			first.FailedCount++
			// A conflict is expected on reruns; don't let it hide a separate
			// network, filesystem or cancellation failure.
			if errors.Is(first.Err, ErrLocalExists) && !errors.Is(r.err, ErrLocalExists) {
				first.Failed, first.Err = r.rel, r.err
			}
		}
	}
	failed := 0
	if first != nil {
		failed = first.FailedCount
	}
	notStarted := len(files) - len(completed) - failed
	if ctx.Err() != nil && notStarted > 0 && (first == nil || errors.Is(first.Err, ErrLocalExists)) {
		if first == nil {
			first = &DirDownloadError{}
		}
		first.Failed, first.Err = "", ctx.Err()
	}
	if first != nil {
		sort.Strings(completed)
		first.Completed = completed
		first.NotStarted = notStarted
		return first
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
