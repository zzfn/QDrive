// Package qrlogin implements the Quark app QR-code login flow and returns a
// Cookie header value for API use. It never persists or prints secrets.
package qrlogin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	clientID      = "386"
	clientVer     = "1.2"
	appVersion    = "3.24.0"
	channel       = "pckk@information_ch"
	scanLoginPage = "https://su.quark.cn/4_eMHBJ"
	uopBase       = "https://uop.quark.cn/cas/ajax"
	panLoginURL   = "https://pan.quark.cn/desktop/account/login"

	userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) quark-cloud-drive/" + appVersion

	statusOK      = 2000000
	statusWaiting = 50004001
	statusExpired = 50004002

	maxBody          = 64 << 10
	maxPollFailures  = 5
	maxRedirects     = 5
	trustedDomain    = "quark.cn"
	defaultTimeout   = 3 * time.Minute
	defaultInterval  = 2 * time.Second
	perRequestTimout = 20 * time.Second
)

var (
	// ErrExpired: the QR code expired before being confirmed.
	ErrExpired = errors.New("qrlogin: qr code expired")
	// ErrTimeout: overall login window elapsed.
	ErrTimeout = errors.New("qrlogin: login timed out")
	// ErrNoCookies: login finished but server issued no usable quark.cn cookie.
	ErrNoCookies = errors.New("qrlogin: no session cookies received")
	// ErrUntrustedRedirect: a redirect pointed outside quark.cn.
	ErrUntrustedRedirect = errors.New("qrlogin: redirect to untrusted host refused")
)

type config struct {
	transport http.RoundTripper
	timeout   time.Duration
	interval  time.Duration
	now       func() time.Time
}

// Login runs the QR login flow. show receives the QR content string to render.
// It returns a Cookie header value; the caller is responsible for storing it.
func Login(ctx context.Context, show func(qrURL string) error) (string, error) {
	return login(ctx, show, config{})
}

func login(ctx context.Context, show func(string) error, cfg config) (string, error) {
	if show == nil {
		return "", errors.New("qrlogin: show callback is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if cfg.timeout <= 0 {
		cfg.timeout = defaultTimeout
	}
	if cfg.interval <= 0 {
		cfg.interval = defaultInterval
	}
	if cfg.transport == nil {
		cfg.transport = http.DefaultTransport
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.timeout)
	defer cancel()

	machineID, err := newDeviceID()
	if err != nil {
		return "", err
	}
	s, err := newSession(cfg.transport, machineID)
	if err != nil {
		return "", err
	}

	token, err := s.getToken(ctx)
	if err != nil {
		return "", finalErr(ctx, err)
	}
	if err := show(buildScanURL(token)); err != nil {
		return "", fmt.Errorf("qrlogin: show qr: %w", err)
	}

	ticket, err := s.waitTicket(ctx, token, cfg.interval)
	if err != nil {
		return "", finalErr(ctx, err)
	}
	cookie, err := s.panLogin(ctx, ticket)
	if err != nil {
		return "", finalErr(ctx, err)
	}
	return cookie, nil
}

// finalErr maps context failures to stable errors.
func finalErr(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			return ErrTimeout
		}
		return ctxErr
	}
	return err
}

func newDeviceID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("qrlogin: device id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func isTrustedHost(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	}
	return h == trustedDomain || strings.HasSuffix(h, "."+trustedDomain)
}

func isTrustedURL(u *url.URL) bool {
	return u != nil && u.Scheme == "https" && u.User == nil && (u.Port() == "" || u.Port() == "443") && isTrustedHost(u.Host)
}

// quarkJar only accepts cookies set by quark.cn hosts.
type quarkJar struct{ inner *cookiejar.Jar }

func (j quarkJar) SetCookies(u *url.URL, cs []*http.Cookie) {
	if isTrustedURL(u) {
		j.inner.SetCookies(u, cs)
	}
}

func (j quarkJar) Cookies(u *url.URL) []*http.Cookie {
	if !isTrustedURL(u) {
		return nil
	}
	return j.inner.Cookies(u)
}

type session struct {
	hc        *http.Client
	jar       quarkJar
	machineID string
}

func newSession(rt http.RoundTripper, machineID string) (*session, error) {
	inner, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	jar := quarkJar{inner: inner}
	hc := &http.Client{
		Transport: rt,
		Jar:       jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return errors.New("qrlogin: too many redirects")
			}
			if !isTrustedURL(req.URL) {
				return ErrUntrustedRedirect
			}
			return nil
		},
	}
	return &session{hc: hc, jar: jar, machineID: machineID}, nil
}

func (s *session) baseParams() url.Values {
	v := url.Values{}
	v.Set("pr", "ucpro")
	v.Set("fr", "pc")
	v.Set("sys", "darwin")
	v.Set("client_id", clientID)
	v.Set("v", clientVer)
	v.Set("request_id", strconv.FormatInt(time.Now().UnixNano(), 10))
	v.Set("uc_param_str", "utprfr")
	v.Set("ut", s.machineID)
	return v
}

