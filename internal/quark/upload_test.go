package quark

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/tls"
	"encoding"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMain makes sure no test can reach the real per-user upload-state
// directory: every upload test must install an in-memory store.
func TestMain(m *testing.M) {
	openUploadState = func(string) (uploadStateStore, error) {
		return nil, errors.New("test must install an in-memory upload state store")
	}
	os.Exit(m.Run())
}

// memUploads is an in-memory upload state store shared by every client
// created for one upMock, so a second attempt sees the first one's record.
type memUploads struct {
	mu   sync.Mutex
	data map[string][]byte
}

type memUploadHandle struct {
	s   *memUploads
	key string
}

func (h memUploadHandle) Load() ([]byte, error) {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	d, ok := h.s.data[h.key]
	if !ok {
		return nil, os.ErrNotExist
	}
	return bytes.Clone(d), nil
}

func (h memUploadHandle) Save(d []byte) error {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	h.s.data[h.key] = bytes.Clone(d)
	return nil
}

func (h memUploadHandle) Clear() error {
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	delete(h.s.data, h.key)
	return nil
}

func (memUploadHandle) Close() error { return nil }

func (s *memUploads) records() []resumeState {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []resumeState
	for _, d := range s.data {
		var rs resumeState
		if err := json.Unmarshal(d, &rs); err != nil {
			panic(err)
		}
		out = append(out, rs)
	}
	return out
}

// upMock is a fake Quark API + OSS endpoint. All hosts are routed to one
// local TLS server, so the production host checks (isQuarkURL) stay active.
type upMock struct {
	t *testing.T

	mu    sync.Mutex
	steps []string
	parts map[int][]byte
	ctxs  map[int]string
	metas []string

	uploadURL  string
	bucket     string
	partSize   any
	callback   any
	rapid      bool
	existing   []entry
	fail       map[string]int // step -> http status (or 0 = api error)
	blockPart  int            // block this part until the request is canceled
	partReady  chan struct{}
	commitBody []byte
	hashBody   map[string]string
	finishBody map[string]string
	ossCookie  bool
	fileName   string
	fileSize   int64
	partThread int
	state      *memUploads
}

const testBucket = "ul-bucket"

func newUpMock(t *testing.T) *upMock {
	m := &upMock{t: t, parts: map[int][]byte{}, ctxs: map[int]string{}, fail: map[string]int{},
		uploadURL: "http://pds.quark.cn", bucket: testBucket, partSize: 64,
		callback: map[string]any{"callbackUrl": "https://x.quark.cn/cb", "callbackBody": "a=${x}"},
		fileName: "a.bin", fileSize: 150, partThread: 3,
		state: &memUploads{data: map[string][]byte{}}}
	prev := openUploadState
	openUploadState = func(key string) (uploadStateStore, error) {
		return memUploadHandle{s: m.state, key: key}, nil
	}
	t.Cleanup(func() { openUploadState = prev })
	return m
}

func (m *upMock) resetSteps() {
	m.mu.Lock()
	m.steps = nil
	m.mu.Unlock()
}

func (m *upMock) step(s string) {
	m.mu.Lock()
	m.steps = append(m.steps, s)
	m.mu.Unlock()
}

func (m *upMock) Steps() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return strings.Join(m.steps, ",")
}

func (m *upMock) failed(w http.ResponseWriter, s string) bool {
	code, ok := m.fail[s]
	if !ok {
		return false
	}
	if code == 0 {
		writeJSON(w, 200, map[string]any{"status": 400, "code": 999, "message": "boom", "data": nil})
	} else {
		w.WriteHeader(code)
		io.WriteString(w, "<Error>denied</Error>")
	}
	return true
}

func (m *upMock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if host == "drive-pc.quark.cn" {
		if r.Header.Get("Cookie") != testCookie {
			m.t.Errorf("api %s without cookie", r.URL.Path)
		}
		m.api(w, r)
		return
	}
	if r.Header.Get("Cookie") != "" {
		m.ossCookie = true
	}
	if host != testBucket+".pds.quark.cn" {
		m.t.Errorf("unexpected host %q", host)
		w.WriteHeader(404)
		return
	}
	m.oss(w, r)
}

