package quark

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"sync"
)

// Parallel (chunked) download state. It shares the file names of the
// sequential state but uses metadata version 2, which the sequential
// downloader refuses: a parallel partial is sparse, so its size is not a
// completed prefix.
//
// Protocol per fresh download:
//  1. probe with "Range: bytes=0-0"; require 206, exact Content-Range with
//     numeric total == remote size and a strong ETag. Otherwise fall back to
//     the sequential downloader before any parallel state exists.
//  2. publish version 2 metadata exclusively, create the partial exclusively.
//  3. every chunk: Range + If-Range(ETag), only 206 accepted, exact range,
//     total, length and ETag checks; write, fsync, then atomically record the
//     chunk SHA-256 in the metadata.
//  4. on resume every recorded chunk is re-hashed; before install every chunk
//     is re-hashed again, then the state is marked complete and finalize runs.
const (
	parallelMetaVersion = 2
	maxDownloadWorkers  = 16
	minChunkBytes       = 8 << 20
	chunkAlign          = 1 << 20
	maxParallelChunks   = 1024
)

// errChunkRetry marks chunk failures that may be retried (short body, read
// error mid-body). Validation failures are never retried.
var errChunkRetry = errors.New("quark: chunk transfer interrupted")

type parallelMeta struct {
	Version   int      `json:"version"`
	FID       string   `json:"fid"`
	Size      int64    `json:"size"`
	Remote    string   `json:"remote"`
	Target    string   `json:"target"`
	ETag      string   `json:"etag"`
	ChunkSize int64    `json:"chunk_size"`
	Chunks    []string `json:"chunk_sha256"` // "" = not yet completed
	Complete  bool     `json:"parallel_complete,omitempty"`
}

func (m parallelMeta) sameIdentity(o parallelMeta) bool {
	return m.Version == o.Version && m.FID == o.FID && m.Size == o.Size && m.Target == o.Target &&
		m.ETag == o.ETag && m.ChunkSize == o.ChunkSize && len(m.Chunks) == len(o.Chunks)
}

func (m parallelMeta) bounds(i int) (start, end int64) {
	start = int64(i) * m.ChunkSize
	length := min(m.ChunkSize, m.Size-start)
	return start, start + length
}

func parallelChunkSize(size int64) int64 {
	if size <= 0 {
		return minChunkBytes
	}
	cs := 1 + (size-1)/maxParallelChunks
	if cs < minChunkBytes {
		cs = minChunkBytes
	}
	return (cs + chunkAlign - 1) / chunkAlign * chunkAlign
}

func chunkCount(size, cs int64) int64 {
	if size <= 0 || cs <= 0 {
		return 0
	}
	return 1 + (size-1)/cs
}

