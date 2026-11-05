package quark

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Resumable download state lives beside the final file:
//
//	.<name>.qdrive-part  partial bytes, always a prefix of the remote content
//	.<name>.qdrive-meta  JSON identity of the remote content behind the partial
//	.<name>.qdrive-lock  exclusive marker held while a download is running
//
// The partial is only ever extended when the metadata matches the current
// remote file ID and size; anything doubtful is reported, never deleted.
const (
	partSuffix         = ".qdrive-part"
	metaSuffix         = ".qdrive-meta"
	lockSuffix         = ".qdrive-lock"
	metaVersion        = 1
	copyBufBytes       = 256 << 10
	downloadMaxRetries = 3
	downloadRetryDelay = time.Second
)

var (
	// ErrLocalExists reports that the final local file already exists.
	ErrLocalExists = errors.New("quark: local file already exists")
	// ErrPartialMismatch reports resume state that does not belong to the
	// requested remote file; it is left untouched for the user to inspect.
	ErrPartialMismatch = errors.New("quark: partial download does not match remote file")
	// ErrRangeUnsupported reports that the server did not honour a Range
	// request; the partial file is kept.
	ErrRangeUnsupported = errors.New("quark: server refused range request")
	// ErrLocked reports another (or a crashed) download of the same target.
	ErrLocked = errors.New("quark: download already in progress (remove stale lock file if not)")
)

type downloadMeta struct {
	Version          int    `json:"version"`
	FID              string `json:"fid"`
	Size             int64  `json:"size"`
	Remote           string `json:"remote"`
	Target           string `json:"target"`
	ETag             string `json:"etag,omitempty"` // strong validator only
	ResponseComplete bool   `json:"response_complete,omitempty"`
}

func (m downloadMeta) sameContent(o downloadMeta) bool {
	return m.Version == o.Version && m.FID == o.FID && m.Size == o.Size && m.Target == o.Target
}

type downloadPaths struct{ target, part, meta, lock string }

func newDownloadPaths(localPath string) (downloadPaths, error) {
	if strings.TrimSpace(localPath) == "" {
		return downloadPaths{}, errors.New("quark: empty local path")
	}
	abs, err := filepath.Abs(localPath)
	if err != nil {
		return downloadPaths{}, fmt.Errorf("quark: local path: %w", err)
	}
	dir, base := filepath.Split(abs)
	if base == "" || base == "." || base == ".." {
		return downloadPaths{}, fmt.Errorf("quark: invalid local file name %q", localPath)
	}
	hidden := filepath.Join(dir, "."+base)
	return downloadPaths{target: abs, part: hidden + partSuffix, meta: hidden + metaSuffix, lock: hidden + lockSuffix}, nil
}

// DownloadFile downloads the regular file at remotePath to localPath,
// resuming a previous partial download of the same remote content when
// possible. It never overwrites an existing localPath. On error or
// cancellation the partial data is kept for a later call.
func (c *Client) DownloadFile(ctx context.Context, remotePath, localPath string) error {
	return c.DownloadFileProgress(ctx, remotePath, localPath, nil)
}

// DownloadFileProgress is DownloadFile with an optional progress callback.
// progress (may be nil) receives the bytes held locally (including a resumed
// prefix) and the remote size. It is called synchronously from the download
// goroutine, so it must be quick and must not call back into the Client.
func (c *Client) DownloadFileProgress(ctx context.Context, remotePath, localPath string, progress func(done, total int64)) error {
	p, err := newDownloadPaths(localPath)
	if err != nil {
		return err
	}
	if fi, err := os.Stat(filepath.Dir(p.target)); err != nil {
		return fmt.Errorf("quark: local directory: %w", err)
	} else if !fi.IsDir() {
		return fmt.Errorf("quark: local parent is not a directory: %s", filepath.Dir(p.target))
	}
	if err := checkAbsent(p.target); err != nil {
		return err
	}

	f, err := c.resolve(ctx, remotePath)
	if err != nil {
		return err
	}
	return c.downloadResolved(ctx, f, remotePath, p, progress)
}

