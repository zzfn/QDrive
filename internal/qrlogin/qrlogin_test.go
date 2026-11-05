package qrlogin

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTrustedURLs(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://pan.quark.cn/path":          true,
		"https://pan.quark.cn:443/path":      true,
		"https://pan.quark.cn:8443/path":     false,
		"https://user@pan.quark.cn/path":     false,
		"http://pan.quark.cn/path":           false,
		"https://quark.cn.evil.example/path": false,
	} {
		u, err := url.Parse(raw)
		if err != nil || isTrustedURL(u) != want {
			t.Errorf("isTrustedURL(%q) = %v, want %v (parse: %v)", raw, isTrustedURL(u), want, err)
		}
	}
}

const (
	secretToken  = "TOKEN-SECRET-123"
	secretTicket = "TICKET-SECRET-456"
)

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func resp(r *http.Request, code int, body string, hdr http.Header) *http.Response {
	if hdr == nil {
		hdr = http.Header{}
	}
	return &http.Response{StatusCode: code, Header: hdr, Body: io.NopCloser(strings.NewReader(body)), Request: r}
}

func tokenBody() string {
	return `{"status":2000000,"data":{"members":{"token":"` + secretToken + `"}}}`
}

type fake struct {
	mu        sync.Mutex
	polls     int
	pollPlan  []string // bodies per poll; last repeats
	loginResp func(*http.Request) *http.Response
	hosts     []string
	seen      []*url.URL
}

func (f *fake) rt(t *testing.T) http.RoundTripper {
	return rtFunc(func(r *http.Request) (*http.Response, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.hosts = append(f.hosts, r.URL.Host)
		f.seen = append(f.seen, r.URL)
		if r.URL.Host != "uop.quark.cn" && r.URL.Host != "pan.quark.cn" {
			t.Errorf("unexpected host %s", r.URL.Host)
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/getTokenForQrcodeLogin"):
			return resp(r, 200, tokenBody(), nil), nil
		case strings.HasSuffix(r.URL.Path, "/getServiceTicketByQrcodeToken"):
			if r.URL.Query().Get("token") != secretToken {
				t.Errorf("poll missing token")
			}
			i := f.polls
			if i >= len(f.pollPlan) {
				i = len(f.pollPlan) - 1
			}
			f.polls++
			return resp(r, 200, f.pollPlan[i], nil), nil
		case r.URL.Path == "/desktop/account/login":
			if r.URL.Query().Get("st") != secretTicket {
				t.Errorf("login missing ticket")
			}
			return f.loginResp(r), nil
		}
		return resp(r, 404, "", nil), nil
	})
}

const (
	waiting = `{"status":50004001,"message":"Query result is empty"}`
	expired = `{"status":50004002}`
	okPoll  = `{"status":2000000,"data":{"members":{"service_ticket":"` + secretTicket + `"}}}`
)

func goodLogin(r *http.Request) *http.Response {
	h := http.Header{}
	h.Add("Set-Cookie", "__puus=P1; Domain=quark.cn; Path=/; Secure")
	h.Add("Set-Cookie", "__pus=P2; Path=/")
	h.Add("Set-Cookie", "evil=X; Domain=example.com; Path=/")
	return resp(r, 200, `{"status":200}`, h)
}

func cfg(rt http.RoundTripper) config {
	return config{transport: rt, timeout: 2 * time.Second, interval: time.Millisecond}
}

func TestLoginSuccess(t *testing.T) {
	f := &fake{pollPlan: []string{waiting, waiting, okPoll}, loginResp: goodLogin}
	var shown string
	got, err := login(context.Background(), func(s string) error { shown = s; return nil }, cfg(f.rt(t)))
	if err != nil {
		t.Fatal(err)
	}
	if got != "__pus=P2; __puus=P1" {
		t.Fatalf("cookie = %q", got)
	}
	u, err := url.Parse(shown)
	if err != nil || u.Host != "su.quark.cn" || u.Query().Get("token") != secretToken || u.Query().Get("client_id") != "386" {
		t.Fatalf("bad qr %q", shown)
	}
	if f.polls != 3 {
		t.Fatalf("polls = %d", f.polls)
	}
	ut := f.seen[0].Query().Get("ut")
	if len(ut) != 32 {
		t.Fatalf("device id %q", ut)
	}
}

func TestExpired(t *testing.T) {
	f := &fake{pollPlan: []string{waiting, expired}}
	_, err := login(context.Background(), func(string) error { return nil }, cfg(f.rt(t)))
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("err = %v", err)
	}
}

func TestTimeout(t *testing.T) {
	f := &fake{pollPlan: []string{waiting}}
	c := cfg(f.rt(t))
	c.timeout = 30 * time.Millisecond
	_, err := login(context.Background(), func(string) error { return nil }, c)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v", err)
	}
}

func TestCancel(t *testing.T) {
	f := &fake{pollPlan: []string{waiting}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := login(ctx, func(string) error { cancel(); return nil }, cfg(f.rt(t)))
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel not honored")
	}
}

