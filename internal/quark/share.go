package quark

// Share links: create, list own shares, save (转存) others' shares, cancel.
//
// Protocol (as observed in the reference client; JSON envelope as List):
//
//	create    POST drive-pc /1/clouddrive/share            {fid_list:[fid], title, url_type, expired_type[, passcode]}
//	           -> {task_id, task_sync, task_resp:{data:{share_id}}}
//	task      GET  drive-pc /1/clouddrive/task?task_id=&retry_index=0
//	           -> {status (2 done, 3 failed, 1 running), share_id}
//	link      POST drive-pc /1/clouddrive/share/password   {share_id}
//	           -> {share_url, pwd_id, passcode?, expired_at?}
//	mine      GET  drive-pc /1/clouddrive/share/mypage/detail?_page&_size&_order_field&_order_type&_fetch_total=1&_fetch_notify_follow=1
//	           -> {list:[{share_id, first_file:{fid,...}, ...}]}
//	token     POST drive-h  /1/clouddrive/share/sharepage/token {pwd_id, passcode, support_visit_limit_private_share:true}
//	           -> {stoken}
//	save      POST drive-pc /1/clouddrive/share/sharepage/save  {fid_list, share_token_list, to_pdir_fid, pwd_id, stoken, pdir_fid:"0", pdir_save_all, exclude_fids:[], scene:"link"}
//	delete    POST drive-pc /1/clouddrive/share/delete     {share_ids}
//
// Safety notes:
//   - Mutating requests (create, save, delete) are never retried. Transport
//     failures after sending are reported as ErrUnconfirmed.
//   - The reference client does not poll the save task and its completion
//     semantics are unknown, so SaveShare only reports that the server
//     accepted the request (SaveResult.Accepted); it never claims the files
//     were saved.
//   - The reference client falls back to guessing the share_id from the
//     share list when task polling fails; this client does not, and returns
//     ErrUnconfirmed instead (the share may or may not exist).
//   - Passcodes and stokens are never put into error messages; ShareLink and
//     ShareRef redact the passcode in their String/GoString forms.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	sharePath         = "/1/clouddrive/share"
	sharePasswordPath = "/1/clouddrive/share/password"
	shareDeletePath   = "/1/clouddrive/share/delete"
	shareMinePath     = "/1/clouddrive/share/mypage/detail"
	shareTokenPath    = "/1/clouddrive/share/sharepage/token"
	shareSavePath     = "/1/clouddrive/share/sharepage/save"

	// shareHBaseURL hosts the sharepage token endpoint in the reference.
	shareHBaseURL = "https://drive-h.quark.cn"

	shareTaskRunning = 1

	sharePasscodeLen  = 4
	maxPasscodeLen    = 32
	maxSharePageSize  = 100
	maxSharePage      = 1000
	maxShareTitleLen  = maxNameBytes
	defaultShareOrder = "created_at"
)

var (
	// ErrInvalidShare reports an unusable share link, id or argument.
	ErrInvalidShare = errors.New("quark: invalid share argument")
	// ErrShareTaskFailed reports that the share-creation task failed.
	ErrShareTaskFailed = errors.New("quark: share task failed")
	// ErrPasscodeMismatch reports that the server returned a different
	// passcode than the one submitted. Values are deliberately not included.
	ErrPasscodeMismatch = errors.New("quark: share passcode mismatch")

	fidRe       = regexp.MustCompile(`^[0-9A-Za-z_-]{1,128}$`)
	shareLinkRe = regexp.MustCompile(`/s/(\w+)`)
	passcodeRe  = regexp.MustCompile(`提取码[:：]\s*([0-9A-Za-z]{1,32})`)
	passcodeABC = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
)

// ShareExpiry is the validity period of a new share.
type ShareExpiry int

// Valid share periods, in days (0 = permanent), as in the reference.
const (
	ExpireNever  ShareExpiry = 0
	Expire1Day   ShareExpiry = 1
	Expire7Days  ShareExpiry = 7
	Expire30Days ShareExpiry = 30
)

func (e ShareExpiry) expiredType() (int, error) {
	switch e {
	case ExpireNever:
		return 1, nil
	case Expire1Day:
		return 2, nil
	case Expire7Days:
		return 3, nil
	case Expire30Days:
		return 4, nil
	}
	return 0, fmt.Errorf("%w: expiry must be 0, 1, 7 or 30 days", ErrInvalidShare)
}

