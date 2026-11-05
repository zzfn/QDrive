package quark

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"encoding"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
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
//  2. for each part, in order:
//     POST upload/auth   (signs an OSS string-to-sign) -> auth_key
//     PUT  OSS ?partNumber=N&uploadId=..  (X-Oss-Hash-Ctx for N>=2) -> ETag
//  3. POST update/hash     (whole-file md5+sha1) -> finish (already stored)
//  4. unless finish: POST upload/auth + POST OSS ?uploadId=.. (complete)
//  5. POST upload/finish
//
// Only a fresh, sequential, single-file upload into an existing directory is
// implemented. Resume, parallel parts, directory creation and overwrite are
// not.
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

	partSize int64
	callback string   // base64 of compact callback JSON object
	ossBase  *url.URL // https://bucket.host/objkey (escaped)
}

// Upload uploads the regular file at localPath into the existing remote
// directory remoteDir, keeping the local base name. It never overwrites.
func (c *Client) Upload(ctx context.Context, localPath string, remoteDir string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("quark: upload: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("quark: upload: %w", err)
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("%w: not a regular file: %s", ErrUnsupported, localPath)
	}
	size := st.Size()
	if size == 0 {
		return fmt.Errorf("%w: empty file", ErrUnsupported)
	}
	name := filepath.Base(localPath)
	if name == "" || name == "." || name == string(filepath.Separator) || strings.ContainsAny(name, "/\\") {
		return fmt.Errorf("quark: upload: invalid file name %q", name)
	}

	dir, err := c.resolve(ctx, remoteDir)
	if err != nil {
		return err
	}
	if !dir.IsDir {
		return fmt.Errorf("%w: %s", ErrNotDir, remoteDir)
	}
	entries, err := c.listDir(ctx, dir.ID)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name == name {
			return fmt.Errorf("%w: %s", ErrExists, name)
		}
	}

	mimeType := mime.TypeByExtension(filepath.Ext(name))
	if mimeType == "" {
		mimeType = "application/octet-stream"
	}

	s, err := c.uploadPre(ctx, name, mimeType, size, dir.ID)
	if err != nil {
		return err
	}
	if n := (size + s.partSize - 1) / s.partSize; n > maxParts {
		return fmt.Errorf("%w: %d parts exceeds %d", ErrUnsupported, n, maxParts)
	}

	md5h, sha1h := md5.New(), sha1.New()
	chain := sha1.New() // state before current part, for X-Oss-Hash-Ctx
	buf := make([]byte, min(s.partSize, size))
	var etags []string
	var done int64
	for part := 1; done < size; part++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := min(s.partSize, size-done)
		chunk := buf[:n]
		if _, err := io.ReadFull(f, chunk); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				return ErrFileChanged
			}
			return fmt.Errorf("quark: upload: read: %w", err)
		}
		var hctx string
		if part >= 2 {
			if hctx, err = sha1Ctx(chain); err != nil {
				return err
			}
		}
		etag, err := c.uploadPart(ctx, s, mimeType, part, chunk, hctx)
		if err != nil {
			return err
		}
		etags = append(etags, etag)
		md5h.Write(chunk)
		sha1h.Write(chunk)
		chain.Write(chunk)
		done += n
	}
	// The file must not have grown or been replaced while reading.
	if extra, _ := f.Read(make([]byte, 1)); extra != 0 {
		return ErrFileChanged
	}
	if st2, err := f.Stat(); err != nil || st2.Size() != size || !st2.ModTime().Equal(st.ModTime()) {
		return ErrFileChanged
	}

	finished, err := c.uploadHash(ctx, s, hex.EncodeToString(md5h.Sum(nil)), hex.EncodeToString(sha1h.Sum(nil)))
	if err != nil {
		return err
	}
	if !finished {
		if err := c.uploadComplete(ctx, s, etags); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var out json.RawMessage
	body, _ := json.Marshal(map[string]string{"obj_key": s.ObjKey, "task_id": s.TaskID})
	return c.call(ctx, "upload finish", http.MethodPost, uploadFinishPath, nil, body, &out)
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
			PartSize json.Number `json:"part_size"`
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
		return err
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
		return fmt.Errorf("quark: upload commit: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return httpErr("upload commit", resp)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxJSONBytes))
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