// downloadResolved downloads an already resolved remote file to p. The
// caller has validated p's parent directory.
func (c *Client) downloadResolved(ctx context.Context, f File, remotePath string, p downloadPaths, progress func(done, total int64)) error {
	if f.IsDir {
		return fmt.Errorf("%w: %s", ErrIsDir, remotePath)
	}
	if f.ID == "" {
		return fmt.Errorf("quark: download: remote file without id: %s", remotePath)
	}
	if f.Size < 0 {
		return errors.New("quark: download: remote size unknown")
	}

	unlock, err := acquireLock(p.lock)
	if err != nil {
		return err
	}
	defer unlock()
	// Re-check after locking: a concurrent run may have just finished.
	if err := checkAbsent(p.target); err != nil {
		return err
	}

	want := downloadMeta{Version: metaVersion, FID: f.ID, Size: f.Size, Remote: remotePath, Target: p.target}
	meta, offset, err := loadOrInitState(p, want)
	if err != nil {
		return err
	}

	if f.Size > 0 && ((offset == f.Size && !meta.ResponseComplete) || (offset < f.Size && meta.ResponseComplete)) {
		return fmt.Errorf("%w: partial size and completed response marker disagree; inspect and move both partial and metadata aside before retrying (data kept)", ErrPartialMismatch)
	}
	if progress != nil {
		progress(offset, f.Size)
	}
	for attempt := 0; offset < f.Size; attempt++ {
		err := c.fetchRange(ctx, f, p, offset, &meta, progress)
		if err == nil {
			meta.ResponseComplete = true
			if err := writeMeta(p.meta, meta); err != nil {
				return err
			}
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt >= downloadMaxRetries || !isTransientNetErr(err) {
			return err
		}
		// Keep the per-file lock while rechecking the partial and its metadata.
		meta, offset, err = loadOrInitState(p, want)
		if err != nil {
			return err
		}
		if offset == f.Size && !meta.ResponseComplete {
			return fmt.Errorf("%w: response ended ambiguously after all bytes were written; inspect and move both partial and metadata aside before retrying (data kept)", ErrPartialMismatch)
		}
		if offset > 0 && meta.ETag == "" {
			return fmt.Errorf("%w: no strong ETag for the partial file; refusing an unsafe resume", ErrRangeUnsupported)
		}
		if err := sleepCtx(ctx, downloadRetryDelay<<attempt); err != nil {
			return err
		}
	}
	return finalize(p, f.Size, meta)
}

func checkAbsent(name string) error {
	if _, err := os.Lstat(name); err == nil {
		return fmt.Errorf("%w: %s", ErrLocalExists, name)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("quark: stat local file: %w", err)
	}
	return nil
}

func acquireLock(name string) (func(), error) {
	lf, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("%w: %s", ErrLocked, name)
		}
		return nil, fmt.Errorf("quark: create lock: %w", err)
	}
	_, _ = fmt.Fprintf(lf, "%d\n", os.Getpid())
	_ = lf.Close()
	return func() { _ = os.Remove(name) }, nil
}

// loadOrInitState returns the metadata and the number of bytes already held
// in the partial file. It refuses to adopt a partial without matching
// metadata and never deletes user data.
func loadOrInitState(p downloadPaths, want downloadMeta) (downloadMeta, int64, error) {
	raw, _, merr := readDownloadMeta(p.meta)
	pfi, perr := os.Lstat(p.part)
	if perr != nil && !errors.Is(perr, os.ErrNotExist) {
		return want, 0, fmt.Errorf("quark: stat partial: %w", perr)
	}
	if perr == nil && !pfi.Mode().IsRegular() {
		return want, 0, fmt.Errorf("%w: %s is not a regular file", ErrPartialMismatch, p.part)
	}

	switch {
	case errors.Is(merr, os.ErrNotExist):
		if perr == nil {
			return want, 0, fmt.Errorf("%w: %s has no metadata", ErrPartialMismatch, p.part)
		}
		// Fresh start: metadata first, then an exclusively created partial.
		if err := createDownloadMeta(p.meta, want); err != nil {
			return want, 0, err
		}
		pf, err := os.OpenFile(p.part, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return want, 0, fmt.Errorf("quark: create partial: %w", err)
		}
		if err := pf.Close(); err != nil {
			return want, 0, fmt.Errorf("quark: create partial: %w", err)
		}
		syncDir(filepath.Dir(p.part))
		return want, 0, nil
	case merr != nil:
		return want, 0, fmt.Errorf("quark: read partial metadata: %w", merr)
	}

	var got downloadMeta
	if err := json.Unmarshal(raw, &got); err != nil {
		return want, 0, fmt.Errorf("%w: unreadable metadata %s", ErrPartialMismatch, p.meta)
	}
	if !got.sameContent(want) {
		return want, 0, fmt.Errorf("%w: %s belongs to fid=%s size=%d, remote is fid=%s size=%d",
			ErrPartialMismatch, p.part, got.FID, got.Size, want.FID, want.Size)
	}
	want.ETag = got.ETag
	want.ResponseComplete = got.ResponseComplete
	if perr != nil {
		if want.ResponseComplete {
			return want, 0, fmt.Errorf("%w: completed response has no partial file", ErrPartialMismatch)
		}
		// Metadata without data (e.g. crash between the two writes).
		pf, err := os.OpenFile(p.part, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return want, 0, fmt.Errorf("quark: create partial: %w", err)
		}
		if err := pf.Close(); err != nil {
			return want, 0, fmt.Errorf("quark: create partial: %w", err)
		}
		syncDir(filepath.Dir(p.part))
		return want, 0, nil
	}
	if pfi.Size() > want.Size {
		return want, 0, fmt.Errorf("%w: partial larger than remote (%d > %d)", ErrPartialMismatch, pfi.Size(), want.Size)
	}
	return want, pfi.Size(), nil
}

