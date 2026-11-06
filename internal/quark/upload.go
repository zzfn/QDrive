package quark

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"hash"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Upload protocol (as observed for the Quark web client):
//
//  1. POST upload/pre      -> task_id, bucket, obj_key, upload_id, upload_url,
//     auth_info, callback; metadata.part_size
//  2. for each part (read and hashed in order, PUT with bounded
//     concurrency from metadata.part_thread; see upload_parallel.go):
//     POST upload/auth   (signs an OSS string-to-sign) -> auth_key
//     PUT  OSS ?partNumber=N&uploadId=..  (X-Oss-Hash-Ctx for N>=2) -> ETag
//  3. POST update/hash     (whole-file md5+sha1) -> finish (already stored)
//  4. unless finish: POST upload/auth + POST OSS ?uploadId=.. (complete)
//  5. POST upload/finish
//
// Part PUTs are retried on transient transport errors (upload_retry.go);
// no other step is retried. Failures of steps 3-5 that may have reached the
// server are reported as ErrUnconfirmed (see unconfirmedCommit).
//
// UploadProgress uploads a single file into an existing directory. Non-empty
// files keep a private local resume record (upload_resume.go); empty files
// only use the unverified rapid-upload path and keep no record. Overwrite is
// not supported; directory uploads are implemented in upload_tree.go.
const (
	uploadPrePath    = "/1/clouddrive/file/upload/pre"
	uploadHashPath   = "/1/clouddrive/file/update/hash"
	uploadAuthPath   = "/1/clouddrive/file/upload/auth"
	uploadFinishPath = "/1/clouddrive/file/upload/finish"

	ossUserAgent = "aliyun-sdk-js/1.0.0 Chrome 145.0.0.0 on Windows 10 64-bit"

	maxPartSize = 128 << 20
	maxParts    = 10000
)

var (
	// ErrUnsupported reports an upload case this client deliberately does
	// not handle.
	ErrUnsupported = errors.New("quark: upload: unsupported")
	// ErrExists reports that the target directory already has an entry with
	// the uploaded file's name.
	ErrExists = errors.New("quark: file already exists")
	// ErrFileChanged reports that the local file changed while uploading.
	ErrFileChanged = errors.New("quark: upload: local file changed during upload")
)

type uploadSession struct {
	TaskID    string          `json:"task_id"`
	Bucket    string          `json:"bucket"`
	ObjKey    string          `json:"obj_key"`
	UploadID  string          `json:"upload_id"`
	UploadURL string          `json:"upload_url"`
	AuthInfo  json.RawMessage `json:"auth_info"`
	Callback  json.RawMessage `json:"callback"`

	partSize   int64
	partThread int      // server-suggested part concurrency, clamped, >= 1
	callback   string   // base64 of compact callback JSON object
	ossBase    *url.URL // https://bucket.host/objkey (escaped)
}

// Upload uploads the regular file at localPath into the existing remote
// directory remoteDir, keeping the local base name. It refuses an existing
// same-name entry observed before upload. Concurrent remote writes may race
// this check; the server's collision behavior has not been verified.
func (c *Client) Upload(ctx context.Context, localPath string, remoteDir string) error {
	return c.UploadProgress(ctx, localPath, remoteDir, nil)
}

// UploadProgress is Upload with an optional progress callback. If progress
// is non-nil it is called synchronously, never concurrently, from the
// goroutine that called UploadProgress, first with the validated previously
// accepted byte count (zero for a new upload), then after each new part has
// been accepted by OSS and recorded locally. The cumulative byte count is
// non-decreasing; parts may complete out of order.
// done == total means every part is stored, not that the file is committed;
// the upload is complete only when UploadProgress returns nil. The callback
// must not block for long and must not call back into the Client.
//
// Non-empty uploads are resumable on the same machine for the same absolute
// local path, destination directory and account (see upload_resume.go);
// resumed parts are reported as progress too. If the commit phase was
// reached or fails in a way that leaves the remote state unknown, the error
// wraps ErrUnconfirmed; check the remote directory before retrying.
func (c *Client) UploadProgress(ctx context.Context, localPath string, remoteDir string, progress func(done, total int64)) error {
	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("quark: upload: %w", err)
	}
	defer f.Close()
	return c.uploadOpened(ctx, f, localPath, remoteDir, "", nil, progress)
}

