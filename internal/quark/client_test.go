package quark

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testCookie = "__pus=secret; __puus=secret2"

type entry struct {
	fid  string
	name string
	dir  bool
	size int64
}

type fakeDrive struct {
	t        *testing.T
	dirs     map[string][]entry
	content  map[string]string
	dlBase   string // base url for download links
	apiErr   map[string]bool
	listHits atomic.Int32
}

func (d *fakeDrive) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(listPath, func(w http.ResponseWriter, r *http.Request) {
		d.listHits.Add(1)
		d.checkCommon(r)
		q := r.URL.Query()
		pdir := q.Get("pdir_fid")
		if d.apiErr[pdir] {
			writeJSON(w, 200, map[string]any{"status": 200, "code": 41010, "message": "no permission", "data": nil})
			return
		}
		page, _ := strconv.Atoi(q.Get("_page"))
		size, _ := strconv.Atoi(q.Get("_size"))
		if page < 1 || size < 1 {
			d.t.Errorf("bad paging %q", r.URL.RawQuery)
		}
		all := d.dirs[pdir]
		start := (page - 1) * size
		var list []map[string]any
		for i := start; i < start+size && i < len(all); i++ {
			e := all[i]
			list = append(list, map[string]any{"fid": e.fid, "file_name": e.name, "dir": e.dir, "size": e.size})
		}
		if list == nil {
			list = []map[string]any{}
		}
		writeJSON(w, 200, map[string]any{"status": 200, "code": 0, "message": "ok",
			"data":     map[string]any{"list": list},
			"metadata": map[string]any{"_total": len(all)}})
	})
	mux.HandleFunc(downloadPath, func(w http.ResponseWriter, r *http.Request) {
		d.checkCommon(r)
		if r.Method != http.MethodPost {
			d.t.Errorf("download method %s", r.Method)
		}
		var body struct{ Fids []string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Fids) != 1 {
			d.t.Errorf("bad download body: %v", err)
		}
		writeJSON(w, 200, map[string]any{"status": 200, "code": 0,
			"data": []map[string]any{{"fid": body.Fids[0], "download_url": d.dlBase + "/blob/" + body.Fids[0] + "?sig=x"}}})
	})
	mux.HandleFunc("/blob/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != testCookie {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		c, ok := d.content[strings.TrimPrefix(r.URL.Path, "/blob/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(c))
	})
	return mux
}