func validSHA256Hex(s string) bool {
	if len(s) != sha256.Size*2 || s != strings.ToLower(s) {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// DownloadFileWorkers downloads remotePath to localPath like
// DownloadFileProgress, but may fetch up to workers byte ranges of the single
// file concurrently (1 <= workers <= 16). Parallel mode is only used when the
// server proves exact Range support with a strong ETag; otherwise, before any
// parallel state is written, it falls back to the sequential downloader.
// An existing sequential (version 1) partial is always continued
// sequentially. progress (may be nil) is called serially with the bytes
// actually downloaded (verified chunks plus in-flight bytes), never the
// sparse partial size; it must be quick and must not call into the Client.
func (c *Client) DownloadFileWorkers(ctx context.Context, remotePath, localPath string, workers int, progress func(done, total int64)) error {
	if workers < 1 || workers > maxDownloadWorkers {
		return fmt.Errorf("quark: workers must be between 1 and %d", maxDownloadWorkers)
	}
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
	if err := checkAbsent(p.target); err != nil {
		return err
	}

	raw, _, merr := readDownloadMeta(p.meta)
	switch {
	case merr == nil:
		var probe struct {
			Version int `json:"version"`
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			return fmt.Errorf("%w: unreadable metadata %s (data kept)", ErrPartialMismatch, p.meta)
		}
		switch probe.Version {
		case metaVersion:
			// Existing sequential partial: continue it exactly as before.
			return c.downloadLocked(ctx, f, remotePath, p, progress)
		case parallelMetaVersion:
			m, err := decodeParallelMeta(raw)
			if err != nil {
				return err
			}
			return c.resumeParallel(ctx, f, p, m, workers, progress)
		default:
			return fmt.Errorf("%w: %s has unknown state version %d (data kept)", ErrPartialMismatch, p.meta, probe.Version)
		}
	case errors.Is(merr, os.ErrNotExist):
	default:
		return fmt.Errorf("quark: read partial metadata: %w", merr)
	}

	// No metadata. A partial without metadata is never adopted.
	if _, err := os.Lstat(p.part); err == nil {
		return fmt.Errorf("%w: %s has no metadata", ErrPartialMismatch, p.part)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("quark: stat partial: %w", err)
	}
	cs := parallelChunkSize(f.Size)
	if workers == 1 || chunkCount(f.Size, cs) < 2 {
		return c.downloadLocked(ctx, f, remotePath, p, progress)
	}
	links := &linkCache{c: c, fid: f.ID}
	etag, ok, err := c.probeRange(ctx, links, f.Size)
	if err != nil {
		return err
	}
	if !ok {
		// Nothing parallel has been written: the sequential path is safe.
		return c.downloadLocked(ctx, f, remotePath, p, progress)
	}

	m := parallelMeta{
		Version: parallelMetaVersion, FID: f.ID, Size: f.Size, Remote: remotePath, Target: p.target,
		ETag: etag, ChunkSize: cs, Chunks: make([]string, chunkCount(f.Size, cs)),
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := publishStateExclusive(p.meta, b); err != nil {
		return err
	}
	if err := createPartExclusive(p.part); err != nil {
		return err
	}
	return c.runParallel(ctx, links, p, m, workers, progress)
}

func decodeParallelMeta(raw []byte) (parallelMeta, error) {
	var m parallelMeta
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil || dec.More() {
		return m, fmt.Errorf("%w: unreadable parallel metadata (data kept)", ErrPartialMismatch)
	}
	return m, nil
}

func (c *Client) resumeParallel(ctx context.Context, f File, p downloadPaths, m parallelMeta, workers int, progress func(done, total int64)) error {
	if m.Version != parallelMetaVersion || m.FID != f.ID || m.Size != f.Size || m.Target != p.target {
		return fmt.Errorf("%w: %s belongs to fid=%s size=%d, remote is fid=%s size=%d (data kept)",
			ErrPartialMismatch, p.part, m.FID, m.Size, f.ID, f.Size)
	}
	if m.ETag == "" || strongETag(m.ETag) != m.ETag {
		return fmt.Errorf("%w: parallel state without strong ETag (data kept)", ErrPartialMismatch)
	}
	n := chunkCount(m.Size, m.ChunkSize)
	if n < 1 || n > maxParallelChunks || int64(len(m.Chunks)) != n {
		return fmt.Errorf("%w: parallel state has an invalid chunk layout (data kept)", ErrPartialMismatch)
	}
	anyDone := false
	for _, s := range m.Chunks {
		if s != "" {
			if !validSHA256Hex(s) {
				return fmt.Errorf("%w: parallel state has an invalid chunk hash (data kept)", ErrPartialMismatch)
			}
			anyDone = true
		}
	}
	pfi, err := os.Lstat(p.part)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if anyDone || m.Complete {
			return fmt.Errorf("%w: recorded chunks but no partial file (metadata kept)", ErrPartialMismatch)
		}
		// Crash between metadata and partial creation.
		if err := createPartExclusive(p.part); err != nil {
			return err
		}
	case err != nil:
		return fmt.Errorf("quark: stat partial: %w", err)
	case !pfi.Mode().IsRegular():
		return fmt.Errorf("%w: %s is not a regular file", ErrPartialMismatch, p.part)
	case pfi.Size() > m.Size:
		return fmt.Errorf("%w: partial larger than remote (%d > %d)", ErrPartialMismatch, pfi.Size(), m.Size)
	}
	return c.runParallel(ctx, &linkCache{c: c, fid: f.ID}, p, m, workers, progress)
}

func createPartExclusive(name string) error {
	pf, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("quark: create partial: %w", err)
	}
	if err := pf.Close(); err != nil {
		return fmt.Errorf("quark: create partial: %w", err)
	}
	syncDir(filepath.Dir(name))
	return nil
}

// runParallel verifies recorded chunks, downloads the missing ones, verifies
// everything again and installs the file.
func (c *Client) runParallel(ctx context.Context, links *linkCache, p downloadPaths, m parallelMeta, workers int, progress func(done, total int64)) error {
	before, err := os.Lstat(p.part)
	if err != nil || !before.Mode().IsRegular() {
		return fmt.Errorf("%w: partial is missing or not a regular file", ErrPartialMismatch)
	}
	pf, err := os.OpenFile(p.part, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("quark: open partial: %w", err)
	}
	defer pf.Close()
	fi, err := pf.Stat()
	if err != nil || !fi.Mode().IsRegular() || !os.SameFile(before, fi) {
		return fmt.Errorf("%w: partial changed while opening", ErrPartialMismatch)
	}

	buf := make([]byte, copyBufBytes)
	var held int64
	var pending []int
	for i, want := range m.Chunks {
		s, e := m.bounds(i)
		if want == "" {
			pending = append(pending, i)
			continue
		}
		if fi.Size() < e {
			return fmt.Errorf("%w: chunk %d recorded but partial is too short (data kept)", ErrPartialMismatch, i)
		}
		got, err := hashRange(ctx, pf, s, e, buf)
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("%w: chunk %d content changed on disk (data kept)", ErrPartialMismatch, i)
		}
		held += e - s
	}
	if m.Complete && len(pending) > 0 {
		return fmt.Errorf("%w: state marked complete with missing chunks (data kept)", ErrPartialMismatch)
	}
	if len(pending) == 0 {
		// No range request will otherwise validate the stored ETag on resume.
		etag, ok, err := c.probeRange(ctx, links, m.Size)
		if err != nil {
			return err
		}
		if !ok || etag != m.ETag {
			return fmt.Errorf("%w: unable to confirm the stored validator before installing (data kept)", ErrPartialMismatch)
		}
	}

	rep := &progressReporter{done: held, total: m.Size, fn: progress}
	rep.add(0)
	st := &parallelState{name: p.meta, m: m}
	if len(pending) > 0 {
		if err := c.fetchChunks(ctx, links, pf, st, pending, workers, rep); err != nil {
			return err
		}
	}
	m = st.snapshot()

	// Final verification of every chunk from disk before marking complete.
	for i, want := range m.Chunks {
		s, e := m.bounds(i)
		if want == "" {
			return fmt.Errorf("%w: chunk %d missing after download (data kept)", ErrPartialMismatch, i)
		}
		got, err := hashRange(ctx, pf, s, e, buf)
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("%w: chunk %d failed final verification (data kept)", ErrPartialMismatch, i)
		}
	}
	if err := pf.Sync(); err != nil {
		return fmt.Errorf("quark: sync partial: %w", err)
	}
	fi, err = pf.Stat()
	if err != nil || fi.Size() != m.Size {
		return fmt.Errorf("%w: partial size mismatch after download (data kept)", ErrPartialMismatch)
	}
	if cur, err := os.Lstat(p.part); err != nil || !cur.Mode().IsRegular() || !os.SameFile(fi, cur) {
		return fmt.Errorf("%w: partial replaced during download (data kept)", ErrPartialMismatch)
	}
	if !m.Complete {
		next := m
		next.Complete = true
		if err := writeParallelState(p.meta, m, next); err != nil {
			return err
		}
		m = next
	}
	if err := pf.Close(); err != nil {
		return fmt.Errorf("quark: close partial: %w", err)
	}
	// Version -1 never matches finalize's sequential-metadata cleanup; the
	// parallel metadata is removed below after its own identity check.
	if err := finalize(p, m.Size, downloadMeta{Version: -1}, fi); err != nil {
		return err
	}
	removeParallelState(p.meta, m)
	return nil
}