// readDownloadMeta refuses symbolic links and changes between Lstat and Open.
// The bounded read prevents an unrelated large file from being loaded as state.
func readDownloadMeta(name string) ([]byte, os.FileInfo, error) {
	before, err := os.Lstat(name)
	if err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%w: metadata is not a regular file: %s", ErrPartialMismatch, name)
	}
	f, err := os.Open(name)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return nil, nil, fmt.Errorf("%w: metadata changed while opening", ErrPartialMismatch)
	}
	b, err := io.ReadAll(io.LimitReader(f, maxJSONBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if len(b) > maxJSONBytes {
		return nil, nil, fmt.Errorf("%w: metadata is too large", ErrPartialMismatch)
	}
	return b, after, nil
}

// createDownloadMeta publishes complete metadata without replacing a file
// that appeared after the initial check, using the same link strategy as the
// final download. The temporary file is always created exclusively.
func createDownloadMeta(name string, m downloadMeta) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(name), filepath.Base(name)+".tmp-*")
	if err != nil {
		return fmt.Errorf("quark: create metadata: %w", err)
	}
	tmpName := f.Name()
	defer os.Remove(tmpName)
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return fmt.Errorf("quark: create metadata: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("quark: sync metadata: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("quark: close metadata: %w", err)
	}
	if err := os.Link(tmpName, name); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w: metadata appeared during creation: %s", ErrPartialMismatch, name)
		}
		return fmt.Errorf("quark: publish metadata: %w", err)
	}
	syncDir(filepath.Dir(name))
	return nil
}

