package quark

import (
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
)

// Bounded parallel part upload.
//
// upload/pre is sent with parallel_upload=true and the server answers with
// metadata.part_thread, its suggested number of concurrent part PUTs. Even
// in parallel mode every part N>=2 must carry X-Oss-Hash-Ctx, the SHA-1
// state of parts 1..N-1. A single producer therefore reads the file
// strictly in order, computes each part's hash context plus the whole-file
// MD5/SHA-1, and hands (part, bytes, ctx) to a fixed set of workers. Only
// the PUTs run concurrently; hash state never depends on completion order.
//
// Memory: exactly `workers` part buffers are allocated up front and
// recycled through a channel, so at most workers*partSize bytes of file
// data are held regardless of how parts complete.
const (
	// maxUploadParallel caps part_thread. The reference client observes a
	// typical value of 3; anything larger from the server is clamped.
	maxUploadParallel = 4
	// uploadBufBudget bounds total part buffer memory.
	uploadBufBudget = 256 << 20
)

// parsePartThread interprets metadata.part_thread. Missing, non-integer or
// non-positive values fall back to 1 (sequential), never to a guess.
func parsePartThread(raw string) int {
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 1 {
		return 1
	}
	if n > maxUploadParallel {
		return maxUploadParallel
	}
	return int(n)
}

// uploadWorkers derives the worker count from the server hint, the part
// count and the buffer memory budget.
func uploadWorkers(partThread int, partSize, size int64) int {
	parts := 1 + (size-1)/partSize
	bufSize := min(partSize, size)
	w := int64(max(partThread, 1))
	w = min(w, parts, max(uploadBufBudget/bufSize, 1))
	return int(max(w, 1))
}

type partJob struct {
	part int
	buf  []byte
	hctx string
	sha  string // lowercase hex SHA-256 of buf
}

type partResult struct {
	part    int
	size    int64
	etag    string
	sha     string // lowercase hex SHA-256 of the part data
	skipped bool   // already recorded in the resume state; not re-sent
	buf     []byte // returned to the pool; nil for producer error results
	err     error
}

// partResume carries resume information into uploadParts. known parts are
// skipped only if the freshly read part has exactly the recorded SHA-256.
// commit is called from the calling goroutine after each newly accepted
// part, before progress is reported; an error stops the upload.
type partResume struct {
	known        map[int]resumePart
	initialBytes int64  // validated, previously accepted bytes
	fileSHA256   string // expected whole-file SHA-256 (lowercase hex)
	commit       func(part int, etag, sha string) error
}

type partsOutcome struct {
	etags   []string
	md5hex  string
	sha1hex string
}

