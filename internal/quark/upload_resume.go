package quark

// Local resume records for non-empty uploads.
//
// A record is keyed by a one-way SHA-256 fingerprint of (absolute local
// path, destination directory FID), with a separate account fingerprint in
// the protected record. The cookie itself is never stored or logged. A
// changed account/session refuses the old record instead of starting over.
// The record holds the
// upload/pre session (including the temporary OSS authorization auth_info
// and callback, which are sensitive), the file size and whole-file SHA-256,
// the part layout and, for each part already accepted by OSS, its ETag and
// the SHA-256 of the part data. Storage, locking and file/ACL protection are
// done by internal/credential; this file only validates content.
//
// Rules:
//   - The whole file is hashed (SHA-256) from the opened handle before every
//     upload/resume, and the handle must not change while being read.
//   - A recorded part is skipped only if its data hashes to exactly the
//     recorded SHA-256; the OSS hash contexts and whole-file MD5/SHA-1 are
//     always rebuilt by reading the file in order. Unrecorded parts are sent.
//   - A new session is persisted before the first part; each accepted part is
//     persisted before it counts as progress. A failed save stops the upload
//     without committing and keeps the record.
//   - Before update/hash, OSS complete or upload/finish the phase is persisted
//     as "finalizing". A finalizing record always yields ErrUnconfirmed and is
//     never resent or reset automatically.
//   - Invalid, foreign or mismatching records stop the upload and are kept.
//     A session rejected by the server is also kept. Records are cleared
//     only after the existing c.verify check succeeded.
//
// Not verified: how long the server keeps an upload session and its parts,
// and whether recorded ETags are still valid. No ListParts call is made; the
// recorded ETags are only checked by the server at final commit.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/zzfn/QDrive/internal/credential"
)

const (
	uploadStateVersion    = 1
	uploadPhaseUploading  = "uploading"
	uploadPhaseFinalizing = "finalizing"

	maxStateToken = 1024
	maxStateURL   = 2048
	maxStatePath  = 32 << 10
	maxStateBlob  = 64 << 10
	maxETagLen    = 128

	hashReadBuf = 1 << 20
)

// ErrResumeState reports a local resume record that was rejected. The record
// is left in place; nothing was uploaded because of it.
var ErrResumeState = errors.New("quark: upload: local resume state rejected (kept; no further data sent)")

type uploadStateStore interface {
	Load() ([]byte, error)
	Save([]byte) error
	Clear() error
	Close() error
}

// openUploadState is a variable so tests can substitute an in-memory store.
var openUploadState = func(key string) (uploadStateStore, error) {
	s, err := credential.OpenUploadState(key)
	if err != nil {
		return nil, err
	}
	return s, nil
}

type resumePart struct {
	N      int    `json:"n"`
	ETag   string `json:"etag"`
	SHA256 string `json:"sha256"`
}

type resumeSession struct {
	TaskID    string          `json:"task_id"`
	Bucket    string          `json:"bucket"`
	ObjKey    string          `json:"obj_key"`
	UploadID  string          `json:"upload_id"`
	UploadURL string          `json:"upload_url"`
	AuthInfo  json.RawMessage `json:"auth_info"`
	Callback  json.RawMessage `json:"callback"`
}

type resumeState struct {
	Version    int           `json:"version"`
	Key        string        `json:"key"`
	Account    string        `json:"account_fingerprint"`
	Phase      string        `json:"phase"`
	LocalPath  string        `json:"local_path"`
	DirID      string        `json:"dir_id"`
	FileName   string        `json:"file_name"`
	MimeType   string        `json:"mime_type"`
	Size       int64         `json:"size"`
	FileSHA256 string        `json:"file_sha256"`
	PartSize   int64         `json:"part_size"`
	PartCount  int           `json:"part_count"`
	PartThread int           `json:"part_thread"`
	CreatedAt  int64         `json:"created_at"` // unix seconds
	Session    resumeSession `json:"session"`
	Parts      []resumePart  `json:"parts"`
}

type resumeIdentity struct {
	key, account, absPath, dirID, name, mimeType string
	size                                         int64
}

// Keying independently of the session lets a new login find and reject an
// old record rather than silently starting a potentially duplicate upload.
func uploadStateKey(absPath, dirID string) string {
	h := sha256.New()
	h.Write([]byte("qdrive/upload-state/v1"))
	for _, f := range []string{absPath, dirID} {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(f)))
		h.Write(n[:])
		h.Write([]byte(f))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func uploadAccountFingerprint(cookie string) string {
	h := sha256.New()
	h.Write([]byte("qdrive/upload-account/v1\x00"))
	h.Write([]byte(cookie))
	return hex.EncodeToString(h.Sum(nil))
}