// get performs a GET and returns status code and a size-limited body.
// Errors never include the URL (which may carry token/ticket).
func (s *session) get(ctx context.Context, step, rawURL string) (int, []byte, error) {
	rctx, cancel := context.WithTimeout(ctx, perRequestTimout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("qrlogin: %s: build request", step)
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Referer", "https://pan.quark.cn/")
	req.Header.Set("Origin", "https://pan.quark.cn")
	resp, err := s.hc.Do(req)
	if err != nil {
		return 0, nil, sanitize(step, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return 0, nil, sanitize(step, err)
	}
	if len(b) > maxBody {
		return 0, nil, fmt.Errorf("qrlogin: %s: response too large", step)
	}
	return resp.StatusCode, b, nil
}

// sanitize strips URLs/details from transport errors, keeping only the class.
func sanitize(step string, err error) error {
	switch {
	case errors.Is(err, ErrUntrustedRedirect):
		return fmt.Errorf("qrlogin: %s: %w", step, ErrUntrustedRedirect)
	case errors.Is(err, context.Canceled):
		return fmt.Errorf("qrlogin: %s: %w", step, context.Canceled)
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("qrlogin: %s: %w", step, context.DeadlineExceeded)
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return fmt.Errorf("qrlogin: %s: network timeout", step)
	}
	return fmt.Errorf("qrlogin: %s: network error", step)
}

type apiResp struct {
	Status  int    `json:"status"`
	Message string `json:"message"`
	Data    struct {
		Members struct {
			Token         string `json:"token"`
			ServiceTicket string `json:"service_ticket"`
		} `json:"members"`
	} `json:"data"`
}

func (s *session) callUOP(ctx context.Context, step, path string, p url.Values) (*apiResp, error) {
	code, b, err := s.get(ctx, step, uopBase+path+"?"+p.Encode())
	if err != nil {
		return nil, err
	}
	if code < 200 || code > 299 {
		return nil, fmt.Errorf("qrlogin: %s: http %d", step, code)
	}
	var out apiResp
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("qrlogin: %s: invalid response", step)
	}
	return &out, nil
}

func (s *session) getToken(ctx context.Context) (string, error) {
	out, err := s.callUOP(ctx, "get token", "/getTokenForQrcodeLogin", s.baseParams())
	if err != nil {
		return "", err
	}
	if out.Status != statusOK || out.Data.Members.Token == "" {
		return "", fmt.Errorf("qrlogin: get token: status %d", out.Status)
	}
	return out.Data.Members.Token, nil
}

func buildScanURL(token string) string {
	q := url.Values{}
	q.Set("token", token)
	q.Set("client_id", clientID)
	q.Set("sch", channel)
	q.Set("sve", appVersion)
	q.Set("ssb", "weblogin")
	q.Set("uc_param_str", "")
	q.Set("uc_biz_str", "S:custom|OPT:SAREA@0|OPT:IMMERSIVE@1|OPT:BACK_BTN_STYLE@0")
	return scanLoginPage + "?" + q.Encode()
}

func (s *session) waitTicket(ctx context.Context, token string, interval time.Duration) (string, error) {
	failures := 0
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		p := s.baseParams()
		p.Set("token", token)
		out, err := s.callUOP(ctx, "poll", "/getServiceTicketByQrcodeToken", p)
		switch {
		case err != nil:
			if ctx.Err() != nil || errors.Is(err, ErrUntrustedRedirect) {
				return "", err
			}
			failures++
			if failures >= maxPollFailures {
				return "", err
			}
		case out.Status == statusOK:
			if out.Data.Members.ServiceTicket == "" {
				return "", errors.New("qrlogin: poll: empty ticket")
			}
			return out.Data.Members.ServiceTicket, nil
		case out.Status == statusExpired:
			return "", ErrExpired
		default: // waiting (50004001) or unknown transient status
			failures = 0
		}
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return "", ctx.Err()
		case <-t.C:
		}
	}
}

func (s *session) panLogin(ctx context.Context, ticket string) (string, error) {
	q := url.Values{}
	q.Set("st", ticket)
	q.Set("fr", "pc")
	q.Set("pr", "ucpro")
	q.Set("ut", s.machineID)
	code, _, err := s.get(ctx, "account login", panLoginURL+"?"+q.Encode())
	if err != nil {
		return "", err
	}
	if code < 200 || code > 299 {
		return "", fmt.Errorf("qrlogin: account login: http %d", code)
	}
	cookies := map[string]string{}
	for _, h := range []string{
		"https://quark.cn/",
		"https://uop.quark.cn/",
		"https://pan.quark.cn/",
		"https://drive-pc.quark.cn/",
	} {
		u, _ := url.Parse(h)
		for _, c := range s.jar.Cookies(u) {
			if c.Name != "" && c.Value != "" {
				cookies[c.Name] = c.Value
			}
		}
	}
	if len(cookies) == 0 {
		return "", ErrNoCookies
	}
	names := make([]string, 0, len(cookies))
	for n := range cookies {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = n + "=" + cookies[n]
	}
	return strings.Join(parts, "; "), nil
}