// uploadOpened uses an already-open handle. Directory uploads pass the
// planned identity and resolved destination ID, avoiding a second open or
// path-based destination resolution after their checks.
func (c *Client) uploadOpened(ctx context.Context, f *os.File, localPath, remoteDir, dirID string, expected os.FileInfo, progress func(done, total int64)) error {
	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("quark: upload: %w", err)
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("%w: not a regular file: %s", ErrUnsupported, localPath)
	}
	if expected != nil && !sameLocal(st, expected) {
		return fmt.Errorf("%w: %s", ErrFileChanged, localPath)
	}
	size := st.Size()
	name := filepath.Base(localPath)
	if name == "" || name == "." || name == string(filepath.Separator) || strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("quark: upload: invalid file name %q", name)
	}

	if dirID == "" {
		dir, err := c.resolve(ctx, remoteDir)
		if err != nil {
			return err
		}
		if !dir.IsDir {
			return fmt.Errorf("%w: %s", ErrNotDir, remoteDir)
		}
		dirID = dir.ID
	}
	mimeType := mime.TypeByExtension(filepath.Ext(name))
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	if size > 0 {
		// Resumable path; it performs its own destination conflict check
		// after inspecting any local resume state.
		return c.uploadResumable(ctx, f, st, localPath, dirID, name, mimeType, size, progress)
	}
	return c.uploadEmpty(ctx, f, st, localPath, dirID, name, mimeType, progress)
}