func rejectState(what string) error { return fmt.Errorf("%w: %s", ErrResumeState, what) }

func isLowerHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// validToken accepts 1..max printable ASCII characters without spaces.
func validToken(s string, max int) bool {
	if s == "" || len(s) > max {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// validETag also excludes XML metacharacters (ETags go into the complete
// request body unescaped).
func validETag(e string) bool {
	return validToken(e, maxETagLen) && !bytes.ContainsAny([]byte(e), "<>&")
}

// session rebuilds an uploadSession, re-applying the checks of uploadPre.
func (rs *resumeState) session() (*uploadSession, error) {
	ss := rs.Session
	if !validToken(ss.TaskID, maxStateToken) || !validToken(ss.UploadID, maxStateToken) {
		return nil, rejectState("session identifiers")
	}
	if !validBucket(ss.Bucket) || len(ss.ObjKey) > maxStateToken || len(ss.UploadURL) > maxStateURL {
		return nil, rejectState("session location")
	}
	if len(ss.AuthInfo) == 0 || len(ss.AuthInfo) > maxStateBlob || !json.Valid(ss.AuthInfo) ||
		string(bytes.TrimSpace(ss.AuthInfo)) == "null" {
		return nil, rejectState("session authorization")
	}
	if len(ss.Callback) == 0 || len(ss.Callback) > maxStateBlob {
		return nil, rejectState("session callback")
	}
	var cbObj map[string]json.RawMessage
	if err := json.Unmarshal(ss.Callback, &cbObj); err != nil || cbObj == nil {
		return nil, rejectState("session callback")
	}
	var cb bytes.Buffer
	if err := json.Compact(&cb, ss.Callback); err != nil {
		return nil, rejectState("session callback")
	}
	base, err := ossObjectURL(ss.UploadURL, ss.Bucket, ss.ObjKey) // HTTPS + quark.cn only
	if err != nil || !isQuarkURL(base) {
		return nil, rejectState("session upload host")
	}
	return &uploadSession{
		TaskID:     ss.TaskID,
		Bucket:     ss.Bucket,
		ObjKey:     ss.ObjKey,
		UploadID:   ss.UploadID,
		UploadURL:  ss.UploadURL,
		AuthInfo:   append(json.RawMessage(nil), ss.AuthInfo...),
		Callback:   append(json.RawMessage(nil), ss.Callback...),
		partSize:   rs.PartSize,
		partThread: rs.PartThread,
		callback:   base64.StdEncoding.EncodeToString(cb.Bytes()),
		ossBase:    base,
	}, nil
}

// validate checks every field against the expected identity. The server's
// session lifetime is unknown, so no local expiry is assumed.
func (rs *resumeState) validate(id resumeIdentity) error {
	if rs.Version != uploadStateVersion {
		return rejectState("unsupported version")
	}
	if len(rs.Key) != len(id.key) || subtle.ConstantTimeCompare([]byte(rs.Key), []byte(id.key)) != 1 ||
		len(rs.Account) != len(id.account) || subtle.ConstantTimeCompare([]byte(rs.Account), []byte(id.account)) != 1 {
		return rejectState("account or identity mismatch")
	}
	if len(rs.LocalPath) > maxStatePath || rs.LocalPath != id.absPath ||
		len(rs.DirID) > maxStateToken || rs.DirID != id.dirID ||
		len(rs.FileName) > maxStateToken || rs.FileName != id.name ||
		len(rs.MimeType) > maxStateToken || rs.MimeType != id.mimeType {
		return rejectState("identity mismatch")
	}
	if rs.Phase != uploadPhaseUploading && rs.Phase != uploadPhaseFinalizing {
		return rejectState("phase")
	}
	if rs.Size <= 0 || rs.Size != id.size {
		return fmt.Errorf("%w: %w: size differs from resume state", ErrFileChanged, ErrResumeState)
	}
	if !isLowerHex64(rs.FileSHA256) {
		return rejectState("file hash")
	}
	if rs.PartSize <= 0 || rs.PartSize > maxPartSize || rs.PartSize%64 != 0 {
		return rejectState("part size")
	}
	n := 1 + (rs.Size-1)/rs.PartSize
	if n > maxParts || int64(rs.PartCount) != n {
		return rejectState("part count")
	}
	if rs.PartThread < 1 || rs.PartThread > maxUploadParallel {
		return rejectState("part thread")
	}
	if rs.CreatedAt <= 0 {
		return rejectState("timestamp")
	}
	if len(rs.Parts) > rs.PartCount {
		return rejectState("parts")
	}
	prev := 0
	for _, p := range rs.Parts {
		if p.N <= prev || p.N > rs.PartCount || !validETag(p.ETag) || !isLowerHex64(p.SHA256) {
			return rejectState("parts")
		}
		prev = p.N
	}
	if rs.Phase == uploadPhaseFinalizing && len(rs.Parts) != rs.PartCount {
		return rejectState("parts")
	}
	if _, err := rs.session(); err != nil {
		return err
	}
	return nil
}

// decodeResumeState parses strictly. Errors never include the content.
func decodeResumeState(data []byte, id resumeIdentity) (*resumeState, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var rs resumeState
	if err := dec.Decode(&rs); err != nil {
		return nil, rejectState("cannot decode")
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, rejectState("trailing data")
	}
	if err := rs.validate(id); err != nil {
		return nil, err
	}
	return &rs, nil
}

func saveResumeState(store uploadStateStore, rs *resumeState) error {
	data, err := json.Marshal(rs)
	if err != nil {
		return errors.New("quark: upload: encode resume state failed")
	}
	err = store.Save(data)
	clear(data)
	return err
}

// hashOpenedFile computes the SHA-256 of the first st.Size() bytes of f and
// checks that the file did not grow or change while being read.
func hashOpenedFile(ctx context.Context, f *os.File, st os.FileInfo) (string, error) {
	size := st.Size()
	h := sha256.New()
	buf := make([]byte, min(int64(hashReadBuf), max(size, 1)))
	var off int64
	for off < size {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n := int(min(int64(len(buf)), size-off))
		m, err := f.ReadAt(buf[:n], off)
		if m != n {
			if err == nil || errors.Is(err, io.EOF) {
				return "", ErrFileChanged
			}
			return "", fmt.Errorf("quark: upload: read: %w", err)
		}
		h.Write(buf[:n])
		off += int64(n)
	}
	if extra, readErr := f.ReadAt(make([]byte, 1), size); extra != 0 {
		return "", ErrFileChanged
	} else if readErr != nil && !errors.Is(readErr, io.EOF) {
		return "", fmt.Errorf("quark: upload: read end of file: %w", readErr)
	}
	cur, err := f.Stat()
	if err != nil || !sameLocal(st, cur) {
		return "", ErrFileChanged
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// uploadResumable uploads a non-empty file using (and maintaining) the local
// resume record. Destination conflict protection is unchanged.
func (c *Client) uploadResumable(ctx context.Context, f *os.File, st os.FileInfo, localPath, dirID, name, mimeType string, size int64, progress func(done, total int64)) (err error) {
	if c.cookie == "" {
		// The account fingerprint would be meaningless; refuse rather than
		// risk sharing records across accounts.
		return fmt.Errorf("%w: resumable upload requires an account cookie", ErrUnsupported)
	}
	abs, err := filepath.Abs(localPath)
	if err != nil {
		return fmt.Errorf("quark: upload: %w", err)
	}
	abs = filepath.Clean(abs)
	id := resumeIdentity{
		key:      uploadStateKey(abs, dirID),
		account:  uploadAccountFingerprint(c.cookie),
		absPath:  abs,
		dirID:    dirID,
		name:     name,
		mimeType: mimeType,
		size:     size,
	}

	store, err := openUploadState(id.key)
	if err != nil {
		return fmt.Errorf("quark: upload: open resume state: %w", err)
	}
	defer func() {
		cerr := store.Close()
		if cerr == nil {
			return
		}
		if err == nil {
			err = fmt.Errorf("%w: upload of %s was verified, but releasing the local resume state lock failed (left in place): %v", ErrUnconfirmed, name, cerr)
			return
		}
		err = errors.Join(err, fmt.Errorf("quark: upload: release resume state lock (left in place): %w", cerr))
	}()

	var rs *resumeState
	data, lerr := store.Load()
	switch {
	case lerr == nil:
		rs, err = decodeResumeState(data, id)
		clear(data)
		if err != nil {
			return fmt.Errorf("%w: upload state %s.json cannot be interpreted; check the destination before retrying: %w", ErrUnconfirmed, id.key, err)
		}
	case errors.Is(lerr, os.ErrNotExist):
	default:
		return fmt.Errorf("quark: upload: load resume state: %w", lerr)
	}
	if rs != nil {
		if rs.Phase == uploadPhaseFinalizing {
			return fmt.Errorf("%w: a previous upload of %s reached the commit phase; check the destination (state %s.json kept, nothing resent)", ErrUnconfirmed, name, id.key)
		}
	}

	fileSHA, err := hashOpenedFile(ctx, f, st)
	if err != nil {
		return err
	}
	if rs != nil && fileSHA != rs.FileSHA256 {
		return fmt.Errorf("%w: content differs from resume state (state kept)", ErrFileChanged)
	}
	var resumedBytes int64
	if rs != nil {
		buf := make([]byte, hashReadBuf)
		for _, part := range rs.Parts {
			start := int64(part.N-1) * rs.PartSize
			length := min(rs.PartSize, size-start)
			sum, hashErr := hashRange(ctx, f, start, start+length, buf)
			if errors.Is(hashErr, ErrPartialMismatch) {
				return ErrFileChanged
			}
			if hashErr != nil {
				return hashErr
			}
			if sum != part.SHA256 {
				return fmt.Errorf("%w: part %d differs from resume state", ErrFileChanged, part.N)
			}
			resumedBytes += length
		}
		if current, statErr := f.Stat(); statErr != nil || !sameLocal(st, current) {
			return ErrFileChanged
		}
	}

	entries, err := c.listDir(ctx, dirID)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name == name {
			return fmt.Errorf("%w: %s", ErrExists, name)
		}
	}

	var s *uploadSession
	if rs == nil {
		s, err = c.uploadPre(ctx, name, mimeType, size, dirID)
		if err != nil {
			return err
		}
		n := 1 + (size-1)/s.partSize
		if n > maxParts {
			return fmt.Errorf("%w: %d parts exceeds %d", ErrUnsupported, n, maxParts)
		}
		rs = &resumeState{
			Version:    uploadStateVersion,
			Key:        id.key,
			Account:    id.account,
			Phase:      uploadPhaseUploading,
			LocalPath:  abs,
			DirID:      dirID,
			FileName:   name,
			MimeType:   mimeType,
			Size:       size,
			FileSHA256: fileSHA,
			PartSize:   s.partSize,
			PartCount:  int(n),
			PartThread: s.partThread,
			CreatedAt:  time.Now().Unix(),
			Session: resumeSession{
				TaskID:    s.TaskID,
				Bucket:    s.Bucket,
				ObjKey:    s.ObjKey,
				UploadID:  s.UploadID,
				UploadURL: s.UploadURL,
				AuthInfo:  append(json.RawMessage(nil), s.AuthInfo...),
				Callback:  append(json.RawMessage(nil), s.Callback...),
			},
		}
		// Refuse sessions we could not reload later.
		if err := rs.validate(id); err != nil {
			return fmt.Errorf("%w: upload pre session not storable: %w", ErrUnsupported, err)
		}
		if err := saveResumeState(store, rs); err != nil {
			return fmt.Errorf("quark: upload: save resume state before first part (nothing uploaded): %w", err)
		}
	} else {
		s, err = rs.session()
		if err != nil {
			return err
		}
	}

	known := make(map[int]resumePart, len(rs.Parts))
	for _, p := range rs.Parts {
		known[p.N] = p
	}
	res := &partResume{
		known:        known,
		initialBytes: resumedBytes,
		fileSHA256:   rs.FileSHA256,
		commit: func(part int, etag, sha string) error {
			if !validETag(etag) || !isLowerHex64(sha) || part < 1 || part > rs.PartCount {
				return errors.New("quark: upload: part result not storable")
			}
			for _, p := range rs.Parts {
				if p.N == part {
					return errors.New("quark: upload: duplicate part result")
				}
			}
			rs.Parts = append(rs.Parts, resumePart{N: part, ETag: etag, SHA256: sha})
			sort.Slice(rs.Parts, func(i, j int) bool { return rs.Parts[i].N < rs.Parts[j].N })
			return saveResumeState(store, rs)
		},
	}

	if progress != nil {
		progress(resumedBytes, size)
	}
	parts, err := c.uploadParts(ctx, f, st, s, mimeType, size, res, progress)
	if err != nil {
		return err
	}

	if err := ctx.Err(); err != nil {
		return err // nothing commit-related sent; state still "uploading"
	}
	rs.Phase = uploadPhaseFinalizing
	if err := saveResumeState(store, rs); err != nil {
		return fmt.Errorf("quark: upload: could not record commit phase; nothing committed: %w", err)
	}
	if err := c.uploadCommit(ctx, s, parts, dirID, name, size); err != nil {
		if errors.Is(err, ErrUnconfirmed) {
			return err
		}
		// Even if nothing was sent (e.g. cancellation just before the
		// first commit request) the record now says finalizing; a later
		// attempt will report ErrUnconfirmed.
		return fmt.Errorf("%w: commit phase of %s did not complete (local resume state kept as finalizing): %w", ErrUnconfirmed, name, err)
	}
	if err := store.Clear(); err != nil {
		return fmt.Errorf("%w: %s appears uploaded and verified, but its local resume state could not be cleared (still present): %v", ErrUnconfirmed, name, err)
	}
	return nil
}