func (m *upMock) api(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	if r.Method == http.MethodPost {
		json.NewDecoder(r.Body).Decode(&body)
	}
	ok := func(data any) { writeJSON(w, 200, map[string]any{"status": 200, "code": 0, "data": data}) }
	switch r.URL.Path {
	case listPath:
		m.step("list")
		list := []map[string]any{}
		if r.URL.Query().Get("pdir_fid") == rootID {
			list = append(list, map[string]any{"fid": "d1", "file_name": "docs", "dir": true})
		} else {
			m.mu.Lock()
			for _, e := range m.existing {
				list = append(list, map[string]any{"fid": e.fid, "file_name": e.name, "dir": e.dir, "size": e.size})
			}
			m.mu.Unlock()
		}
		ok(map[string]any{"list": list})
	case uploadPrePath:
		m.step("pre")
		if body["pdir_fid"] != "d1" || body["file_name"] != m.fileName || body["size"] != float64(m.fileSize) ||
			body["format_type"] != "application/octet-stream" {
			m.t.Errorf("bad pre body %v", body)
		}
		if m.failed(w, "pre") {
			return
		}
		writeJSON(w, 200, map[string]any{"status": 200, "code": 0,
			"data": map[string]any{"task_id": "T1", "bucket": m.bucket, "obj_key": "ul/obj 1",
				"upload_id": "U1", "upload_url": m.uploadURL, "auth_info": "AI", "callback": m.callback},
			"metadata": map[string]any{"part_size": m.partSize, "part_thread": m.partThread}})
	case uploadAuthPath:
		m.step("auth")
		if body["task_id"] != "T1" || body["auth_info"] != "AI" {
			m.t.Errorf("bad auth body %v", body)
		}
		m.mu.Lock()
		m.metas = append(m.metas, body["auth_meta"].(string))
		m.mu.Unlock()
		if m.failed(w, "auth") {
			return
		}
		ok(map[string]any{"auth_key": "OSS key:" + strconv.Itoa(len(m.metas))})
	case uploadHashPath:
		m.step("hash")
		m.hashBody = map[string]string{}
		for k, v := range body {
			m.hashBody[k], _ = v.(string)
		}
		if m.failed(w, "hash") {
			return
		}
		ok(map[string]any{"finish": m.rapid})
	case uploadFinishPath:
		m.step("finish")
		m.finishBody = map[string]string{}
		for k, v := range body {
			m.finishBody[k], _ = v.(string)
		}
		if m.failed(w, "finish") {
			return
		}
		// The committed file becomes visible to the post-finish check.
		m.mu.Lock()
		m.existing = append(m.existing, entry{fid: "F1", name: m.fileName, size: m.fileSize})
		m.mu.Unlock()
		ok(map[string]any{"fid": "F1"})
	default:
		m.t.Errorf("unexpected api %s", r.URL.Path)
		w.WriteHeader(404)
	}
}

func (m *upMock) oss(w http.ResponseWriter, r *http.Request) {
	if r.URL.EscapedPath() != "/ul/obj%201" || r.URL.Query().Get("uploadId") != "U1" {
		m.t.Errorf("bad oss url %s", r.URL)
	}
	if r.Header.Get("x-oss-user-agent") != ossUserAgent || r.Header.Get("x-oss-date") == "" ||
		!strings.HasPrefix(r.Header.Get("Authorization"), "OSS key:") {
		m.t.Errorf("bad oss headers %v", r.Header)
	}
	body, _ := io.ReadAll(r.Body)
	switch r.Method {
	case http.MethodPut:
		n, _ := strconv.Atoi(r.URL.Query().Get("partNumber"))
		m.step("put" + strconv.Itoa(n))
		if n == m.blockPart {
			if m.partReady != nil {
				close(m.partReady)
			}
			<-r.Context().Done()
			return
		}
		if m.failed(w, "put"+strconv.Itoa(n)) {
			return
		}
		m.mu.Lock()
		m.parts[n] = body
		m.ctxs[n] = r.Header.Get("X-Oss-Hash-Ctx")
		m.mu.Unlock()
		if m.fail["noetag"] == 0 {
			w.Header().Set("ETag", `"E`+strconv.Itoa(n)+`"`)
		}
	case http.MethodPost:
		m.step("commit")
		m.commitBody = body
		sum := md5.Sum(body)
		if r.Header.Get("Content-MD5") != base64.StdEncoding.EncodeToString(sum[:]) ||
			r.Header.Get("Content-Type") != "application/xml" || r.Header.Get("x-oss-callback") == "" {
			m.t.Errorf("bad commit headers %v", r.Header)
		}
		if m.failed(w, "commit") {
			return
		}
		io.WriteString(w, "<CompleteMultipartUploadResult/>")
	}
}