func TestShowErrorStops(t *testing.T) {
	f := &fake{pollPlan: []string{waiting}}
	_, err := login(context.Background(), func(string) error { return errors.New("boom") }, cfg(f.rt(t)))
	if err == nil || f.polls != 0 {
		t.Fatalf("err=%v polls=%d", err, f.polls)
	}
}

func TestPrivacyNetworkError(t *testing.T) {
	n := 0
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		n++
		if strings.HasSuffix(r.URL.Path, "/getTokenForQrcodeLogin") {
			return resp(r, 200, tokenBody(), nil), nil
		}
		return nil, errors.New("dial failed for " + r.URL.String())
	})
	_, err := login(context.Background(), func(string) error { return nil }, cfg(rt))
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), secretToken) || strings.Contains(err.Error(), "dial failed") {
		t.Fatalf("leaked: %v", err)
	}
	if n != 1+maxPollFailures {
		t.Fatalf("requests = %d", n)
	}
}

func TestPrivacyLoginError(t *testing.T) {
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/getTokenForQrcodeLogin"):
			return resp(r, 200, tokenBody(), nil), nil
		case strings.HasSuffix(r.URL.Path, "/getServiceTicketByQrcodeToken"):
			return resp(r, 200, okPoll, nil), nil
		}
		return resp(r, 500, "error st="+secretTicket, nil), nil
	})
	_, err := login(context.Background(), func(string) error { return nil }, cfg(rt))
	if err == nil || strings.Contains(err.Error(), secretTicket) || strings.Contains(err.Error(), secretToken) {
		t.Fatalf("err = %v", err)
	}
}

func TestBadTokenBodyNotEchoed(t *testing.T) {
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		return resp(r, 200, `{"status":1,"message":"x","data":{"members":{"token":"`+secretToken+`"}}}`, nil), nil
	})
	_, err := login(context.Background(), func(string) error { return nil }, cfg(rt))
	if err == nil || strings.Contains(err.Error(), secretToken) {
		t.Fatalf("err = %v", err)
	}
}

func TestOversizedBody(t *testing.T) {
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		return resp(r, 200, strings.Repeat("a", maxBody+10), nil), nil
	})
	_, err := login(context.Background(), func(string) error { return nil }, cfg(rt))
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("err = %v", err)
	}
}

func TestRedirectOutsideRefused(t *testing.T) {
	var evil bool
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "uop.quark.cn" {
			evil = true
			return resp(r, 200, tokenBody(), nil), nil
		}
		h := http.Header{"Location": {"https://quark.cn.evil.com/steal"}}
		return resp(r, 302, "", h), nil
	})
	_, err := login(context.Background(), func(string) error { return nil }, cfg(rt))
	if !errors.Is(err, ErrUntrustedRedirect) || evil {
		t.Fatalf("err=%v evil=%v", err, evil)
	}
}

func TestRedirectHTTPRefused(t *testing.T) {
	f := &fake{pollPlan: []string{okPoll}, loginResp: func(r *http.Request) *http.Response {
		return resp(r, 302, "", http.Header{"Location": {"http://pan.quark.cn/x"}})
	}}
	_, err := login(context.Background(), func(string) error { return nil }, cfg(f.rt(t)))
	if !errors.Is(err, ErrUntrustedRedirect) {
		t.Fatalf("err = %v", err)
	}
}

func TestRedirectWithinQuarkFollowed(t *testing.T) {
	f := &fake{pollPlan: []string{okPoll}}
	f.loginResp = func(r *http.Request) *http.Response {
		h := http.Header{"Location": {"https://pan.quark.cn/done"}, "Set-Cookie": {"a=1; Path=/"}}
		return resp(r, 302, "", h)
	}
	base := f.rt(t)
	rt := rtFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/done" {
			return resp(r, 200, "{}", http.Header{"Set-Cookie": {"b=2; Path=/"}}), nil
		}
		return base.RoundTrip(r)
	})
	got, err := login(context.Background(), func(string) error { return nil }, cfg(rt))
	if err != nil || got != "a=1; b=2" {
		t.Fatalf("got=%q err=%v", got, err)
	}
}

func TestNoCookies(t *testing.T) {
	f := &fake{pollPlan: []string{okPoll}, loginResp: func(r *http.Request) *http.Response {
		return resp(r, 200, "{}", http.Header{"Set-Cookie": {"x=1; Domain=example.com"}})
	}}
	_, err := login(context.Background(), func(string) error { return nil }, cfg(f.rt(t)))
	if !errors.Is(err, ErrNoCookies) {
		t.Fatalf("err = %v", err)
	}
}

func TestTrustedHost(t *testing.T) {
	for h, want := range map[string]bool{
		"quark.cn": true, "pan.quark.cn": true, "PAN.QUARK.CN.": true, "pan.quark.cn:443": true,
		"evilquark.cn": false, "quark.cn.evil.com": false, "": false,
	} {
		if isTrustedHost(h) != want {
			t.Errorf("%q", h)
		}
	}
}

func TestDeviceIDRandom(t *testing.T) {
	a, _ := newDeviceID()
	b, _ := newDeviceID()
	if a == b || len(a) != 32 {
		t.Fatal(a, b)
	}
}