// uploadParts uploads every part of f (size bytes, stat st at open) and
// returns the ordered ETags and whole-file digests. progress is invoked
// only from the calling goroutine, with a non-decreasing byte count. On any
// error (or cancellation) all goroutines are stopped and awaited before
// returning; nothing is reported as committed.
func (c *Client) uploadParts(parent context.Context, f *os.File, st os.FileInfo, s *uploadSession, mimeType string, size int64, res *partResume, progress func(done, total int64)) (*partsOutcome, error) {
	partSize := s.partSize
	totalParts := int(1 + (size-1)/partSize)
	workers := uploadWorkers(s.partThread, partSize, size)

	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	free := make(chan []byte, workers)
	bufSize := min(partSize, size)
	for i := 0; i < workers; i++ {
		free <- make([]byte, bufSize)
	}
	jobs := make(chan partJob)
	// Capacity covers every possible concurrent sender (workers plus the
	// producer) so senders never block on an exited receiver; the receiver
	// drains until close anyway.
	results := make(chan partResult, workers+1)

	var out partsOutcome // digests written by producer, read after wg.Wait
	var wg sync.WaitGroup

	// Producer: sequential read, hashing and hash-context computation.
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(jobs)
		fail := func(err error) {
			results <- partResult{err: err}
			cancel()
		}
		md5h, sha1h, chain, whole := md5.New(), sha1.New(), sha1.New(), sha256.New()
		// Always read from offset 0, independent of the handle's position.
		rd := io.NewSectionReader(f, 0, size)
		var off int64
		for part := 1; part <= totalParts; part++ {
			var buf []byte
			select {
			case buf = <-free:
			case <-ctx.Done():
				return
			}
			n := min(partSize, size-off)
			chunk := buf[:n]
			if _, err := io.ReadFull(rd, chunk); err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
					fail(ErrFileChanged)
				} else {
					fail(fmt.Errorf("quark: upload: read: %w", err))
				}
				return
			}
			var hctx string
			if part >= 2 {
				var err error
				if hctx, err = sha1Ctx(chain); err != nil {
					fail(err)
					return
				}
			}
			md5h.Write(chunk)
			sha1h.Write(chunk)
			chain.Write(chunk)
			whole.Write(chunk)
			psum := sha256.Sum256(chunk)
			psha := hex.EncodeToString(psum[:])
			off += n
			if res != nil {
				if kp, ok := res.known[part]; ok {
					if kp.SHA256 != psha {
						fail(fmt.Errorf("%w: part %d differs from resume state", ErrFileChanged, part))
						return
					}
					// Recorded and identical: reuse its ETag, do not re-send.
					select {
					case results <- partResult{part: part, size: n, etag: kp.ETag, sha: psha, skipped: true, buf: buf[:cap(buf)]}:
					case <-ctx.Done():
						return
					}
					continue
				}
			}
			select {
			case jobs <- partJob{part: part, buf: chunk, hctx: hctx, sha: psha}:
			case <-ctx.Done():
				return
			}
		}
		// The file must not have grown or been replaced while reading.
		if extra, readErr := f.ReadAt(make([]byte, 1), size); extra != 0 {
			fail(ErrFileChanged)
			return
		} else if readErr != nil && !errors.Is(readErr, io.EOF) {
			fail(fmt.Errorf("quark: upload: read end of file: %w", readErr))
			return
		}
		if st2, err := f.Stat(); err != nil || st2.Size() != size || !sameLocal(st, st2) {
			fail(ErrFileChanged)
			return
		}
		if res != nil && hex.EncodeToString(whole.Sum(nil)) != res.fileSHA256 {
			fail(fmt.Errorf("%w: content differs from the pre-upload hash", ErrFileChanged))
			return
		}
		out.md5hex = hex.EncodeToString(md5h.Sum(nil))
		out.sha1hex = hex.EncodeToString(sha1h.Sum(nil))
	}()

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				r := partResult{part: job.part, size: int64(len(job.buf)), sha: job.sha, buf: job.buf[:cap(job.buf)]}
				if err := ctx.Err(); err != nil {
					r.err = err
				} else {
					r.etag, r.err = c.uploadPartRetry(ctx, s, mimeType, job.part, job.buf, job.hctx)
				}
				results <- r
			}
		}()
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	etags := make([]string, totalParts)
	var done int64
	if res != nil {
		done = res.initialBytes
	}
	var firstErr error
	isCtx := func(err error) bool {
		return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
	}
	for r := range results {
		if r.buf != nil {
			free <- r.buf // never blocks: only `workers` buffers exist
		}
		if r.err == nil && (r.part < 1 || r.part > totalParts || etags[r.part-1] != "" || r.etag == "") {
			r.err = fmt.Errorf("quark: upload: inconsistent result for part %d", r.part)
		}
		if r.err != nil {
			// Prefer the root cause over the cancellations it triggers.
			if firstErr == nil || (isCtx(firstErr) && !isCtx(r.err)) {
				firstErr = r.err
			}
			cancel()
			continue
		}
		if firstErr != nil {
			continue // late success after a failure: ignore, no progress
		}
		if !r.skipped && res != nil && res.commit != nil {
			// Persist etag+hash before counting the part as done. On failure
			// stop; the part is re-sent next time (409 check applies).
			if err := res.commit(r.part, r.etag, r.sha); err != nil {
				firstErr = fmt.Errorf("quark: upload: save resume state after part %d (nothing committed): %w", r.part, err)
				cancel()
				continue
			}
		}
		etags[r.part-1] = r.etag
		if !r.skipped {
			done += r.size
			if progress != nil {
				progress(done, size)
			}
		}
	}
	// results is closed only after wg.Wait: every goroutine has exited.

	if err := parent.Err(); err != nil {
		return nil, err
	}
	if firstErr != nil {
		return nil, firstErr
	}
	if done != size {
		return nil, fmt.Errorf("quark: upload: %d of %d bytes accepted", done, size)
	}
	for i, e := range etags {
		if e == "" {
			return nil, fmt.Errorf("quark: upload: part %d missing", i+1)
		}
	}
	if out.md5hex == "" || out.sha1hex == "" {
		return nil, errors.New("quark: upload: digests unavailable")
	}
	out.etags = etags
	return &out, nil
}

// A rejection of a later commit-phase request does not prove that an
// earlier hash update or OSS callback made no remote change. Preserve the
// underlying error for diagnostics, but require checking before retrying.
func unconfirmedCommit(ctx context.Context, op string, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%w: %s: %v", ErrUnconfirmed, op, ctx.Err())
	}
	return fmt.Errorf("%w: %s: %v", ErrUnconfirmed, op, err)
}
