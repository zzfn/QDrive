// Package quark implements a minimal client for Quark Drive.
package quark

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

const (
	defaultBaseURL = "https://drive-pc.quark.cn"
	panOrigin      = "https://pan.quark.cn"
	userAgent      = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/142.0.0.0 Safari/537.36"

	listPath     = "/1/clouddrive/file/sort"
	downloadPath = "/1/clouddrive/file/download"
	taskPath     = "/1/clouddrive/task"

	// codeSizeLimit is returned to web clients for files above the web
	// download size limit; the desktop client identity is allowed instead.
	codeSizeLimit    = 23018
	desktopUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) quark-cloud-drive/2.5.56 Chrome/100.0.4896.160 Electron/18.3.5.12-a038f7b798 Safari/537.36 Channel/pckk_other_ch"

	defaultPollInterval = 2 * time.Second
	defaultMaxPolls     = 60
	maxTaskIDLen        = 256

	taskStatusDone   = 2
	taskStatusFailed = 3

	rootID       = "0"
	pageSize     = 50
	maxPages     = 10000
	maxJSONBytes = 16 << 20
	maxErrBytes  = 512
)

var (
	// ErrNotFound reports that a path component does not exist.
	ErrNotFound = errors.New("quark: file not found")
	// ErrNotDir reports that a non-directory was used as a directory.
	ErrNotDir = errors.New("quark: not a directory")
	// ErrIsDir reports an attempt to download a directory.
	ErrIsDir = errors.New("quark: is a directory")
	// ErrAmbiguous reports that several entries share the requested name.
	ErrAmbiguous = errors.New("quark: ambiguous path")
	// ErrTaskFailed reports that an asynchronous download task failed.
	ErrTaskFailed = errors.New("quark: download task failed")
	// ErrTaskTimeout reports that an asynchronous download task did not
	// finish within the polling limit.
	ErrTaskTimeout = errors.New("quark: download task timed out")
	// ErrUntrustedURL reports a download URL outside trusted Quark hosts.
	ErrUntrustedURL = errors.New("quark: untrusted download url")
	errMissingData  = errors.New("missing data")
)

// File is an entry in a Quark Drive directory.
type File struct {
	ID    string
	Name  string
	IsDir bool
	Size  int64
}

// HTTPError is a non-2xx HTTP response.
type HTTPError struct {
	Op         string
	StatusCode int
	Body       string // truncated
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("quark: %s: http %d: %s", e.Op, e.StatusCode, e.Body)
}

// APIError is a business error reported in the JSON envelope.
type APIError struct {
	Op      string
	Status  int
	Code    int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("quark: %s: api status=%d code=%d: %s", e.Op, e.Status, e.Code, e.Message)
}

// Client is a Quark Drive client. The cookie is only ever sent over HTTPS to
// quark.cn hosts.
type Client struct {
	cookie  string
	baseURL string
	hc      *http.Client
	trusted func(*url.URL) bool

	pollInterval time.Duration // delay between task polls
	maxPolls     int           // upper bound on task polls
}

// New returns a client authenticated by the given browser cookie string.
func New(cookie string) *Client {
	c := &Client{
		cookie:  strings.TrimSpace(cookie),
		baseURL: defaultBaseURL,
		trusted: isQuarkURL,

		pollInterval: defaultPollInterval,
		maxPolls:     defaultMaxPolls,
	}
	c.hc = &http.Client{
		Timeout: 0, // rely on ctx; downloads may be long
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("quark: too many redirects")
			}
			if !c.trusted(req.URL) {
				return ErrUntrustedURL
			}
			return nil
		},
	}
	return c
}

func isQuarkURL(u *url.URL) bool {
	if u == nil || u.Scheme != "https" || u.User != nil || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return h == "quark.cn" || strings.HasSuffix(h, ".quark.cn")
}

// List returns the entries of the directory at remotePath ("/" is root).
func (c *Client) List(ctx context.Context, remotePath string) ([]File, error) {
	f, err := c.resolve(ctx, remotePath)
	if err != nil {
		return nil, err
	}
	if !f.IsDir {
		return nil, fmt.Errorf("%w: %s", ErrNotDir, remotePath)
	}
	return c.listDir(ctx, f.ID)
}

// Download writes the content of the regular file at remotePath to dst.
func (c *Client) Download(ctx context.Context, remotePath string, dst io.Writer) error {
	if dst == nil {
		return errors.New("quark: nil writer")
	}
	f, err := c.resolve(ctx, remotePath)
	if err != nil {
		return err
	}
	if f.IsDir {
		return fmt.Errorf("%w: %s", ErrIsDir, remotePath)
	}
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
		return fmt.Errorf("quark: download: %w", err)
	}
	c.setHeaders(req)
	req.Header.Set("Accept", "*/*")
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("quark: download: %w", scrub(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return httpErr("download", resp)
	}
	n, err := io.Copy(dst, resp.Body)
	if err != nil {
		return fmt.Errorf("quark: download: %w", err)
	}
	if resp.ContentLength >= 0 && n != resp.ContentLength {
		return fmt.Errorf("quark: download: short body %d/%d", n, resp.ContentLength)
	}
	if f.Size > 0 && n != f.Size {
		return fmt.Errorf("quark: download: size mismatch %d/%d", n, f.Size)
	}
	return nil
}