// uploadEmpty preserves the empty-file rapid-upload behavior, but takes the
// same lock and checks for a previous non-empty upload before sending it.
func (c *Client) uploadEmpty(ctx context.Context, f *os.File, st os.FileInfo, localPath, dirID, name, mimeType string, progress func(int64, int64)) (err error) {
	abs, err := filepath.Abs(localPath)
	if err != nil {
		return fmt.Errorf("quark: upload: %w", err)
	}
	key := uploadStateKey(filepath.Clean(abs), dirID)
	store, err := openUploadState(key)
	if err != nil {
		return fmt.Errorf("quark: upload: open resume state: %w", err)
	}
	defer func() {
		if closeErr := store.Close(); closeErr != nil {
			if err == nil {
				err = fmt.Errorf("%w: upload was verified but its local lock could not be released: %v", ErrUnconfirmed, closeErr)
			} else {
				err = errors.Join(err, fmt.Errorf("quark: upload: release local lock: %w", closeErr))
			}
		}
	}()
	data, loadErr := store.Load()
	if loadErr == nil {
		clear(data)
		return fmt.Errorf("%w: upload record %s.json already exists for %s; check it and the destination before retrying (record kept)", ErrUnconfirmed, key, name)
	}
	if !errors.Is(loadErr, os.ErrNotExist) {
		return fmt.Errorf("quark: upload: load resume state: %w", loadErr)
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

	s, err := c.uploadPre(ctx, name, mimeType, 0, dirID)
	if err != nil {
		return err
	}
	if progress != nil {
		progress(0, 0)
	}
	// Empty file: unchanged legacy behavior, no resume record.
	// The reference implementation hashes an empty stream and attempts
	// rapid upload; if it isn't rapid, it also tries an empty-parts OSS
	// completion. We deliberately omit that unverified fallback.
	current, err := f.Stat()
	if err != nil || !sameLocal(st, current) {
		return fmt.Errorf("%w: empty file changed", ErrFileChanged)
	}
	md5sum, sha1sum := md5.Sum(nil), sha1.Sum(nil)
	parts := &partsOutcome{md5hex: fmt.Sprintf("%x", md5sum), sha1hex: fmt.Sprintf("%x", sha1sum)}
	return c.uploadCommit(ctx, s, parts, dirID, name, 0)
}

// uploadCommit runs update/hash, the OSS complete request (if needed) and
// upload/finish, then verifies the destination listing.
func (c *Client) uploadCommit(ctx context.Context, s *uploadSession, parts *partsOutcome, dirID, name string, size int64) error {
	// Commit phase: from here on a request may change remote state.
	if err := ctx.Err(); err != nil {
		return err // nothing commit-related sent yet
	}
	finished, err := c.uploadHash(ctx, s, parts.md5hex, parts.sha1hex)
	if err != nil {
		return unconfirmedCommit(ctx, "upload hash", err)
	}
	if !finished && size == 0 {
		return fmt.Errorf("%w: server did not confirm empty-file rapid upload; check destination before retrying", ErrUnconfirmed)
	}
	if !finished {
		if err := c.uploadComplete(ctx, s, parts.etags); err != nil {
			return err // already classified
		}
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: upload finish not sent: %v", ErrUnconfirmed, err)
	}
	var out json.RawMessage
	body, _ := json.Marshal(map[string]string{"obj_key": s.ObjKey, "task_id": s.TaskID})
	if err := c.call(ctx, "upload finish", http.MethodPost, uploadFinishPath, nil, body, &out); err != nil {
		return unconfirmedCommit(ctx, "upload finish", err)
	}
	// A successful finish response alone does not establish that the file is
	// visible under the expected name. This check cannot prove content identity
	// or guard against concurrent remote writers.
	return c.verify(ctx, "upload finish", func() (bool, error) {
		matches, err := c.children(ctx, dirID, name)
		if err != nil {
			return false, err
		}
		return len(matches) == 1 && !matches[0].IsDir && matches[0].Size == size, nil
	})
}

func (c *Client) uploadPre(ctx context.Context, name, mimeType string, size int64, pdir string) (*uploadSession, error) {
	now := time.Now().UnixMilli()
	body, _ := json.Marshal(map[string]any{
		"ccp_hash_update": true,
		"parallel_upload": true,
		"dir_name":        "",
		"file_name":       name,
		"format_type":     mimeType,
		"l_created_at":    now,
		"l_updated_at":    now,
		"pdir_fid":        pdir,
		"size":            size,
	})
	// part_size lives in the top-level "metadata", which call() discards,
	// so decode the envelope here.
	var resp struct {
		Status   int    `json:"status"`
		Code     int    `json:"code"`
		Message  string `json:"message"`
		Data     *uploadSession
		Metadata struct {
			PartSize   json.Number `json:"part_size"`
			PartThread json.Number `json:"part_thread"`
		} `json:"metadata"`
	}
	if err := c.postJSONFull(ctx, "upload pre", uploadPrePath, body, &resp); err != nil {
		return nil, err
	}
	if resp.Code != 0 || resp.Status != http.StatusOK {
		return nil, &APIError{Op: "upload pre", Status: resp.Status, Code: resp.Code, Message: resp.Message}
	}
	s := resp.Data
	if s == nil {
		return nil, errors.New("quark: upload pre: missing data")
	}
	ps, err := strconv.ParseInt(resp.Metadata.PartSize.String(), 10, 64)
	if err != nil || ps <= 0 {
		return nil, errors.New("quark: upload pre: missing part_size")
	}
	// X-Oss-Hash-Ctx carries no pending bytes, so every part boundary must
	// fall on a SHA-1 block boundary.
	if ps > maxPartSize || ps%64 != 0 {
		return nil, fmt.Errorf("%w: part_size %d", ErrUnsupported, ps)
	}
	s.partSize = ps
	s.partThread = parsePartThread(resp.Metadata.PartThread.String())
	if s.TaskID == "" || s.UploadID == "" || len(s.AuthInfo) == 0 || string(s.AuthInfo) == "null" {
		return nil, errors.New("quark: upload pre: incomplete session")
	}
	var cbObj map[string]json.RawMessage
	if err := json.Unmarshal(s.Callback, &cbObj); err != nil || cbObj == nil {
		return nil, fmt.Errorf("%w: callback is not a JSON object", ErrUnsupported)
	}
	var cb bytes.Buffer
	if err := json.Compact(&cb, s.Callback); err != nil {
		return nil, fmt.Errorf("quark: upload pre: %w", err)
	}
	s.callback = base64.StdEncoding.EncodeToString(cb.Bytes())
	if s.ossBase, err = ossObjectURL(s.UploadURL, s.Bucket, s.ObjKey); err != nil {
		return nil, err
	}
	return s, nil
}

// postJSONFull posts to a trusted API path and decodes the whole envelope.
func (c *Client) postJSONFull(ctx context.Context, op, p string, body []byte, out any) error {
	u, err := url.Parse(c.baseURL + p)
	if err != nil || !c.trusted(u) {
		return fmt.Errorf("quark: %s: refusing untrusted api host", op)
	}
	u.RawQuery = url.Values{"pr": {"ucpro"}, "fr": {"pc"}, "uc_param_str": {""}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("quark: %s: %w", op, err)
	}
	c.setHeaders(req)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Origin", panOrigin)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("quark: %s: %w", op, scrub(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return httpErr(op, resp)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONBytes+1))
	if err != nil {
		return fmt.Errorf("quark: %s: read body: %w", op, err)
	}
	if len(data) > maxJSONBytes {
		return fmt.Errorf("quark: %s: response too large", op)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("quark: %s: invalid json: %w", op, err)
	}
	return nil
}

// ossObjectURL builds https://<bucket>.<upload host>/<obj_key> and refuses
// anything that is not a plain HTTPS quark.cn host, so a hostile pre-upload
// response cannot point uploads (or signatures) elsewhere.
func ossObjectURL(uploadURL, bucket, objKey string) (*url.URL, error) {
	bad := func(why string) error { return fmt.Errorf("%w: oss %s", ErrUntrustedURL, why) }
	if !validBucket(bucket) {
		return nil, bad("bucket")
	}
	if objKey == "" || strings.HasPrefix(objKey, "/") || strings.ContainsAny(objKey, "?#\\%") {
		return nil, bad("object key")
	}
	segs := strings.Split(objKey, "/")
	for i, sg := range segs {
		if sg == "" || sg == "." || sg == ".." {
			return nil, bad("object key")
		}
		for _, r := range sg {
			if r < 0x20 || r == 0x7f {
				return nil, bad("object key")
			}
		}
		segs[i] = url.PathEscape(sg)
	}
	raw := uploadURL
	switch {
	case strings.HasPrefix(raw, "https://"):
	case strings.HasPrefix(raw, "http://"):
		raw = "https://" + strings.TrimPrefix(raw, "http://") // always upgrade
	case !strings.Contains(raw, "://"):
		raw = "https://" + raw
	default:
		return nil, bad("scheme")
	}
	base, err := url.Parse(raw)
	if err != nil || base.User != nil || base.Port() != "" || (base.Path != "" && base.Path != "/") ||
		base.RawQuery != "" || base.Fragment != "" || base.Hostname() == "" {
		return nil, bad("upload_url")
	}
	u := &url.URL{
		Scheme:  "https",
		Host:    bucket + "." + strings.ToLower(base.Hostname()),
		Path:    "/" + objKey,
		RawPath: "/" + strings.Join(segs, "/"),
	}
	if !isQuarkURL(u) {
		return nil, bad("host")
	}
	return u, nil
}

func validBucket(b string) bool {
	if len(b) < 3 || len(b) > 63 || b[0] == '-' || b[len(b)-1] == '-' {
		return false
	}
	for _, r := range b {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return false
		}
	}
	return true
}

