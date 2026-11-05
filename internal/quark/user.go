package quark

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	// accountInfoPath lives on pan.quark.cn (not the drive API host) and uses
	// a different envelope: {"success":bool,"code":...,"msg":...,"data":{...}}.
	accountInfoPath = "/account/info"
	// memberPath lives on the drive API host and uses the standard envelope.
	memberPath = "/1/clouddrive/member"
)

// Account is the displayable profile of the logged-in user. Only these
// fields are decoded; the upstream response also carries live credentials
// and other identifiers which are deliberately never read or returned.
type Account struct {
	Nickname  string
	AvatarURL string
}

// Capacity is the storage quota and membership of the logged-in user.
type Capacity struct {
	Used       int64  // bytes
	Total      int64  // bytes
	MemberType string // e.g. "NORMAL", "SUPER_VIP"; empty if not reported
	VIPExpAt   int64  // super VIP expiry, epoch milliseconds; 0 if none
}

// Free returns the remaining bytes, never negative.
func (c Capacity) Free() int64 {
	if c.Total > c.Used {
		return c.Total - c.Used
	}
	return 0
}

// AccountInfo returns the nickname and avatar of the logged-in user.
func (c *Client) AccountInfo(ctx context.Context) (*Account, error) {
	const op = "account"
	q := url.Values{}
	q.Set("fr", "pc")
	q.Set("platform", "pc")
	var data struct {
		Nickname  string `json:"nickname"`
		AvatarURI string `json:"avatarUri"`
		AvatarURL string `json:"avatar_url"`
		Avatar    string `json:"avatar"`
	}
	if err := c.panCall(ctx, op, accountInfoPath, q, &data); err != nil {
		return nil, err
	}
	a := &Account{Nickname: data.Nickname}
	for _, s := range []string{data.AvatarURI, data.AvatarURL, data.Avatar} {
		if s != "" {
			a.AvatarURL = s
			break
		}
	}
	return a, nil
}

// Capacity returns the storage usage, quota and membership of the
// logged-in user.
func (c *Client) Capacity(ctx context.Context) (*Capacity, error) {
	q := url.Values{}
	q.Set("fetch_subscribe", "true")
	q.Set("fetch_identity", "true")
	var data struct {
		UseCapacity   json.Number `json:"use_capacity"`
		TotalCapacity json.Number `json:"total_capacity"`
		MemberType    string      `json:"member_type"`
		SuperVIPExpAt json.Number `json:"super_vip_exp_at"`
	}
	if err := c.call(ctx, "member", http.MethodGet, memberPath, q, nil, &data); err != nil {
		return nil, err
	}
	used, err := numInt64(data.UseCapacity)
	if err != nil {
		return nil, errors.New("quark: member: invalid use_capacity")
	}
	total, err := numInt64(data.TotalCapacity)
	if err != nil {
		return nil, errors.New("quark: member: invalid total_capacity")
	}
	exp, err := numInt64(data.SuperVIPExpAt)
	if err != nil {
		exp = 0 // optional, informational only
	}
	return &Capacity{Used: used, Total: total, MemberType: data.MemberType, VIPExpAt: exp}, nil
}

func numInt64(n json.Number) (int64, error) {
	if n == "" {
		return 0, nil
	}
	if v, err := strconv.ParseInt(n.String(), 10, 64); err == nil {
		return v, nil
	}
	f, err := n.Float64()
	if err != nil {
		return 0, err
	}
	return int64(f), nil
}

// panEnvelope is the response shape of pan.quark.cn account endpoints.
// code may be a string or a number upstream, so it is kept raw.
type panEnvelope struct {
	Success bool            `json:"success"`
	Code    json.RawMessage `json:"code"`
	Msg     string          `json:"msg"`
	Data    json.RawMessage `json:"data"`
}

// panCall performs a GET against the fixed pan.quark.cn origin. It is kept
// separate from call because that host differs from c.baseURL and uses a
// different envelope. The same trusted-host check, cookie scoping (via
// setHeaders) and redirect policy (via c.hc) apply.
func (c *Client) panCall(ctx context.Context, op, p string, q url.Values, out any) error {
	u, err := url.Parse(panOrigin + p)
	if err != nil {
		return fmt.Errorf("quark: %s: %w", op, err)
	}
	if !c.trusted(u) {
		return fmt.Errorf("quark: %s: refusing untrusted api host", op)
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("quark: %s: %w", op, err)
	}
	c.setHeaders(req)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("quark: %s: %w", op, scrub(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Do not echo the body: account endpoints may carry credentials.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrBytes))
		return &HTTPError{Op: op, StatusCode: resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxJSONBytes+1))
	if err != nil {
		return fmt.Errorf("quark: %s: read body: %w", op, err)
	}
	if len(body) > maxJSONBytes {
		return fmt.Errorf("quark: %s: response too large", op)
	}
	var env panEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return fmt.Errorf("quark: %s: invalid json", op)
	}
	if !env.Success {
		// The account response can contain credentials; never echo its fields.
		ae := &APIError{Op: op, Status: resp.StatusCode, Message: "account request rejected"}
		code := strings.Trim(string(env.Code), `"`)
		if n, err := strconv.Atoi(code); err == nil {
			ae.Code = n
		}
		return ae
	}
	if len(env.Data) == 0 || string(env.Data) == "null" {
		return fmt.Errorf("quark: %s: %w", op, errMissingData)
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("quark: %s: invalid data", op)
	}
	return nil
}