func splitPath(p string) ([]string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		p = "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	for _, s := range strings.Split(p, "/") {
		if s == ".." {
			return nil, fmt.Errorf("quark: invalid path %q", p)
		}
	}
	p = path.Clean(p)
	if p == "/" {
		return nil, nil
	}
	return strings.Split(strings.TrimPrefix(p, "/"), "/"), nil
}

func (c *Client) resolve(ctx context.Context, remotePath string) (File, error) {
	parts, err := splitPath(remotePath)
	if err != nil {
		return File{}, err
	}
	cur := File{ID: rootID, Name: "/", IsDir: true}
	for i, name := range parts {
		if !cur.IsDir {
			return File{}, fmt.Errorf("%w: /%s", ErrNotDir, strings.Join(parts[:i], "/"))
		}
		entries, err := c.listDir(ctx, cur.ID)
		if err != nil {
			return File{}, err
		}
		found := -1
		for j := range entries {
			if entries[j].Name == name {
				if found >= 0 {
					return File{}, fmt.Errorf("%w: /%s", ErrAmbiguous, strings.Join(parts[:i+1], "/"))
				}
				found = j
			}
		}
		if found < 0 {
			return File{}, fmt.Errorf("%w: /%s", ErrNotFound, strings.Join(parts[:i+1], "/"))
		}
		cur = entries[found]
	}
	return cur, nil
}