func (d *fakeDrive) checkCommon(r *http.Request) {
	if got := r.Header.Get("Cookie"); got != testCookie {
		d.t.Errorf("cookie not sent to api")
	}
	q := r.URL.Query()
	if q.Get("pr") != "ucpro" || q.Get("fr") != "pc" {
		d.t.Errorf("missing pr/fr: %s", r.URL.RawQuery)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// newTestClient points the client at srv and trusts only srv's host.
func newTestClient(srv *httptest.Server) *Client {
	c := New(testCookie)
	c.baseURL = srv.URL
	c.hc.Transport = srv.Client().Transport
	host := mustURL(srv.URL).Host
	c.trusted = func(u *url.URL) bool { return u != nil && u.Host == host }
	return c
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

func setup(t *testing.T) (*fakeDrive, *httptest.Server, *Client) {
	d := &fakeDrive{t: t, dirs: map[string][]entry{}, content: map[string]string{}, apiErr: map[string]bool{}}
	srv := httptest.NewServer(d.handler())
	t.Cleanup(srv.Close)
	d.dlBase = srv.URL
	return d, srv, newTestClient(srv)
}

func TestListRootPaginated(t *testing.T) {
	d, _, c := setup(t)
	n := pageSize*2 + 7
	for i := 0; i < n; i++ {
		d.dirs["0"] = append(d.dirs["0"], entry{fid: fmt.Sprintf("f%d", i), name: fmt.Sprintf("n%d", i), size: int64(i)})
	}
	for _, p := range []string{"/", "", "//"} {
		d.listHits.Store(0)
		files, err := c.List(context.Background(), p)
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != n {
			t.Fatalf("got %d files want %d", len(files), n)
		}
		if files[n-1] != (File{ID: fmt.Sprintf("f%d", n-1), Name: fmt.Sprintf("n%d", n-1), Size: int64(n - 1)}) {
			t.Fatalf("bad last file %+v", files[n-1])
		}
		if h := d.listHits.Load(); h != 3 {
			t.Fatalf("expected 3 pages, got %d", h)
		}
	}
}

func TestListExactPageBoundary(t *testing.T) {
	d, _, c := setup(t)
	for i := 0; i < pageSize; i++ {
		d.dirs["0"] = append(d.dirs["0"], entry{fid: fmt.Sprintf("f%d", i), name: fmt.Sprintf("n%d", i)})
	}
	files, err := c.List(context.Background(), "/")
	if err != nil || len(files) != pageSize {
		t.Fatalf("len=%d err=%v", len(files), err)
	}
	if h := d.listHits.Load(); h != 2 {
		t.Fatalf("expected 2 requests, got %d", h)
	}
}

func TestListNestedPath(t *testing.T) {
	d, _, c := setup(t)
	d.dirs["0"] = []entry{{fid: "a", name: "docs", dir: true}, {fid: "x", name: "readme.txt", size: 3}}
	d.dirs["a"] = []entry{{fid: "b", name: "sub dir", dir: true}}
	d.dirs["b"] = []entry{{fid: "c", name: "file.bin", size: 42}}

	files, err := c.List(context.Background(), "/docs/sub dir/")
	if err != nil {
		t.Fatal(err)
	}
	want := []File{{ID: "c", Name: "file.bin", Size: 42}}
	if len(files) != 1 || files[0] != want[0] {
		t.Fatalf("got %+v", files)
	}
	if _, err := c.List(context.Background(), "docs/./sub dir"); err != nil {
		t.Fatalf("relative path: %v", err)
	}
	if _, err := c.List(context.Background(), "/docs/missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	if _, err := c.List(context.Background(), "/readme.txt"); !errors.Is(err, ErrNotDir) {
		t.Fatalf("want ErrNotDir, got %v", err)
	}
	if _, err := c.List(context.Background(), "/readme.txt/x"); !errors.Is(err, ErrNotDir) {
		t.Fatalf("want ErrNotDir, got %v", err)
	}
	if _, err := c.List(context.Background(), "/docs/../etc"); err == nil {
		t.Fatal("expected error for ..")
	}
}

func TestAmbiguousName(t *testing.T) {
	d, _, c := setup(t)
	d.dirs["0"] = []entry{{fid: "a", name: "dup", dir: true}, {fid: "b", name: "dup", dir: true}}
	if _, err := c.List(context.Background(), "/dup"); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("want ErrAmbiguous, got %v", err)
	}
}

func TestBusinessError(t *testing.T) {
	d, _, c := setup(t)
	d.apiErr["0"] = true
	_, err := c.List(context.Background(), "/")
	var ae *APIError
	if !errors.As(err, &ae) || ae.Code != 41010 || ae.Message != "no permission" {
		t.Fatalf("want APIError, got %v", err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatal("cookie leaked in error")
	}
}

func TestHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("pdir_fid") {
		default:
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte("<html>bad gateway</html>"))
		}
	}))
	defer srv.Close()
	c := newTestClient(srv)
	_, err := c.List(context.Background(), "/")
	var he *HTTPError
	if !errors.As(err, &he) || he.StatusCode != 502 || !strings.Contains(he.Body, "bad gateway") {
		t.Fatalf("want HTTPError, got %v", err)
	}

	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 401, map[string]any{"status": 401, "code": 31001, "message": "require login"})
	}))
	defer srv2.Close()
	_, err = newTestClient(srv2).List(context.Background(), "/")
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 401 || ae.Code != 31001 {
		t.Fatalf("want APIError, got %v", err)
	}

	srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	defer srv3.Close()
	if _, err = newTestClient(srv3).List(context.Background(), "/"); err == nil {
		t.Fatal("expected json error")
	}
}