func newUpClient(t *testing.T, m *upMock) *Client {
	srv := httptest.NewTLSServer(m)
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()
	c := New(testCookie)
	c.pollInterval = time.Millisecond // keep failed post-finish checks fast
	c.hc.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}
	return c // baseURL and trust policy are the production ones
}

func testFile(t *testing.T) (string, []byte) {
	data := make([]byte, 150)
	for i := range data {
		data[i] = byte(i*7 + 3)
	}
	p := filepath.Join(t.TempDir(), "a.bin")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p, data
}

func TestUploadSuccess(t *testing.T) {
	m := newUpMock(t)
	c := newUpClient(t, m)
	p, data := testFile(t)
	if err := c.Upload(context.Background(), p, "/docs"); err != nil {
		t.Fatal(err)
	}
	// Parts are sent concurrently, so only the phases are ordered.
	steps := strings.Split(m.Steps(), ",")
	if len(steps) != 14 || strings.Join(steps[:3], ",") != "list,list,pre" ||
		strings.Join(steps[9:], ",") != "hash,auth,commit,finish,list" {
		t.Fatalf("steps %s", m.Steps())
	}
	mid := slices.Clone(steps[3:9])
	slices.Sort(mid)
	if strings.Join(mid, ",") != "auth,auth,auth,put1,put2,put3" {
		t.Fatalf("part steps %s", m.Steps())
	}
	if m.ossCookie {
		t.Fatal("cookie sent to OSS")
	}
	if !bytes.Equal(bytes.Join([][]byte{m.parts[1], m.parts[2], m.parts[3]}, nil), data) || len(m.parts[1]) != 64 {
		t.Fatal("part bodies mismatch")
	}
	md5sum, sha1sum := md5.Sum(data), sha1.Sum(data)
	if m.hashBody["md5"] != hex.EncodeToString(md5sum[:]) || m.hashBody["sha1"] != hex.EncodeToString(sha1sum[:]) || m.hashBody["task_id"] != "T1" {
		t.Fatalf("hash body %v", m.hashBody)
	}
	if m.finishBody["obj_key"] != "ul/obj 1" || m.finishBody["task_id"] != "T1" {
		t.Fatalf("finish body %v", m.finishBody)
	}
	wantXML := `<CompleteMultipartUpload><Part><PartNumber>1</PartNumber><ETag>"E1"</ETag></Part><Part><PartNumber>2</PartNumber><ETag>"E2"</ETag></Part><Part><PartNumber>3</PartNumber><ETag>"E3"</ETag></Part></CompleteMultipartUpload>`
	if string(m.commitBody) != wantXML {
		t.Fatalf("commit body %s", m.commitBody)
	}
	// Signed metadata mirrors the actual requests (auth order is not fixed).
	metaFor := func(suffix string) string {
		t.Helper()
		for _, s := range m.metas {
			if strings.HasSuffix(s, suffix) {
				return s
			}
		}
		t.Fatalf("no signed meta ending in %q: %q", suffix, m.metas)
		return ""
	}
	meta1 := metaFor("\n/" + testBucket + "/ul/obj 1?partNumber=1&uploadId=U1")
	if !strings.HasPrefix(meta1, "PUT\n\napplication/octet-stream\n") || strings.Contains(meta1, "X-Oss-Hash-Ctx") {
		t.Fatalf("meta1 %q", meta1)
	}
	if meta2 := metaFor("?partNumber=2&uploadId=U1"); !strings.Contains(meta2, "X-Oss-Hash-Ctx:"+m.ctxs[2]+"\n") {
		t.Fatalf("meta2 %q", meta2)
	}
	if cm := metaFor("/" + testBucket + "/ul/obj 1?uploadId=U1"); !strings.HasPrefix(cm, "POST\n") || !strings.Contains(cm, "\napplication/xml\n") {
		t.Fatalf("commit meta %q", cm)
	}
	if len(m.state.records()) != 0 {
		t.Fatal("resume state not cleared after a verified upload")
	}
	// Each hash ctx must be the SHA-1 state over all previous parts.
	if m.ctxs[1] != "" {
		t.Fatal("part 1 must not carry hash ctx")
	}
	for n, prefix := range map[int]int{2: 64, 3: 128} {
		checkHashCtx(t, m.ctxs[n], data, prefix)
	}
}