type envelope struct {
	Status  int             `json:"status"`
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

type rawFile struct {
	Fid      json.RawMessage `json:"fid"`
	FileName string          `json:"file_name"`
	Size     json.Number     `json:"size"`
	Dir      *bool           `json:"dir"`
	File     *bool           `json:"file"`
}

func (c *Client) listDir(ctx context.Context, fid string) ([]File, error) {
	var out []File
	for page := 1; page <= maxPages; page++ {
		q := url.Values{}
		q.Set("pdir_fid", fid)
		q.Set("_page", strconv.Itoa(page))
		q.Set("_size", strconv.Itoa(pageSize))
		q.Set("_fetch_total", "1")
		q.Set("_sort", "file_type:asc,updated_at:desc")
		var data struct {
			List []rawFile `json:"list"`
		}
		if err := c.call(ctx, "list", http.MethodGet, listPath, q, nil, &data); err != nil {
			return nil, err
		}
		for _, r := range data.List {
			f, err := r.toFile()
			if err != nil {
				return nil, err
			}
			out = append(out, f)
		}
		// The server total is unreliable; a short page marks the end.
		if len(data.List) < pageSize {
			return out, nil
		}
	}
	return nil, errors.New("quark: list: too many pages")
}

func (r rawFile) toFile() (File, error) {
	f := File{Name: r.FileName}
	var s string
	if err := json.Unmarshal(r.Fid, &s); err == nil {
		f.ID = s
	} else {
		var n json.Number
		if err := json.Unmarshal(r.Fid, &n); err != nil {
			return File{}, errors.New("quark: list: invalid fid")
		}
		f.ID = n.String()
	}
	if f.ID == "" || f.Name == "" {
		return File{}, errors.New("quark: list: entry missing fid or name")
	}
	if r.Size != "" {
		n, err := strconv.ParseInt(r.Size.String(), 10, 64)
		if err != nil {
			fl, ferr := r.Size.Float64()
			if ferr != nil {
				return File{}, errors.New("quark: list: invalid size")
			}
			n = int64(fl)
		}
		f.Size = n
	}
	switch {
	case r.Dir != nil:
		f.IsDir = *r.Dir
	case r.File != nil:
		f.IsDir = !*r.File
	}
	return f, nil
}

type linkItem struct {
	DownloadURL string `json:"download_url"`
}

func firstLink(items []linkItem) string {
	if len(items) > 0 {
		return items[0].DownloadURL
	}
	return ""
}

func (c *Client) downloadURL(ctx context.Context, fid string) (string, error) {
	body, _ := json.Marshal(map[string][]string{"fids": {fid}})
	var raw json.RawMessage
	err := c.callUA(ctx, "download", http.MethodPost, downloadPath, nil, body, &raw, userAgent)
	var ae *APIError
	if errors.As(err, &ae) && isSizeLimit(ae) {
		// Large files are refused to the web client; retry exactly once
		// with the desktop client identity against the same endpoint.
		q := url.Values{}
		q.Set("sys", "win32")
		q.Set("ve", "2.5.56")
		q.Set("ut", "")
		q.Set("guid", "")
		raw = nil
		err = c.callUA(ctx, "download", http.MethodPost, downloadPath, q, body, &raw, desktopUserAgent)
	}
	if err != nil {
		return "", err
	}

	var arr []linkItem
	if json.Unmarshal(raw, &arr) == nil {
		if u := firstLink(arr); u != "" {
			return u, nil
		}
		return "", errors.New("quark: download: empty download_url")
	}
	var obj struct {
		TaskID   string `json:"task_id"`
		TaskResp struct {
			Data []linkItem `json:"data"`
		} `json:"task_resp"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		if u := firstLink(obj.TaskResp.Data); u != "" {
			return u, nil
		}
		if obj.TaskID != "" {
			return c.waitTask(ctx, obj.TaskID)
		}
	}
	return "", errors.New("quark: download: invalid response data")
}

func isSizeLimit(e *APIError) bool {
	return e.Code == codeSizeLimit || strings.Contains(strings.ToLower(e.Message), "download file size limit")
}

// waitTask polls an asynchronous download task until it yields a link,
// fails, the poll limit is reached, or ctx is done.
func (c *Client) waitTask(ctx context.Context, taskID string) (string, error) {
	if len(taskID) > maxTaskIDLen {
		return "", errors.New("quark: task: invalid task id")
	}
	interval, limit := c.pollInterval, c.maxPolls
	if interval <= 0 {
		interval = defaultPollInterval
	}
	if limit <= 0 {
		limit = defaultMaxPolls
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	for i := 0; i < limit; i++ {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-timer.C:
		}
		q := url.Values{}
		q.Set("task_id", taskID)
		q.Set("retry_index", "0") // as observed in the reference protocol
		var data struct {
			Status   *int       `json:"status"`
			URL      string     `json:"download_url"`
			Data     []linkItem `json:"data"`
			TaskResp struct {
				Data []linkItem `json:"data"`
			} `json:"task_resp"`
		}
		if err := c.callUA(ctx, "task", http.MethodGet, taskPath, q, nil, &data, userAgent); err != nil && !errors.Is(err, errMissingData) {
			return "", err
		}
		if data.Status != nil {
			switch *data.Status {
			case taskStatusFailed:
				return "", ErrTaskFailed
			case taskStatusDone:
				for _, u := range []string{data.URL, firstLink(data.Data), firstLink(data.TaskResp.Data)} {
					if u != "" {
						return u, nil
					}
				}
				return "", errors.New("quark: task: finished without download_url")
			}
		}
		timer.Reset(interval)
	}
	return "", ErrTaskTimeout
}

func (c *Client) setHeaders(req *http.Request) {
	c.setHeadersUA(req, userAgent)
}

func (c *Client) setHeadersUA(req *http.Request, ua string) {
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Referer", panOrigin+"/")
	if c.cookie != "" && c.trusted(req.URL) {
		req.Header.Set("Cookie", c.cookie)
	}
}

func (c *Client) call(ctx context.Context, op, method, p string, q url.Values, body []byte, out any) error {
	return c.callUA(ctx, op, method, p, q, body, out, userAgent)
}

func (c *Client) callUA(ctx context.Context, op, method, p string, q url.Values, body []byte, out any, ua string) error {
	u, err := url.Parse(c.baseURL + p)
	if err != nil {
		return fmt.Errorf("quark: %s: %w", op, err)
	}
	if !c.trusted(u) {
		return fmt.Errorf("quark: %s: refusing untrusted api host", op)
	}
	if q == nil {
		q = url.Values{}
	}
	q.Set("pr", "ucpro")
	q.Set("fr", "pc")
	q.Set("uc_param_str", "")
	u.RawQuery = q.Encode()

	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return fmt.Errorf("quark: %s: %w", op, err)
	}
	c.setHeadersUA(req, ua)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Origin", panOrigin)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("quark: %s: %w", op, scrub(err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONBytes+1))
	if err != nil {
		return fmt.Errorf("quark: %s: read body: %w", op, err)
	}
	if len(data) > maxJSONBytes {
		return fmt.Errorf("quark: %s: response too large", op)
	}
	var env envelope
	jerr := json.Unmarshal(data, &env)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		if jerr == nil && (env.Code != 0 || env.Message != "") {
			return &APIError{Op: op, Status: resp.StatusCode, Code: env.Code, Message: env.Message}
		}
		return &HTTPError{Op: op, StatusCode: resp.StatusCode, Body: truncate(data)}
	}
	if jerr != nil {
		return fmt.Errorf("quark: %s: invalid json: %w", op, jerr)
	}
	if env.Code != 0 || (env.Status != 0 && env.Status != http.StatusOK) {
		return &APIError{Op: op, Status: env.Status, Code: env.Code, Message: env.Message}
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return fmt.Errorf("quark: %s: %w", op, errMissingData)
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("quark: %s: invalid data: %w", op, err)
	}
	return nil
}

func httpErr(op string, resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBytes))
	return &HTTPError{Op: op, StatusCode: resp.StatusCode, Body: truncate(b)}
}

func truncate(b []byte) string {
	if len(b) > maxErrBytes {
		b = b[:maxErrBytes]
	}
	return strings.TrimSpace(string(b))
}

// scrub drops the URL (which may carry signed tokens) from transport errors.
func scrub(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}