func (c *Client) fetchChunks(ctx context.Context, links *linkCache, pf *os.File, st *parallelState, pending []int, workers int, rep *progressReporter) error {
	if workers > len(pending) {
		workers = len(pending)
	}
	jobs := make(chan int, len(pending))
	for _, i := range pending {
		jobs <- i
	}
	close(jobs)

	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		once     sync.Once
		firstErr error
		wg       sync.WaitGroup
	)
	fail := func(err error) {
		once.Do(func() {
			firstErr = err
			cancel()
		})
	}
	m := st.snapshot()
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, copyBufBytes)
			for i := range jobs {
				if wctx.Err() != nil {
					return
				}
				s, e := m.bounds(i)
				sum, err := c.fetchChunkRetry(wctx, links, pf, m.ETag, m.Size, s, e, buf, rep)
				if err != nil {
					fail(err)
					return
				}
				if err := st.record(i, sum); err != nil {
					fail(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}
	return firstErr
}

func (c *Client) fetchChunkRetry(ctx context.Context, links *linkCache, pf *os.File, etag string, size, start, end int64, buf []byte, rep *progressReporter) (string, error) {
	for attempt := 0; ; attempt++ {
		link, err := links.get(ctx)
		var sum string
		if err == nil {
			sum, err = c.fetchChunk(ctx, link, pf, etag, size, start, end, buf, rep)
			if err == nil {
				return sum, nil
			}
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if attempt >= downloadMaxRetries || !retryableChunkErr(err) {
			return "", err
		}
		links.invalidate(link)
		if err := sleepCtx(ctx, downloadRetryDelay<<attempt); err != nil {
			return "", err
		}
	}
}

func retryableChunkErr(err error) bool {
	if errors.Is(err, errChunkRetry) || isTransientNetErr(err) {
		return true
	}
	var he *HTTPError
	if errors.As(err, &he) {
		switch {
		case he.StatusCode == http.StatusForbidden, // possibly an expired signed link
			he.StatusCode == http.StatusRequestTimeout,
			he.StatusCode == http.StatusTooManyRequests,
			he.StatusCode >= 500 && he.StatusCode <= 599:
			return true
		}
	}
	return false
}

// fetchChunk downloads [start,end) into pf and returns its SHA-256. On any
// error the bytes written for this attempt are unrecorded and are rewritten
// by a later attempt; progress is rolled back.
func (c *Client) fetchChunk(ctx context.Context, link string, pf *os.File, etag string, size, start, end int64, buf []byte, rep *progressReporter) (sum string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return "", errors.New("quark: download: invalid request")
	}
	c.setHeaders(req)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Range", "bytes="+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(end-1, 10))
	req.Header.Set("If-Range", etag)
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("quark: download: %w", scrub(err))
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusPartialContent:
	case http.StatusOK, http.StatusRequestedRangeNotSatisfiable:
		return "", fmt.Errorf("%w: http %d for chunk %d-%d (data kept)", ErrRangeUnsupported, resp.StatusCode, start, end-1)
	default:
		return "", httpErr("download", resp)
	}
	if got := strings.TrimSpace(resp.Header.Get("ETag")); got != etag {
		return "", fmt.Errorf("%w: response validator missing or changed (data kept)", ErrPartialMismatch)
	}
	if ce := strings.TrimSpace(resp.Header.Get("Content-Encoding")); ce != "" && !strings.EqualFold(ce, "identity") {
		return "", fmt.Errorf("%w: encoded range response (data kept)", ErrRangeUnsupported)
	}
	if err := checkExactContentRange(resp.Header.Get("Content-Range"), start, end-1, size); err != nil {
		return "", err
	}
	n := end - start
	if resp.ContentLength >= 0 && resp.ContentLength != n {
		return "", fmt.Errorf("quark: download: content-length %d, expected %d", resp.ContentLength, n)
	}

	var written int64
	defer func() {
		if err != nil && written > 0 {
			rep.add(-written)
		}
	}()
	h := sha256.New()
	body := io.LimitReader(resp.Body, n+1)
	for {
		k, rerr := body.Read(buf)
		if k > 0 {
			if written+int64(k) > n {
				return "", fmt.Errorf("quark: download: chunk body longer than expected %d", n)
			}
			if _, werr := pf.WriteAt(buf[:k], start+written); werr != nil {
				return "", fmt.Errorf("quark: write partial: %w", werr)
			}
			h.Write(buf[:k])
			written += int64(k)
			rep.add(int64(k))
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return "", fmt.Errorf("%w: %v", errChunkRetry, scrub(rerr))
		}
	}
	if written != n {
		return "", fmt.Errorf("%w: short chunk body %d/%d", errChunkRetry, written, n)
	}
	if err := pf.Sync(); err != nil {
		return "", fmt.Errorf("quark: sync partial: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// probeRange checks with a one-byte request whether the server honours exact
// byte ranges with a strong validator. ok=false means "not proven", which is
// safe to answer with the sequential downloader since nothing was written.
func (c *Client) probeRange(ctx context.Context, links *linkCache, size int64) (etag string, ok bool, err error) {
	link, err := links.get(ctx)
	if err != nil {
		return "", false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, link, nil)
	if err != nil {
		return "", false, errors.New("quark: download: invalid request")
	}
	c.setHeaders(req)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Range", "bytes=0-0")
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", false, fmt.Errorf("quark: download probe: %w", scrub(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return "", false, nil
	}
	if ce := strings.TrimSpace(resp.Header.Get("Content-Encoding")); ce != "" && !strings.EqualFold(ce, "identity") {
		return "", false, nil
	}
	etag = strongETag(resp.Header.Get("ETag"))
	if etag == "" {
		return "", false, nil
	}
	if checkExactContentRange(resp.Header.Get("Content-Range"), 0, 0, size) != nil {
		return "", false, nil
	}
	if resp.ContentLength != 1 && resp.ContentLength != -1 {
		return "", false, nil
	}
	b, rerr := io.ReadAll(io.LimitReader(resp.Body, 2))
	if rerr != nil {
		return "", false, fmt.Errorf("quark: download probe: %w", scrub(rerr))
	}
	if len(b) != 1 {
		return "", false, nil
	}
	return etag, true, nil
}

// checkExactContentRange requires "bytes <first>-<last>/<size>" with a
// numeric total (no "*").
func checkExactContentRange(v string, first, last, size int64) error {
	bad := fmt.Errorf("%w: invalid Content-Range %q (data kept)", ErrRangeUnsupported, v)
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
	a, err1 := strconv.ParseInt(s, 10, 64)
	b, err2 := strconv.ParseInt(e, 10, 64)
	t, err3 := strconv.ParseInt(total, 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || a != first || b != last || t != size {
		return bad
	}
	return nil
}

func hashRange(ctx context.Context, f *os.File, start, end int64, buf []byte) (string, error) {
	h := sha256.New()
	r := io.NewSectionReader(f, start, end-start)
	var got int64
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		k, err := r.Read(buf)
		if k > 0 {
			h.Write(buf[:k])
			got += int64(k)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("quark: read partial: %w", err)
		}
	}
	if got != end-start {
		return "", fmt.Errorf("%w: partial too short at %d (data kept)", ErrPartialMismatch, start+got)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// parallelState serialises chunk completion records.
type parallelState struct {
	mu   sync.Mutex
	name string
	m    parallelMeta
}

func (s *parallelState) snapshot() parallelMeta {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.m
	m.Chunks = append([]string(nil), s.m.Chunks...)
	return m
}

func (s *parallelState) record(i int, sum string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m.Chunks[i] != "" {
		return fmt.Errorf("%w: chunk %d recorded twice", ErrPartialMismatch, i)
	}
	next := s.m
	next.Chunks = append([]string(nil), s.m.Chunks...)
	next.Chunks[i] = sum
	if err := writeParallelState(s.name, s.m, next); err != nil {
		return err
	}
	s.m = next
	return nil
}

// writeParallelState atomically replaces the metadata after checking that the
// file on disk is still exactly the state we expect (prev).
func writeParallelState(name string, prev, next parallelMeta) error {
	raw, before, err := readDownloadMeta(name)
	if err != nil {
		return fmt.Errorf("quark: read metadata before update: %w", err)
	}
	old, err := decodeParallelMeta(raw)
	if err != nil || !old.sameIdentity(prev) || !old.sameIdentity(next) || old.Complete != prev.Complete {
		return fmt.Errorf("%w: parallel metadata changed before update (data kept)", ErrPartialMismatch)
	}
	for i := range old.Chunks {
		if old.Chunks[i] != prev.Chunks[i] || (old.Chunks[i] != "" && next.Chunks[i] != old.Chunks[i]) {
			return fmt.Errorf("%w: parallel metadata changed before update (data kept)", ErrPartialMismatch)
		}
	}
	b, err := json.Marshal(next)
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
		_ = tmp.Close()
		return fmt.Errorf("quark: write metadata: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("quark: write metadata: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("quark: write metadata: %w", err)
	}
	cur, err := os.Lstat(name)
	if err != nil || !cur.Mode().IsRegular() || !os.SameFile(before, cur) {
		return fmt.Errorf("%w: parallel metadata changed before replace (data kept)", ErrPartialMismatch)
	}
	if err := os.Rename(tmpName, name); err != nil {
		return fmt.Errorf("quark: write metadata: %w", err)
	}
	syncDir(filepath.Dir(name))
	return nil
}

// publishStateExclusive creates name with content b without ever replacing
// an existing file (hard link of a fully written temp file).
func publishStateExclusive(name string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(name), filepath.Base(name)+".tmp-*")
	if err != nil {
		return fmt.Errorf("quark: create metadata: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("quark: create metadata: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("quark: sync metadata: %w", err)
	}
	if err := tmp.Close(); err != nil {
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

// removeParallelState deletes the metadata only if it is still exactly our
// completed state.
func removeParallelState(name string, m parallelMeta) {
	raw, original, err := readDownloadMeta(name)
	if err != nil {
		return
	}
	got, err := decodeParallelMeta(raw)
	if err != nil || !got.sameIdentity(m) || !got.Complete {
		return
	}
	for i := range got.Chunks {
		if got.Chunks[i] != m.Chunks[i] {
			return
		}
	}
	if cur, err := os.Lstat(name); err == nil && cur.Mode().IsRegular() && os.SameFile(original, cur) {
		_ = os.Remove(name)
		syncDir(filepath.Dir(name))
	}
}

// linkCache shares one signed download URL between workers and refreshes it
// after a failed attempt.
type linkCache struct {
	mu   sync.Mutex
	c    *Client
	fid  string
	link string
}

func (l *linkCache) get(ctx context.Context) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.link != "" {
		return l.link, nil
	}
	link, err := l.c.downloadURL(ctx, l.fid)
	if err != nil {
		return "", err
	}
	u, err := url.Parse(link)
	if err != nil || !l.c.trusted(u) {
		return "", ErrUntrustedURL
	}
	l.link = link
	return link, nil
}

func (l *linkCache) invalidate(old string) {
	if old == "" {
		return
	}
	l.mu.Lock()
	if l.link == old {
		l.link = ""
	}
	l.mu.Unlock()
}

// progressReporter serialises progress callbacks.
type progressReporter struct {
	mu          sync.Mutex
	done, total int64
	fn          func(done, total int64)
}

func (r *progressReporter) add(d int64) {
	if r == nil || r.fn == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.done += d
	if r.done < 0 {
		r.done = 0
	}
	if r.done > r.total {
		r.done = r.total
	}
	r.fn(r.done, r.total)
}