func writeMeta(name string, m downloadMeta) error {
	raw, before, err := readDownloadMeta(name)
	if err != nil {
		return fmt.Errorf("quark: read metadata before update: %w", err)
	}
	var old downloadMeta
	if err := json.Unmarshal(raw, &old); err != nil || !old.sameContent(m) {
		return fmt.Errorf("%w: metadata changed before update", ErrPartialMismatch)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(name), filepath.Base(name)+".tmp-*")
	if err != nil {
		return fmt.Errorf("quark: write metadata: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("quark: write metadata: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("quark: write metadata: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("quark: write metadata: %w", err)
	}
	current, err := os.Lstat(name)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(before, current) {
		return fmt.Errorf("%w: metadata changed before replace", ErrPartialMismatch)
	}
	if err := os.Rename(tmpName, name); err != nil {
		return fmt.Errorf("quark: write metadata: %w", err)
	}
	syncDir(filepath.Dir(name))
	return nil
}

// fetchRange downloads bytes [offset, f.Size) into the partial file. On a
// fresh start it records the response's strong ETag (before any body bytes
// are written) so later resumes can send If-Range.
func (c *Client) fetchRange(ctx context.Context, f File, p downloadPaths, offset int64, meta *downloadMeta, progress func(done, total int64)) error {
	link, err := c.downloadURL(ctx, f.ID)
	if err != nil {
		return err
	}
	u, err := url.Parse(link)
	if err != nil || !c.trusted(u) {
		return ErrUntrustedURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return errors.New("quark: download: invalid request") // avoid echoing signed url
	}
	c.setHeaders(req)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Encoding", "identity") // byte offsets must be raw
	if offset > 0 {
		if meta.ETag == "" {
			return fmt.Errorf("%w: no strong ETag for the partial file; refusing an unsafe resume", ErrRangeUnsupported)
		}
		req.Header.Set("Range", "bytes="+strconv.FormatInt(offset, 10)+"-")
		req.Header.Set("If-Range", meta.ETag)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("quark: download: %w", scrub(err))
	}
	defer resp.Body.Close()

	remaining := f.Size - offset
	switch {
	case offset == 0 && resp.StatusCode == http.StatusOK:
		if resp.ContentLength >= 0 && resp.ContentLength != f.Size {
			return fmt.Errorf("quark: download: content-length %d, remote size %d", resp.ContentLength, f.Size)
		}
	case offset > 0 && resp.StatusCode == http.StatusPartialContent:
		if etag := strings.TrimSpace(resp.Header.Get("ETag")); etag != "" && etag != meta.ETag {
			return fmt.Errorf("%w: response validator changed; partial kept", ErrPartialMismatch)
		}
		if err := checkContentRange(resp.Header.Get("Content-Range"), offset, f.Size); err != nil {
			return err
		}
		if resp.ContentLength >= 0 && resp.ContentLength != remaining {
			return fmt.Errorf("quark: download: content-length %d, expected %d", resp.ContentLength, remaining)
		}
	case offset > 0 && (resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusRequestedRangeNotSatisfiable):
		// Full body (range ignored or If-Range validator changed) or range
		// rejected. Appending would corrupt data; keep the partial.
		return fmt.Errorf("%w: http %d at offset %d (partial kept)", ErrRangeUnsupported, resp.StatusCode, offset)
	case resp.StatusCode == http.StatusPartialContent:
		return errors.New("quark: download: unexpected partial content")
	default:
		return httpErr("download", resp)
	}

	if offset == 0 {
		if et := strongETag(resp.Header.Get("ETag")); et != meta.ETag {
			meta.ETag = et
			// Persist the validator before writing bytes, so later resumes do not
			// accidentally use a stale or missing value.
			if err := writeMeta(p.meta, *meta); err != nil {
				return err
			}
		}
	}
	var onWrite func(int64)
	if progress != nil {
		onWrite = func(held int64) { progress(min(held, f.Size), f.Size) }
	}
	return appendBody(p.part, offset, remaining, resp.Body, onWrite)
}

// progressWriter reports the absolute partial size after each write.
type progressWriter struct {
	w    io.Writer
	held int64
	fn   func(int64)
}

func (pw *progressWriter) Write(b []byte) (int, error) {
	n, err := pw.w.Write(b)
	if n > 0 {
		pw.held += int64(n)
		pw.fn(pw.held)
	}
	return n, err
}

func strongETag(v string) string {
	v = strings.TrimSpace(v)
	if strings.HasPrefix(v, `"`) && strings.HasSuffix(v, `"`) && len(v) >= 2 && len(v) <= 256 {
		return v
	}
	return ""
}

// checkContentRange requires exactly "bytes <offset>-<size-1>/<size>".
func checkContentRange(v string, offset, size int64) error {
	bad := fmt.Errorf("quark: download: invalid Content-Range %q", v)
	unit, rest, ok := strings.Cut(strings.TrimSpace(v), " ")
	if !ok || !strings.EqualFold(unit, "bytes") {
		return bad
	}
	rng, total, ok := strings.Cut(strings.TrimSpace(rest), "/")
	if !ok {
		return bad
	}
	s, e, ok := strings.Cut(rng, "-")
	if !ok {
		return bad
	}
	start, err1 := strconv.ParseInt(s, 10, 64)
	end, err2 := strconv.ParseInt(e, 10, 64)
	if err1 != nil || err2 != nil {
		return bad
	}
	if start != offset || end != size-1 {
		return fmt.Errorf("quark: download: Content-Range %d-%d, expected %d-%d", start, end, offset, size-1)
	}
	if total != "*" {
		t, err := strconv.ParseInt(total, 10, 64)
		if err != nil || t != size {
			return fmt.Errorf("quark: download: Content-Range total %q, expected %d", total, size)
		}
	}
	return nil
}

// appendBody writes at most n bytes of body at offset. Every byte written is
// a correct prefix extension, so on error/cancel the partial stays usable.
func appendBody(part string, offset, n int64, body io.Reader, onWrite func(held int64)) (err error) {
	before, err := os.Lstat(part)
	if err != nil || !before.Mode().IsRegular() {
		return fmt.Errorf("%w: partial is missing or not a regular file", ErrPartialMismatch)
	}
	pf, err := os.OpenFile(part, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("quark: open partial: %w", err)
	}
	defer func() {
		if serr := pf.Sync(); err == nil && serr != nil {
			err = fmt.Errorf("quark: sync partial: %w", serr)
		}
		if cerr := pf.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("quark: close partial: %w", cerr)
		}
	}()
	fi, err := pf.Stat()
	if err != nil || !fi.Mode().IsRegular() || !os.SameFile(before, fi) || fi.Size() != offset {
		return fmt.Errorf("%w: partial changed during download", ErrPartialMismatch)
	}
	if _, err := pf.Seek(offset, io.SeekStart); err != nil {
		return fmt.Errorf("quark: seek partial: %w", err)
	}
	// Read one extra byte to detect an over-long body.
	var dst io.Writer = pf
	if onWrite != nil {
		// Wrapping also hides *os.File's ReadFrom, keeping the buffer in use.
		dst = &progressWriter{w: pf, held: offset, fn: onWrite}
	}
	w, err := io.CopyBuffer(dst, io.LimitReader(body, n+1), make([]byte, copyBufBytes))
	if w > n {
		// The response is invalid; discard all bytes from this response so a
		// later run cannot mistake an overlong body for a complete file.
		if terr := pf.Truncate(offset); terr != nil {
			return fmt.Errorf("quark: truncate partial: %w", terr)
		}
		return fmt.Errorf("quark: download: body longer than expected %d", n)
	}
	if err != nil {
		return fmt.Errorf("quark: download: %w (partial kept at %d bytes)", scrub(err), offset+w)
	}
	if w != n {
		return fmt.Errorf("quark: download: short body %d/%d (partial kept): %w", offset+w, offset+n, io.ErrUnexpectedEOF)
	}
	return nil
}

// finalize verifies the partial size and publishes it under the target name
// without ever replacing an existing file (hard link fails on EEXIST).
func finalize(p downloadPaths, size int64, meta downloadMeta) error {
	// Windows requires a writable handle for Sync (FlushFileBuffers).
	before, err := os.Lstat(p.part)
	if err != nil || !before.Mode().IsRegular() {
		return fmt.Errorf("%w: partial is missing or not a regular file", ErrPartialMismatch)
	}
	pf, err := os.OpenFile(p.part, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("quark: open partial: %w", err)
	}
	fi, err := pf.Stat()
	if err == nil {
		err = pf.Sync()
	}
	_ = pf.Close()
	if err != nil {
		return fmt.Errorf("quark: verify partial: %w", err)
	}
	if !fi.Mode().IsRegular() || !os.SameFile(before, fi) || fi.Size() != size {
		return fmt.Errorf("%w: partial changed or size mismatch %d/%d (partial kept)", ErrPartialMismatch, fi.Size(), size)
	}
	current, err := os.Lstat(p.part)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(fi, current) {
		return fmt.Errorf("%w: partial changed before install", ErrPartialMismatch)
	}
	if err := os.Link(p.part, p.target); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("%w: %s", ErrLocalExists, p.target)
		}
		return fmt.Errorf("quark: install %s (partial kept at %s): %w", p.target, p.part, err)
	}
	linked, err := os.Lstat(p.target)
	if err != nil || !linked.Mode().IsRegular() || !os.SameFile(fi, linked) || linked.Size() != size {
		return fmt.Errorf("%w: installed target changed; target and partial kept for inspection", ErrPartialMismatch)
	}
	syncDir(filepath.Dir(p.target))
	// Only clean up the identities we just validated. Unexpected files are
	// preserved for inspection rather than treated as our own leftovers.
	if current, err := os.Lstat(p.part); err == nil && current.Mode().IsRegular() && os.SameFile(fi, current) {
		_ = os.Remove(p.part)
	}
	if raw, original, err := readDownloadMeta(p.meta); err == nil {
		var got downloadMeta
		if json.Unmarshal(raw, &got) == nil && got.sameContent(meta) && got.ETag == meta.ETag && got.ResponseComplete == meta.ResponseComplete {
			if current, err := os.Lstat(p.meta); err == nil && current.Mode().IsRegular() && os.SameFile(original, current) {
				_ = os.Remove(p.meta)
			}
		}
	}
	syncDir(filepath.Dir(p.target))
	return nil
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}