// ShareOptions control CreateShare / CreateShareByFID.
type ShareOptions struct {
	Expiry ShareExpiry
	// Passcode requests a passcode-protected share. A random 4-character
	// passcode is generated with crypto/rand.
	Passcode bool
}

// ShareLink is a created share. Passcode is a secret: do not log it.
type ShareLink struct {
	ShareID   string
	ShareURL  string
	PwdID     string
	Passcode  string // empty if the share has no passcode
	ExpiresAt int64  // milliseconds since epoch as returned; 0 if unknown
}

// String redacts the passcode so that %v / %s are safe to log.
func (l ShareLink) String() string {
	return fmt.Sprintf("ShareLink{ShareID:%s URL:%s PwdID:%s Passcode:%s ExpiresAt:%d}",
		l.ShareID, redact(l.ShareURL), l.PwdID, redact(l.Passcode), l.ExpiresAt)
}

// GoString redacts the passcode for %#v.
func (l ShareLink) GoString() string { return l.String() }

// ShareRef identifies someone's share. Passcode is a secret: do not log it.
type ShareRef struct {
	PwdID    string
	Passcode string
}

// String redacts the passcode.
func (r ShareRef) String() string {
	return fmt.Sprintf("ShareRef{PwdID:%s Passcode:%s}", r.PwdID, redact(r.Passcode))
}

// GoString redacts the passcode for %#v.
func (r ShareRef) GoString() string { return r.String() }

func redact(s string) string {
	if s == "" {
		return `""`
	}
	return "[redacted]"
}

// ParseShareLink extracts the share id from a link such as
// https://pan.quark.cn/s/abc123 and an optional passcode written as
// "提取码:xxxx" in the same text, following the reference client.
func ParseShareLink(text string) (ShareRef, error) {
	m := shareLinkRe.FindStringSubmatch(text)
	if len(m) < 2 || !fidRe.MatchString(m[1]) {
		return ShareRef{}, fmt.Errorf("%w: unrecognised share link", ErrInvalidShare)
	}
	ref := ShareRef{PwdID: m[1]}
	if pc := passcodeRe.FindStringSubmatch(text); len(pc) >= 2 {
		ref.Passcode = pc[1]
	}
	if err := validPasscode(ref.Passcode); err != nil {
		return ShareRef{}, err
	}
	return ref, nil
}

func validPasscode(p string) error {
	if len(p) > maxPasscodeLen {
		return fmt.Errorf("%w: passcode too long", ErrInvalidShare)
	}
	for _, r := range p {
		if r <= 0x20 || r == 0x7f {
			return fmt.Errorf("%w: passcode contains invalid characters", ErrInvalidShare)
		}
	}
	return nil
}

func validID(kind, id string) error {
	if !fidRe.MatchString(id) {
		return fmt.Errorf("%w: invalid %s", ErrInvalidShare, kind)
	}
	return nil
}

// CreateShare shares the file or directory at remotePath. The root
// directory cannot be shared.
func (c *Client) CreateShare(ctx context.Context, remotePath string, opt ShareOptions) (ShareLink, error) {
	parentPath, name, err := splitParent(remotePath)
	if err != nil {
		return ShareLink{}, err
	}
	_, f, err := c.resolveSource(ctx, parentPath, name, AnyKind)
	if err != nil {
		return ShareLink{}, err
	}
	return c.createShare(ctx, f.ID, f.Name, opt)
}

// CreateShareByFID shares the entry with the given fid. Its existence is
// not checked locally; title is the share title (normally the file name).
func (c *Client) CreateShareByFID(ctx context.Context, fid, title string, opt ShareOptions) (ShareLink, error) {
	if err := validID("fid", fid); err != nil {
		return ShareLink{}, err
	}
	if fid == rootID {
		return ShareLink{}, ErrRoot
	}
	if err := validName(title); err != nil || len(title) > maxShareTitleLen {
		return ShareLink{}, fmt.Errorf("%w: invalid title", ErrInvalidShare)
	}
	return c.createShare(ctx, fid, title, opt)
}