// sha1Ctx encodes the intermediate SHA-1 state (chaining values and bit
// length) of h, which must have consumed a multiple of 64 bytes.
func sha1Ctx(h hash.Hash) (string, error) {
	m, ok := h.(encoding.BinaryMarshaler)
	if !ok {
		return "", errors.New("quark: upload: sha1 state unavailable")
	}
	st, err := m.MarshalBinary()
	// Layout: magic(4) | h0..h4 (5x uint32 BE) | block(64) | length(uint64 BE)
	if err != nil || len(st) != 4+20+64+8 || string(st[:3]) != "sha" {
		return "", errors.New("quark: upload: unexpected sha1 state")
	}
	n := binary.BigEndian.Uint64(st[88:])
	if n%64 != 0 {
		return "", errors.New("quark: upload: sha1 state not block aligned")
	}
	bits := n * 8
	w := func(i int) string { return strconv.FormatUint(uint64(binary.BigEndian.Uint32(st[4+4*i:])), 10) }
	v := struct {
		HashType string `json:"hash_type"`
		H0       string `json:"h0"`
		H1       string `json:"h1"`
		H2       string `json:"h2"`
		H3       string `json:"h3"`
		H4       string `json:"h4"`
		Nl       string `json:"Nl"`
		Nh       string `json:"Nh"`
		Data     string `json:"data"`
		Num      string `json:"num"`
	}{"sha1", w(0), w(1), w(2), w(3), w(4),
		strconv.FormatUint(bits&0xffffffff, 10), strconv.FormatUint(bits>>32, 10), "", "0"}
	j, _ := json.Marshal(v)
	return base64.StdEncoding.EncodeToString(j), nil
}