func TestContextCanceled(t *testing.T) {
	_, _, c := setup(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.List(ctx, "/"); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestDownload(t *testing.T) {
	d, _, c := setup(t)
	d.dirs["0"] = []entry{{fid: "a", name: "dir", dir: true}}
	d.dirs["a"] = []entry{{fid: "f1", name: "hello.txt", size: 11}}
	d.content["f1"] = "hello world"

	var buf bytes.Buffer
	if err := c.Download(context.Background(), "/dir/hello.txt", &buf); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "hello world" {
		t.Fatalf("got %q", buf.String())
	}
	if err := c.Download(context.Background(), "/dir", &buf); !errors.Is(err, ErrIsDir) {
		t.Fatalf("want ErrIsDir, got %v", err)
	}
	if err := c.Download(context.Background(), "/dir/nope", &buf); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestDownloadHTTPError(t *testing.T) {
	d, _, c := setup(t)
	d.dirs["0"] = []entry{{fid: "gone", name: "f"}}
	err := c.Download(context.Background(), "/f", &bytes.Buffer{})
	var he *HTTPError
	if !errors.As(err, &he) || he.StatusCode != 404 {
		t.Fatalf("want 404 HTTPError, got %v", err)
	}
}

func TestDownloadRefusesUntrustedHost(t *testing.T) {
	var leaked atomic.Bool
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != "" {
			leaked.Store(true)
		}
		w.Write([]byte("data"))
	}))
	defer evil.Close()

	d, _, c := setup(t)
	d.dlBase = evil.URL
	d.dirs["0"] = []entry{{fid: "f1", name: "f"}}
	d.content["f1"] = "x"
	err := c.Download(context.Background(), "/f", &bytes.Buffer{})
	if !errors.Is(err, ErrUntrustedURL) {
		t.Fatalf("want ErrUntrustedURL, got %v", err)
	}
	if leaked.Load() {
		t.Fatal("cookie leaked to untrusted host")
	}
}

func TestRedirectRejectsUntrustedHost(t *testing.T) {
	var gotCookie atomic.Value
	gotCookie.Store("unset")
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie.Store(r.Header.Get("Cookie"))
		w.Write([]byte("payload"))
	}))
	defer evil.Close()

	d, srv, c := setup(t)
	_ = srv
	d.dirs["0"] = []entry{{fid: "r1", name: "f"}}
	// Replace blob handler with a redirect by serving content via a wrapper.
	wrapped := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/blob/") {
			http.Redirect(w, r, evil.URL+"/x", http.StatusFound)
			return
		}
		d.handler().ServeHTTP(w, r)
	}))
	defer wrapped.Close()
	d.dlBase = wrapped.URL
	c = newTestClient(wrapped)

	var buf bytes.Buffer
	if err := c.Download(context.Background(), "/f", &buf); !errors.Is(err, ErrUntrustedURL) {
		t.Fatalf("want ErrUntrustedURL, got %v", err)
	}
	if v := gotCookie.Load().(string); v != "unset" {
		t.Fatal("untrusted host was contacted")
	}
	if buf.Len() != 0 {
		t.Fatalf("unexpected body %q", buf.String())
	}
}

func TestDownloadRejectsMismatchedFileSize(t *testing.T) {
	d, _, c := setup(t)
	d.dirs["0"] = []entry{{fid: "f", name: "file", size: 1000}}
	d.content["f"] = "short"
	var buf bytes.Buffer
	if err := c.Download(context.Background(), "/file", &buf); err == nil || !strings.Contains(err.Error(), "size mismatch") {
		t.Fatalf("expected size mismatch, got %v", err)
	}
}