// checkHashCtx resumes SHA-1 from the transmitted state and verifies it
// yields the digest of the whole file.
func checkHashCtx(t *testing.T, enc string, data []byte, prefix int) {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]string
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if v["hash_type"] != "sha1" || v["Nh"] != "0" || v["Nl"] != strconv.Itoa(prefix*8) || v["data"] != "" || v["num"] != "0" {
		t.Fatalf("ctx %v", v)
	}
	st := make([]byte, 0, 96)
	st = append(st, "sha\x01"...)
	for _, k := range []string{"h0", "h1", "h2", "h3", "h4"} {
		n, err := strconv.ParseUint(v[k], 10, 32)
		if err != nil {
			t.Fatal(err)
		}
		st = binary.BigEndian.AppendUint32(st, uint32(n))
	}
	st = append(st, make([]byte, 64)...)
	st = binary.BigEndian.AppendUint64(st, uint64(prefix))
	h := sha1.New()
	if err := h.(encoding.BinaryUnmarshaler).UnmarshalBinary(st); err != nil {
		t.Fatal(err)
	}
	h.Write(data[prefix:])
	want := sha1.Sum(data)
	if !bytes.Equal(h.Sum(nil), want[:]) {
		t.Fatalf("hash ctx at %d does not chain", prefix)
	}
}

func TestUploadRapidSkipsCommit(t *testing.T) {
	m := newUpMock(t)
	m.rapid = true
	p, _ := testFile(t)
	if err := newUpClient(t, m).Upload(context.Background(), p, "/docs"); err != nil {
		t.Fatal(err)
	}
	if got := m.Steps(); strings.Contains(got, "commit") || !strings.HasSuffix(got, "hash,finish,list") {
		t.Fatalf("steps %s", got)
	}
}

func TestUploadErrorsNeverFinish(t *testing.T) {
	cases := []struct {
		name  string
		setup func(m *upMock)
		last  string // last step expected to have happened
		is    error
	}{
		{"pre api error", func(m *upMock) { m.fail["pre"] = 0 }, "pre", nil},
		{"auth error", func(m *upMock) { m.fail["auth"] = 0 }, "auth", nil},
		{"part http error", func(m *upMock) { m.fail["put2"] = 500 }, "put2", nil},
		{"missing etag", func(m *upMock) { m.fail["noetag"] = 1 }, "put", nil}, // any concurrent part may fail first
		{"hash error", func(m *upMock) { m.fail["hash"] = 0 }, "hash", nil},
		{"commit error", func(m *upMock) { m.fail["commit"] = 403 }, "commit", nil},
		{"oss redirect not followed", func(m *upMock) { m.fail["put1"] = 307 }, "put1", nil},
		{"untrusted upload host", func(m *upMock) { m.uploadURL = "https://evil.example.com" }, "pre", ErrUntrustedURL},
		{"ip upload host", func(m *upMock) { m.uploadURL = "169.254.169.254" }, "pre", ErrUntrustedURL},
		{"upload url with port", func(m *upMock) { m.uploadURL = "https://pds.quark.cn:8443" }, "pre", ErrUntrustedURL},
		{"upload url userinfo", func(m *upMock) { m.uploadURL = "https://a@pds.quark.cn" }, "pre", ErrUntrustedURL},
		{"bad scheme", func(m *upMock) { m.uploadURL = "ftp://pds.quark.cn" }, "pre", ErrUntrustedURL},
		{"bucket injection", func(m *upMock) { m.bucket = "evil.com/x" }, "pre", ErrUntrustedURL},
		{"unaligned part size", func(m *upMock) { m.partSize = 100 }, "pre", ErrUnsupported},
		{"missing part size", func(m *upMock) { m.partSize = 0 }, "pre", nil},
		{"string callback", func(m *upMock) { m.callback = "cb" }, "pre", ErrUnsupported},
		{"name exists", func(m *upMock) { m.existing = []entry{{fid: "x", name: "a.bin"}} }, "list", ErrExists},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newUpMock(t)
			tc.setup(m)
			p, _ := testFile(t)
			err := newUpClient(t, m).Upload(context.Background(), p, "/docs")
			if err == nil {
				t.Fatal("expected error")
			}
			if tc.is != nil && !errors.Is(err, tc.is) {
				t.Fatalf("want %v, got %v", tc.is, err)
			}
			steps := m.Steps()
			if strings.Contains(steps, "finish") {
				t.Fatalf("finish called after error: %s", steps)
			}
			if strings.HasPrefix(tc.last, "put") {
				// Other parts run concurrently and may finish later.
				if !strings.Contains(steps, tc.last) || strings.Contains(steps, "hash") {
					t.Fatalf("steps %s, want %s and no commit phase", steps, tc.last)
				}
			} else if !strings.HasSuffix(steps, tc.last) {
				t.Fatalf("steps %s, want last %s", steps, tc.last)
			}
			if m.ossCookie {
				t.Fatal("cookie sent to OSS")
			}
		})
	}
}