func (c *Client) ossAuth(ctx context.Context, s *uploadSession, meta string) (string, error) {
	body, err := json.Marshal(map[string]any{"auth_info": s.AuthInfo, "auth_meta": meta, "task_id": s.TaskID})
	if err != nil {
		return "", fmt.Errorf("quark: upload auth: %w", err)
	}
	var out struct {
		AuthKey string `json:"auth_key"`
	}
	if err := c.call(ctx, "upload auth", http.MethodPost, uploadAuthPath, nil, body, &out); err != nil {
		return "", err
	}
	if out.AuthKey == "" || strings.ContainsAny(out.AuthKey, "\r\n") {
		return "", errors.New("quark: upload auth: invalid auth_key")
	}
	return out.AuthKey, nil
}

func ossDate() string { return time.Now().UTC().Format(http.TimeFormat) }

func (c *Client) uploadPart(ctx context.Context, s *uploadSession, mimeType string, part int, chunk []byte, hctx string) (string, error) {
	date := ossDate()
	var meta strings.Builder
	fmt.Fprintf(&meta, "PUT\n\n%s\n%s\n", mimeType, date)
	if hctx != "" {
		fmt.Fprintf(&meta, "X-Oss-Hash-Ctx:%s\n", hctx)
	}
	fmt.Fprintf(&meta, "x-oss-date:%s\nx-oss-user-agent:%s\n/%s/%s?partNumber=%d&uploadId=%s",
		date, ossUserAgent, s.Bucket, s.ObjKey, part, s.UploadID)
	key, err := c.ossAuth(ctx, s, meta.String())
	if err != nil {
		return "", err
	}
	u := *s.ossBase
	u.RawQuery = url.Values{"partNumber": {strconv.Itoa(part)}, "uploadId": {s.UploadID}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), bytes.NewReader(chunk))
	if err != nil {
		return "", fmt.Errorf("quark: upload part %d: %w", part, err)
	}
	req.Header.Set("Authorization", key)
	req.Header.Set("Content-Type", mimeType)
	req.Header.Set("x-oss-date", date)
	req.Header.Set("x-oss-user-agent", ossUserAgent)
	if hctx != "" {
		req.Header.Set("X-Oss-Hash-Ctx", hctx)
	}
	resp, err := c.ossDo(req)
	if err != nil {
		return "", fmt.Errorf("quark: upload part %d: %w", part, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		// A previous attempt may have stored this part before its response
		// was lost; accept it only if its ETag matches our data.
		return partAlreadyExistETag(resp, part, chunk)
	}
	if resp.StatusCode != http.StatusOK {
		return "", httpErr("upload part "+strconv.Itoa(part), resp)
	}
	etag := resp.Header.Get("ETag")
	if etag == "" || strings.ContainsAny(etag, "<>&\r\n") {
		return "", fmt.Errorf("quark: upload part %d: missing or invalid ETag", part)
	}
	return etag, nil
}