// asyncServer serves a one-file root, answers download with the given
// handler and answers task polls with taskFn.
func asyncServer(t *testing.T, dl http.HandlerFunc, taskFn func(n int32, r *http.Request) any) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var polls atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") != testCookie {
			t.Errorf("cookie missing on %s", r.URL.Path)
		}
		switch r.URL.Path {
		case listPath:
			writeJSON(w, 200, map[string]any{"status": 200, "code": 0, "data": map[string]any{"list": []any{
				map[string]any{"fid": "f", "file_name": "big", "dir": false, "size": 7}}}})
		case downloadPath:
			dl(w, r)
		case taskPath:
			if r.Method != http.MethodGet {
				t.Errorf("task method %s", r.Method)
			}
			q := r.URL.Query()
			if q.Get("task_id") != "t1" || q.Get("retry_index") == "" || q.Get("pr") != "ucpro" {
				t.Errorf("bad task query %s", r.URL.RawQuery)
			}
			writeJSON(w, 200, taskFn(polls.Add(1), r))
		case "/blob/f":
			w.Write([]byte("bigdata"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &polls
}

func fastClient(srv *httptest.Server) *Client {
	c := newTestClient(srv)
	c.pollInterval = time.Millisecond
	c.maxPolls = 5
	return c
}

func taskStarted(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"status": 200, "code": 0, "data": map[string]any{"task_id": "t1", "task_sync": false}})
}

func TestTaskContinuesAfterEmptyData(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			writeJSON(w, 200, map[string]any{"status": 200, "code": 0, "data": nil})
			return
		}
		writeJSON(w, 200, map[string]any{"status": 200, "code": 0, "data": map[string]any{"status": 2, "download_url": "https://download.quark.cn/file"}})
	}))
	defer srv.Close()
	c := newTestClient(srv)
	c.pollInterval = time.Millisecond
	link, err := c.waitTask(context.Background(), "task")
	if err != nil || link != "https://download.quark.cn/file" || calls != 2 {
		t.Fatalf("link=%q calls=%d err=%v", link, calls, err)
	}
}

func TestAsyncDownloadSuccess(t *testing.T) {
	var base string
	srv, polls := asyncServer(t, taskStarted, func(n int32, r *http.Request) any {
		if n < 3 {
			return map[string]any{"status": 200, "code": 0, "data": map[string]any{"status": 1}}
		}
		return map[string]any{"status": 200, "code": 0, "data": map[string]any{"status": 2,
			"data": []any{map[string]any{"download_url": base + "/blob/f?sig=x"}}}}
	})
	base = srv.URL
	var buf bytes.Buffer
	if err := fastClient(srv).Download(context.Background(), "/big", &buf); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "bigdata" || polls.Load() != 3 {
		t.Fatalf("got %q after %d polls", buf.String(), polls.Load())
	}
}

func TestAsyncDownloadTaskRespInline(t *testing.T) {
	var base string
	srv, polls := asyncServer(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"status": 200, "code": 0, "data": map[string]any{"task_id": "t1",
			"task_resp": map[string]any{"data": []any{map[string]any{"download_url": base + "/blob/f"}}}}})
	}, func(int32, *http.Request) any { return nil })
	base = srv.URL
	var buf bytes.Buffer
	if err := fastClient(srv).Download(context.Background(), "/big", &buf); err != nil || buf.String() != "bigdata" {
		t.Fatalf("got %q err=%v", buf.String(), err)
	}
	if polls.Load() != 0 {
		t.Fatal("should not poll when task_resp has link")
	}
}

func TestAsyncDownloadFailed(t *testing.T) {
	srv, _ := asyncServer(t, taskStarted, func(int32, *http.Request) any {
		return map[string]any{"status": 200, "code": 0, "data": map[string]any{"status": 3}}
	})
	err := fastClient(srv).Download(context.Background(), "/big", &bytes.Buffer{})
	if !errors.Is(err, ErrTaskFailed) {
		t.Fatalf("want ErrTaskFailed, got %v", err)
	}
}