func (c *Client) createShare(ctx context.Context, fid, title string, opt ShareOptions) (ShareLink, error) {
	et, err := opt.Expiry.expiredType()
	if err != nil {
		return ShareLink{}, err
	}
	req := map[string]any{
		"fid_list":     []string{fid},
		"title":        title,
		"url_type":     1, // 1 = no passcode, 2 = passcode
		"expired_type": et,
	}
	var passcode string
	if opt.Passcode {
		if passcode, err = randomPasscode(sharePasscodeLen); err != nil {
			return ShareLink{}, err
		}
		req["url_type"] = 2
		req["passcode"] = passcode
	}
	body, err := json.Marshal(req)
	if err != nil {
		return ShareLink{}, fmt.Errorf("quark: share: %w", err)
	}
	raw, err := c.mutate(ctx, "share", sharePath, body)
	if err != nil {
		return ShareLink{}, safeShareError(err)
	}
	if raw == nil {
		return ShareLink{}, fmt.Errorf("%w: share: empty response", ErrUnconfirmed)
	}
	var resp struct {
		TaskID   string `json:"task_id"`
		TaskSync bool   `json:"task_sync"`
		TaskResp struct {
			Data struct {
				ShareID string `json:"share_id"`
			} `json:"data"`
		} `json:"task_resp"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return ShareLink{}, fmt.Errorf("%w: share: invalid response", ErrUnconfirmed)
	}
	shareID := resp.TaskResp.Data.ShareID
	if shareID == "" {
		if resp.TaskID == "" {
			return ShareLink{}, fmt.Errorf("%w: share: no share_id or task_id", ErrUnconfirmed)
		}
		if shareID, err = c.waitShareTask(ctx, resp.TaskID); err != nil {
			return ShareLink{}, err
		}
	}
	if err := validID("share_id", shareID); err != nil {
		return ShareLink{}, fmt.Errorf("%w: share: server returned invalid share_id", ErrUnconfirmed)
	}
	link, err := c.ShareLinkByID(ctx, shareID)
	if err != nil {
		// The share exists; only retrieving its link failed.
		return ShareLink{ShareID: shareID}, fmt.Errorf("%w: share %s created but fetching its link failed: %v", ErrUnconfirmed, shareID, err)
	}
	if opt.Passcode && link.Passcode != passcode {
		return ShareLink{ShareID: shareID}, fmt.Errorf("%w: share %s created: %w", ErrUnconfirmed, shareID, ErrPasscodeMismatch)
	}
	if !opt.Passcode && link.Passcode != "" {
		return ShareLink{ShareID: shareID}, fmt.Errorf("%w: share %s created with an unexpected passcode: %w", ErrUnconfirmed, shareID, ErrPasscodeMismatch)
	}
	return link, nil
}

// waitShareTask polls the share-creation task. Only status 2 with a
// share_id is success; failure is status 3; anything else until the poll
// limit is ErrUnconfirmed, since the share may still be created.
func (c *Client) waitShareTask(ctx context.Context, taskID string) (string, error) {
	if len(taskID) > maxTaskIDLen || strings.ContainsAny(taskID, "\r\n") {
		return "", fmt.Errorf("%w: share: invalid task id", ErrUnconfirmed)
	}
	interval, limit := c.pollParams()
	var lastErr error
	for i := 0; i < limit; i++ {
		if err := sleepCtx(ctx, interval); err != nil {
			return "", fmt.Errorf("%w: share task %s: %v", ErrUnconfirmed, taskID, err)
		}
		q := url.Values{}
		q.Set("task_id", taskID)
		q.Set("retry_index", "0")
		var data struct {
			Status  *int   `json:"status"`
			ShareID string `json:"share_id"`
		}
		// Task polling is a read; a failed poll is tolerated and repeated.
		err := c.call(ctx, "share task", http.MethodGet, taskPath, q, nil, &data)
		if err != nil {
			lastErr = safeShareError(err)
			continue
		}
		if data.Status == nil {
			continue
		}
		switch *data.Status {
		case taskStatusDone:
			if data.ShareID != "" {
				return data.ShareID, nil
			}
			return "", fmt.Errorf("%w: share task %s finished without share_id", ErrUnconfirmed, taskID)
		case taskStatusFailed:
			return "", fmt.Errorf("%w: task %s", ErrShareTaskFailed, taskID)
		case shareTaskRunning:
		default:
			// Unknown status: keep waiting rather than guessing.
		}
	}
	if lastErr != nil {
		return "", fmt.Errorf("%w: share task %s: %v", ErrUnconfirmed, taskID, lastErr)
	}
	return "", fmt.Errorf("%w: share task %s: timed out", ErrUnconfirmed, taskID)
}

// ShareLinkByID returns the URL, pwd_id and passcode of one of your shares.
// The returned Passcode is a secret.
func (c *Client) ShareLinkByID(ctx context.Context, shareID string) (ShareLink, error) {
	if err := validID("share_id", shareID); err != nil {
		return ShareLink{}, err
	}
	body, _ := json.Marshal(map[string]string{"share_id": shareID})
	var data struct {
		ShareURL  string          `json:"share_url"`
		PwdID     string          `json:"pwd_id"`
		Passcode  json.RawMessage `json:"passcode"`
		ExpiredAt json.Number     `json:"expired_at"`
	}
	// Read-only per the reference (it returns existing link data).
	if err := c.call(ctx, "share link", http.MethodPost, sharePasswordPath, nil, body, &data); err != nil {
		return ShareLink{}, safeShareError(err)
	}
	if data.ShareURL == "" {
		return ShareLink{}, errors.New("quark: share link: empty share_url")
	}
	if u, err := url.Parse(data.ShareURL); err != nil || !isQuarkURL(u) {
		return ShareLink{}, errors.New("quark: share link: unexpected share_url host")
	}
	link := ShareLink{ShareID: shareID, ShareURL: data.ShareURL, PwdID: data.PwdID}
	var pc string
	if len(data.Passcode) > 0 && json.Unmarshal(data.Passcode, &pc) == nil {
		link.Passcode = pc
	}
	if data.ExpiredAt != "" {
		if n, err := strconv.ParseInt(data.ExpiredAt.String(), 10, 64); err == nil {
			link.ExpiresAt = n
		} else if f, err := data.ExpiredAt.Float64(); err == nil {
			link.ExpiresAt = int64(f)
		}
	}
	return link, nil
}

func randomPasscode(n int) (string, error) {
	b := make([]byte, n)
	max := big.NewInt(int64(len(passcodeABC)))
	for i := range b {
		k, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", fmt.Errorf("quark: share: generate passcode: %w", err)
		}
		b[i] = passcodeABC[k.Int64()]
	}
	return string(b), nil
}

// MyShare contains the non-secret fields needed to display a share.
type MyShare struct {
	ShareID       string
	FirstFileID   string
	FirstFileName string
}

func (s MyShare) String() string {
	return fmt.Sprintf("MyShare{ShareID:%s FirstFileID:%s FirstFileName:%q}", s.ShareID, s.FirstFileID, s.FirstFileName)
}

func (s MyShare) GoString() string { return s.String() }

// ShareListOptions control ListMyShares. Zero values use the reference
// defaults: page 1, size 50, created_at desc.
type ShareListOptions struct {
	Page       int    // 1..1000
	Size       int    // 1..100
	OrderField string // created_at, updated_at, file_name, size
	OrderType  string // asc, desc
}

func (o *ShareListOptions) normalize() error {
	if o.Page == 0 {
		o.Page = 1
	}
	if o.Size == 0 {
		o.Size = pageSize
	}
	if o.OrderField == "" {
		o.OrderField = defaultShareOrder
	}
	if o.OrderType == "" {
		o.OrderType = "desc"
	}
	if o.Page < 1 || o.Page > maxSharePage {
		return fmt.Errorf("%w: page must be in [1, %d]", ErrInvalidShare, maxSharePage)
	}
	if o.Size < 1 || o.Size > maxSharePageSize {
		return fmt.Errorf("%w: size must be in [1, %d]", ErrInvalidShare, maxSharePageSize)
	}
	switch o.OrderField {
	case "created_at", "updated_at", "file_name", "size":
	default:
		return fmt.Errorf("%w: invalid order field", ErrInvalidShare)
	}
	if o.OrderType != "asc" && o.OrderType != "desc" {
		return fmt.Errorf("%w: invalid order type", ErrInvalidShare)
	}
	return nil
}

// ListMyShares returns one page of your shares. A page shorter than the
// requested size marks the end (the server total is not relied upon).
func (c *Client) ListMyShares(ctx context.Context, opt ShareListOptions) ([]MyShare, error) {
	if err := opt.normalize(); err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Set("_page", strconv.Itoa(opt.Page))
	q.Set("_size", strconv.Itoa(opt.Size))
	q.Set("_order_field", opt.OrderField)
	q.Set("_order_type", opt.OrderType)
	q.Set("_fetch_total", "1")
	q.Set("_fetch_notify_follow", "1")
	var data struct {
		List []json.RawMessage `json:"list"`
	}
	if err := c.call(ctx, "share list", http.MethodGet, shareMinePath, q, nil, &data); err != nil {
		return nil, safeShareError(err)
	}
	out := make([]MyShare, 0, len(data.List))
	for _, raw := range data.List {
		var it struct {
			ShareID   string `json:"share_id"`
			FirstFile struct {
				Fid      string `json:"fid"`
				FileName string `json:"file_name"`
			} `json:"first_file"`
		}
		if err := json.Unmarshal(raw, &it); err != nil || it.ShareID == "" {
			return nil, errors.New("quark: share list: invalid entry")
		}
		out = append(out, MyShare{
			ShareID:       it.ShareID,
			FirstFileID:   it.FirstFile.Fid,
			FirstFileName: it.FirstFile.FileName,
		})
	}
	return out, nil
}

// ListAllMyShares pages through all of your shares (bounded by the
// reference page limit).
func (c *Client) ListAllMyShares(ctx context.Context) ([]MyShare, error) {
	var all []MyShare
	for page := 1; page <= maxSharePage; page++ {
		items, err := c.ListMyShares(ctx, ShareListOptions{Page: page, Size: pageSize})
		if err != nil {
			return nil, err
		}
		all = append(all, items...)
		if len(items) < pageSize {
			return all, nil
		}
	}
	return nil, errors.New("quark: share list: too many pages")
}

// SaveOptions select what to save from a share. With no FIDs the whole
// share is saved (the only mode exercised by the reference CLI). Selective
// saving requires the share_fid_token of each fid, in the same order.
type SaveOptions struct {
	FIDs           []string
	ShareFIDTokens []string // secrets; never logged
}

// SaveResult reports that the server accepted a save request. The save
// runs asynchronously; completion is NOT confirmed by this client.
type SaveResult struct {
	Accepted bool
	TaskID   string // empty if the server did not return one
}

// SaveShare saves (转存) the share ref into the directory at dstDir.
func (c *Client) SaveShare(ctx context.Context, ref ShareRef, dstDir string, opt SaveOptions) (SaveResult, error) {
	d, err := c.resolveDir(ctx, dstDir)
	if err != nil {
		return SaveResult{}, err
	}
	return c.SaveShareToFID(ctx, ref, d.ID, opt)
}

// SaveShareToFID saves the share ref into the directory fid ("0" = root).
// The fid is not checked to be an existing directory.
func (c *Client) SaveShareToFID(ctx context.Context, ref ShareRef, dstFID string, opt SaveOptions) (SaveResult, error) {
	if err := validID("pwd_id", ref.PwdID); err != nil {
		return SaveResult{}, err
	}
	if err := validPasscode(ref.Passcode); err != nil {
		return SaveResult{}, err
	}
	if err := validID("target fid", dstFID); err != nil {
		return SaveResult{}, err
	}
	if len(opt.FIDs) != len(opt.ShareFIDTokens) {
		return SaveResult{}, fmt.Errorf("%w: fids and share tokens differ in length", ErrInvalidShare)
	}
	for _, f := range opt.FIDs {
		if err := validID("fid", f); err != nil {
			return SaveResult{}, err
		}
	}
	for _, t := range opt.ShareFIDTokens {
		if t == "" || len(t) > 1024 || strings.ContainsAny(t, "\r\n") {
			return SaveResult{}, fmt.Errorf("%w: invalid share token", ErrInvalidShare)
		}
	}
	stoken, err := c.shareStoken(ctx, ref)
	if err != nil {
		return SaveResult{}, err
	}
	fids, tokens := opt.FIDs, opt.ShareFIDTokens
	if fids == nil {
		fids, tokens = []string{}, []string{}
	}
	body, err := json.Marshal(map[string]any{
		"fid_list":         fids,
		"share_token_list": tokens,
		"to_pdir_fid":      dstFID,
		"pwd_id":           ref.PwdID,
		"stoken":           stoken,
		"pdir_fid":         rootID,
		"pdir_save_all":    len(fids) == 0,
		"exclude_fids":     []string{},
		"scene":            "link",
	})
	if err != nil {
		return SaveResult{}, fmt.Errorf("quark: share save: %w", err)
	}
	q, err := shareRequestQuery()
	if err != nil {
		return SaveResult{}, err
	}
	raw, err := c.mutateWithQuery(ctx, "share save", shareSavePath, q, body)
	if err != nil {
		return SaveResult{}, safeShareError(err)
	}
	res := SaveResult{Accepted: true}
	if raw != nil {
		var d struct {
			TaskID string `json:"task_id"`
		}
		if json.Unmarshal(raw, &d) == nil {
			res.TaskID = d.TaskID
		}
	}
	return res, nil
}

// safeShareError avoids echoing server bodies that could contain share tokens
// or extraction codes while preserving ambiguous-operation semantics.
func safeShareError(err error) error {
	if errors.Is(err, ErrUnconfirmed) {
		return fmt.Errorf("%w: share request; check remote state before retrying", ErrUnconfirmed)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var api *APIError
	if errors.As(err, &api) {
		return fmt.Errorf("quark: share API rejected request (status %d, code %d)", api.Status, api.Code)
	}
	var httpErr *HTTPError
	if errors.As(err, &httpErr) {
		return fmt.Errorf("quark: share HTTP request failed (status %d)", httpErr.StatusCode)
	}
	return errors.New("quark: share request failed")
}

// shareRequestQuery includes the request timing parameters used by the
// reference client for share-token and share-save requests.
func shareRequestQuery() (url.Values, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(900))
	if err != nil {
		return nil, fmt.Errorf("quark: share: request nonce: %w", err)
	}
	q := url.Values{}
	q.Set("__dt", strconv.FormatInt(n.Int64()+100, 10))
	q.Set("__t", strconv.FormatInt(time.Now().UnixMilli(), 10))
	return q, nil
}

// shareStoken exchanges pwd_id + passcode for a share access token. The
// token is a secret and is only returned to the caller in memory.
func (c *Client) shareStoken(ctx context.Context, ref ShareRef) (string, error) {
	body, err := json.Marshal(map[string]any{
		"pwd_id":                            ref.PwdID,
		"passcode":                          ref.Passcode,
		"support_visit_limit_private_share": true,
	})
	if err != nil {
		return "", fmt.Errorf("quark: share token: %w", err)
	}
	hc := *c
	if c.baseURL == defaultBaseURL {
		hc.baseURL = shareHBaseURL
	}
	var data struct {
		Stoken string `json:"stoken"`
	}
	q, err := shareRequestQuery()
	if err != nil {
		return "", err
	}
	// Token exchange creates no user-visible state; treated as a read.
	if err := hc.call(ctx, "share token", http.MethodPost, shareTokenPath, q, body, &data); err != nil {
		return "", safeShareError(err)
	}
	if data.Stoken == "" {
		return "", errors.New("quark: share token: empty stoken")
	}
	return data.Stoken, nil
}

// DeleteShares cancels your shares by share_id. This is destructive: the
// links stop working. Afterwards the share list is re-read; if any id is
// still present (or the list cannot be read) ErrUnconfirmed is returned.
func (c *Client) DeleteShares(ctx context.Context, shareIDs []string) error {
	if len(shareIDs) == 0 {
		return fmt.Errorf("%w: no share ids", ErrInvalidShare)
	}
	want := make(map[string]bool, len(shareIDs))
	for _, id := range shareIDs {
		if err := validID("share_id", id); err != nil {
			return err
		}
		want[id] = true
	}
	body, err := json.Marshal(map[string]any{"share_ids": shareIDs})
	if err != nil {
		return fmt.Errorf("quark: share delete: %w", err)
	}
	if _, err := c.mutate(ctx, "share delete", shareDeletePath, body); err != nil {
		return safeShareError(err)
	}
	return c.verify(ctx, "share delete", func() (bool, error) {
		all, err := c.ListAllMyShares(ctx)
		if err != nil {
			return false, err
		}
		for _, s := range all {
			if want[s.ShareID] {
				return false, nil
			}
		}
		return true, nil
	})
}