func (c *Client) uploadHash(ctx context.Context, s *uploadSession, md5hex, sha1hex string) (bool, error) {
	body, _ := json.Marshal(map[string]string{"md5": md5hex, "sha1": sha1hex, "task_id": s.TaskID})
	var out struct {
		Finish bool `json:"finish"`
	}
	if err := c.call(ctx, "upload hash", http.MethodPost, uploadHashPath, nil, body, &out); err != nil {
		return false, err
	}
	return out.Finish, nil
}

func (c *Client) uploadComplete(ctx context.Context, s *uploadSession, etags []string) error {
	var x strings.Builder
	x.WriteString("<CompleteMultipartUpload>")
	for i, e := range etags {
		fmt.Fprintf(&x, "<Part><PartNumber>%d</PartNumber><ETag>%s</ETag></Part>", i+1, e)
	}
	x.WriteString("</CompleteMultipartUpload>")
	body := []byte(x.String())
	sum := md5.Sum(body)
	contentMD5 := base64.StdEncoding.EncodeToString(sum[:])

	callback := s.callback

	date := ossDate()
	meta := fmt.Sprintf("POST\n%s\napplication/xml\n%s\nx-oss-callback:%s\nx-oss-date:%s\nx-oss-user-agent:%s\n/%s/%s?uploadId=%s",
		contentMD5, date, callback, date, ossUserAgent, s.Bucket, s.ObjKey, s.UploadID)
	key, err := c.ossAuth(ctx, s, meta)
	if err != nil {
		// The complete request was not sent, but update/hash already was.
		return unconfirmedCommit(ctx, "upload commit auth", err)
	}
	u := *s.ossBase
	u.RawQuery = url.Values{"uploadId": {s.UploadID}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("quark: upload commit: %w", err)
	}
	req.Header.Set("Authorization", key)
	req.Header.Set("Content-MD5", contentMD5)
	req.Header.Set("Content-Type", "application/xml")
	req.Header.Set("Referer", panOrigin+"/")
	req.Header.Set("x-oss-callback", callback)
	req.Header.Set("x-oss-date", date)
	req.Header.Set("x-oss-user-agent", ossUserAgent)
	resp, err := c.ossDo(req)
	if err != nil {
		if errors.Is(err, ErrUntrustedURL) {
			return fmt.Errorf("%w: upload commit not sent: %v", ErrUnconfirmed, err)
		}
		return unconfirmedCommit(ctx, "upload commit", fmt.Errorf("quark: upload commit: %w", err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 203 CallbackFailed: object assembled, callback outcome unknown.
		return unconfirmedCommit(ctx, "upload commit", httpErr("upload commit", resp))
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONBytes+1))
	if err != nil {
		return unconfirmedCommit(ctx, "upload commit response", err)
	}
	if len(data) > maxJSONBytes {
		return unconfirmedCommit(ctx, "upload commit response", errors.New("response too large"))
	}
	var root struct{ XMLName xml.Name }
	if xml.Unmarshal(data, &root) == nil && root.XMLName.Local == "Error" {
		return unconfirmedCommit(ctx, "upload commit response", errors.New("OSS returned an error response"))
	}
	return nil
}

// ossDo sends a signed OSS request: never with the account cookie, never
// following redirects, and only to a host validated by ossObjectURL.
func (c *Client) ossDo(req *http.Request) (*http.Response, error) {
	if !isQuarkURL(req.URL) {
		return nil, ErrUntrustedURL
	}
	req.Header.Del("Cookie")
	hc := &http.Client{
		Transport: c.hc.Transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, scrub(err)
	}
	return resp, nil
}