func TestAsyncDownloadAPIError(t *testing.T) {
	srv, _ := asyncServer(t, taskStarted, func(int32, *http.Request) any {
		return map[string]any{"status": 200, "code": 32003, "message": "task gone"}
	})
	err := fastClient(srv).Download(context.Background(), "/big", &bytes.Buffer{})
	var ae *APIError
	if !errors.As(err, &ae) || ae.Op != "task" || ae.Code != 32003 {
		t.Fatalf("want task APIError, got %v", err)
	}
	if strings.Contains(err.Error(), "secret") {
		t.Fatal("cookie leaked in error")
	}
}

func TestAsyncDownloadTimeout(t *testing.T) {
	srv, polls := asyncServer(t, taskStarted, func(int32, *http.Request) any {
		return map[string]any{"status": 200, "code": 0, "data": map[string]any{"status": 1}}
	})
	err := fastClient(srv).Download(context.Background(), "/big", &bytes.Buffer{})
	if !errors.Is(err, ErrTaskTimeout) {
		t.Fatalf("want ErrTaskTimeout, got %v", err)
	}
	if polls.Load() != 5 {
		t.Fatalf("polls=%d want 5", polls.Load())
	}
}

func TestAsyncDownloadDoneWithoutURL(t *testing.T) {
	srv, _ := asyncServer(t, taskStarted, func(int32, *http.Request) any {
		return map[string]any{"status": 200, "code": 0, "data": map[string]any{"status": 2}}
	})
	if err := fastClient(srv).Download(context.Background(), "/big", &bytes.Buffer{}); err == nil {
		t.Fatal("expected error")
	}
}

func TestAsyncDownloadUntrustedURL(t *testing.T) {
	srv, _ := asyncServer(t, taskStarted, func(int32, *http.Request) any {
		return map[string]any{"status": 200, "code": 0, "data": map[string]any{"status": 2, "download_url": "https://evil.example/x"}}
	})
	err := fastClient(srv).Download(context.Background(), "/big", &bytes.Buffer{})
	if !errors.Is(err, ErrUntrustedURL) {
		t.Fatalf("want ErrUntrustedURL, got %v", err)
	}
}

func TestAsyncDownloadCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv, polls := asyncServer(t, taskStarted, func(n int32, _ *http.Request) any {
		if n == 2 {
			cancel()
		}
		return map[string]any{"status": 200, "code": 0, "data": map[string]any{"status": 1}}
	})
	c := fastClient(srv)
	c.maxPolls = 1000
	err := c.Download(ctx, "/big", &bytes.Buffer{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if polls.Load() != 2 {
		t.Fatalf("polls=%d, cancel not honored promptly", polls.Load())
	}
}

func TestAsyncDownloadCanceledDuringWait(t *testing.T) {
	srv, polls := asyncServer(t, taskStarted, func(int32, *http.Request) any { return nil })
	c := newTestClient(srv)
	c.pollInterval = time.Hour
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := c.Download(ctx, "/big", &bytes.Buffer{})
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 5*time.Second {
		t.Fatalf("want prompt DeadlineExceeded, got %v", err)
	}
	if polls.Load() != 0 {
		t.Fatal("unexpected poll")
	}
}

func TestDownloadSizeLimitDesktopRetry(t *testing.T) {
	var base string
	var calls atomic.Int32
	srv, _ := asyncServer(t, func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		q := r.URL.Query()
		var body struct{ Fids []string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Fids) != 1 || body.Fids[0] != "f" {
			t.Errorf("call %d: bad body %v", n, err)
		}
		if n == 1 {
			if q.Has("sys") || strings.Contains(r.UserAgent(), "quark-cloud-drive") {
				t.Error("first attempt should use web identity")
			}
			writeJSON(w, 400, map[string]any{"status": 400, "code": 23018, "message": "download file size limit"})
			return
		}
		if q.Get("sys") != "win32" || q.Get("ve") != "2.5.56" || !q.Has("ut") || !q.Has("guid") || q.Get("pr") != "ucpro" {
			t.Errorf("desktop query missing: %s", r.URL.RawQuery)
		}
		if !strings.Contains(r.UserAgent(), "quark-cloud-drive/2.5.56") {
			t.Errorf("desktop UA missing: %s", r.UserAgent())
		}
		writeJSON(w, 200, map[string]any{"status": 200, "code": 0, "data": []any{map[string]any{"download_url": base + "/blob/f"}}})
	}, func(int32, *http.Request) any { return nil })
	base = srv.URL
	var buf bytes.Buffer
	if err := fastClient(srv).Download(context.Background(), "/big", &buf); err != nil || buf.String() != "bigdata" {
		t.Fatalf("got %q err=%v", buf.String(), err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls=%d want 2", calls.Load())
	}
}

func TestDownloadSizeLimitDesktopThenAsync(t *testing.T) {
	var base string
	var calls atomic.Int32
	srv, _ := asyncServer(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			writeJSON(w, 200, map[string]any{"status": 200, "code": 23018, "message": "x"})
			return
		}
		taskStarted(w, r)
	}, func(int32, *http.Request) any {
		return map[string]any{"status": 200, "code": 0, "data": map[string]any{"status": 2, "download_url": base + "/blob/f"}}
	})
	base = srv.URL
	var buf bytes.Buffer
	if err := fastClient(srv).Download(context.Background(), "/big", &buf); err != nil || buf.String() != "bigdata" {
		t.Fatalf("got %q err=%v", buf.String(), err)
	}
}

func TestDownloadSizeLimitRetriesOnlyOnce(t *testing.T) {
	var calls atomic.Int32
	srv, _ := asyncServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeJSON(w, 400, map[string]any{"status": 400, "code": 23018, "message": "download file size limit"})
	}, func(int32, *http.Request) any { return nil })
	err := fastClient(srv).Download(context.Background(), "/big", &bytes.Buffer{})
	var ae *APIError
	if !errors.As(err, &ae) || ae.Code != 23018 || calls.Load() != 2 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestDownloadOtherErrorNoDesktopRetry(t *testing.T) {
	var calls atomic.Int32
	srv, _ := asyncServer(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeJSON(w, 200, map[string]any{"status": 200, "code": 41010, "message": "no permission"})
	}, func(int32, *http.Request) any { return nil })
	if err := fastClient(srv).Download(context.Background(), "/big", &bytes.Buffer{}); err == nil || calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestIsQuarkURL(t *testing.T) {
	cases := map[string]bool{
		"https://drive-pc.quark.cn/x":       true,
		"https://quark.cn/":                 true,
		"http://drive-pc.quark.cn/x":        false,
		"https://evilquark.cn/":             false,
		"https://quark.cn.evil.com/":        false,
		"https://user@drive.quark.cn/":      false,
		"https://dl-pc.drive.quark.cn:443":  true,
		"https://dl-pc.drive.quark.cn:8443": false,
	}
	for s, want := range cases {
		if got := isQuarkURL(mustURL(s)); got != want {
			t.Errorf("%s: got %v want %v", s, got, want)
		}
	}
}

func TestNumericFidAndFileFlag(t *testing.T) {
	var r rawFile
	if err := json.Unmarshal([]byte(`{"fid":12345,"file_name":"a","file":true,"size":7}`), &r); err != nil {
		t.Fatal(err)
	}
	f, err := r.toFile()
	if err != nil || f != (File{ID: "12345", Name: "a", Size: 7}) {
		t.Fatalf("%+v %v", f, err)
	}
}