// A failed finish leaves the remote state unknown: the error is
// ErrUnconfirmed and the record stays "finalizing", so a retry sends nothing.
func TestUploadFinishError(t *testing.T) {
	m := newUpMock(t)
	m.fail["finish"] = 0
	p, _ := testFile(t)
	if err := newUpClient(t, m).Upload(context.Background(), p, "/docs"); !errors.Is(err, ErrUnconfirmed) {
		t.Fatalf("want ErrUnconfirmed, got %v", err)
	}
	recs := m.state.records()
	if len(recs) != 1 || recs[0].Phase != uploadPhaseFinalizing || len(recs[0].Parts) != 3 {
		t.Fatalf("records %+v", recs)
	}
	delete(m.fail, "finish")
	m.resetSteps()
	if err := newUpClient(t, m).Upload(context.Background(), p, "/docs"); !errors.Is(err, ErrUnconfirmed) {
		t.Fatalf("retry: want ErrUnconfirmed, got %v", err)
	}
	if s := m.Steps(); strings.Contains(s, "pre") || strings.Contains(s, "put") || strings.Contains(s, "finish") {
		t.Fatalf("retry after finalizing sent upload requests: %s", s)
	}
	if len(m.state.records()) != 1 {
		t.Fatal("finalizing record removed")
	}
}

// An interrupted upload resumes with the recorded session and only sends
// the parts that were not accepted before.
func TestUploadResume(t *testing.T) {
	m := newUpMock(t)
	m.partThread = 1 // serial parts: part 1 is accepted, part 2 fails
	m.fail["put2"] = 403
	p, data := testFile(t)
	if err := newUpClient(t, m).Upload(context.Background(), p, "/docs"); err == nil {
		t.Fatal("expected error")
	}
	recs := m.state.records()
	if len(recs) != 1 || recs[0].Phase != uploadPhaseUploading || len(recs[0].Parts) != 1 || recs[0].Parts[0].N != 1 {
		t.Fatalf("records after failure %+v", recs)
	}

	delete(m.fail, "put2")
	m.resetSteps()
	var first []int64
	err := newUpClient(t, m).UploadProgress(context.Background(), p, "/docs", func(done, total int64) {
		if first == nil {
			first = []int64{done, total}
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := m.Steps(), "list,list,auth,put2,auth,put3,hash,auth,commit,finish,list"; got != want {
		t.Fatalf("resume steps\n got %s\nwant %s", got, want)
	}
	if !slices.Equal(first, []int64{64, 150}) {
		t.Fatalf("first progress %v, want resumed bytes", first)
	}
	if !bytes.Equal(bytes.Join([][]byte{m.parts[1], m.parts[2], m.parts[3]}, nil), data) {
		t.Fatal("part bodies mismatch")
	}
	checkHashCtx(t, m.ctxs[3], data, 128)
	if !strings.Contains(string(m.commitBody), `<PartNumber>1</PartNumber><ETag>"E1"</ETag>`) {
		t.Fatalf("commit body %s", m.commitBody)
	}
	if len(m.state.records()) != 0 {
		t.Fatal("resume state not cleared")
	}
}

// A same-size content change after a partial upload is refused before any
// further request, and the record is kept.
func TestUploadResumeFileChanged(t *testing.T) {
	m := newUpMock(t)
	m.partThread = 1
	m.fail["put2"] = 403
	p, data := testFile(t)
	if err := newUpClient(t, m).Upload(context.Background(), p, "/docs"); err == nil {
		t.Fatal("expected error")
	}
	data[0] ^= 0xff
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	delete(m.fail, "put2")
	m.resetSteps()
	if err := newUpClient(t, m).Upload(context.Background(), p, "/docs"); !errors.Is(err, ErrFileChanged) {
		t.Fatalf("want ErrFileChanged, got %v", err)
	}
	if s := m.Steps(); strings.Contains(s, "pre") || strings.Contains(s, "auth") || strings.Contains(s, "put") {
		t.Fatalf("changed file sent upload requests: %s", s)
	}
	if len(m.state.records()) != 1 {
		t.Fatal("record not kept")
	}
}

func TestUploadEmpty(t *testing.T) {
	for _, rapid := range []bool{true, false} {
		t.Run(strconv.FormatBool(rapid), func(t *testing.T) {
			m := newUpMock(t)
			m.rapid = rapid
			m.fileName, m.fileSize = "e.bin", 0
			empty := filepath.Join(t.TempDir(), "e.bin")
			if err := os.WriteFile(empty, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			err := newUpClient(t, m).Upload(context.Background(), empty, "/docs")
			if rapid {
				if err != nil {
					t.Fatal(err)
				}
				if s := m.Steps(); s != "list,list,pre,hash,finish,list" {
					t.Fatalf("steps %s", s)
				}
			} else {
				if !errors.Is(err, ErrUnconfirmed) {
					t.Fatalf("want ErrUnconfirmed, got %v", err)
				}
				if s := m.Steps(); s != "list,list,pre,hash" {
					t.Fatalf("steps %s", s)
				}
			}
			md5sum, sha1sum := md5.Sum(nil), sha1.Sum(nil)
			if m.hashBody["md5"] != hex.EncodeToString(md5sum[:]) || m.hashBody["sha1"] != hex.EncodeToString(sha1sum[:]) {
				t.Fatalf("hash body %v", m.hashBody)
			}
			if len(m.state.records()) != 0 {
				t.Fatal("empty upload left a resume record")
			}
		})
	}
}

func TestUploadTimeout(t *testing.T) {
	m := newUpMock(t)
	m.blockPart = 2
	p, _ := testFile(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := newUpClient(t, m).Upload(ctx, p, "/docs")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded, got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("timeout not honored")
	}
	// Part 3 may be in flight concurrently; nothing may be committed.
	if s := m.Steps(); strings.Contains(s, "hash") || strings.Contains(s, "commit") || strings.Contains(s, "finish") {
		t.Fatalf("continued after timeout: %s", s)
	}
}

func TestUploadCancel(t *testing.T) {
	m := newUpMock(t)
	m.blockPart = 3
	m.partReady = make(chan struct{})
	p, _ := testFile(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-m.partReady; cancel() }()
	err := newUpClient(t, m).Upload(ctx, p, "/docs")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want canceled, got %v", err)
	}
	if s := m.Steps(); strings.Contains(s, "hash") || strings.Contains(s, "finish") {
		t.Fatalf("continued after cancel: %s", s)
	}
}

func TestUploadPrecanceled(t *testing.T) {
	m := newUpMock(t)
	p, _ := testFile(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := newUpClient(t, m).Upload(ctx, p, "/docs"); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if s := m.Steps(); s != "" {
		t.Fatalf("requests sent: %s", s)
	}
}

func TestUploadLocalErrors(t *testing.T) {
	m := newUpMock(t)
	c := newUpClient(t, m)
	dir := t.TempDir()
	if err := c.Upload(context.Background(), dir, "/docs"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("dir: %v", err)
	}
	if err := c.Upload(context.Background(), filepath.Join(dir, "missing"), "/docs"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing: %v", err)
	}
	if s := m.Steps(); s != "" {
		t.Fatalf("requests sent: %s", s)
	}
	p, _ := testFile(t)
	if err := c.Upload(context.Background(), p, "/nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("remote dir: %v", err)
	}
}

func TestOSSObjectURL(t *testing.T) {
	u, err := ossObjectURL("pds.quark.cn", "b-1", "a/b c")
	if err != nil || u.String() != "https://b-1.pds.quark.cn/a/b%20c" {
		t.Fatalf("%v %v", u, err)
	}
	for _, k := range []string{"", "/a", "a/../b", "a?x", "a#x", "a//b", "a\\b", "a%2f"} {
		if _, err := ossObjectURL("pds.quark.cn", "b-1", k); !errors.Is(err, ErrUntrustedURL) {
			t.Errorf("objkey %q accepted", k)
		}
	}
	for _, h := range []string{"pds.quark.cn.evil.com", "quark.cn@evil.com", "https://pds.quark.cn/path", "https://pds.quark.cn?x=1"} {
		if _, err := ossObjectURL(h, "b-1", "k"); !errors.Is(err, ErrUntrustedURL) {
			t.Errorf("upload_url %q accepted", h)
		}
	}
}
