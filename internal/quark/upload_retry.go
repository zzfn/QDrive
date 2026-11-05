package quark

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Part retry policy. Only the OSS part PUT (with its signing call) is
// retried: re-sending the same bytes to the same partNumber/uploadId is
// idempotent on OSS, and a part that did land is reported as 409
// PartAlreadyExist, which is verified below. upload/pre, update/hash, the
// OSS complete request and upload/finish are never retried, because a lost
// response leaves their server-side effect unknown.
const (
	partMaxRetries  = 3
	partBaseBackoff = time.Second
)

// uploadPartRetry calls uploadPart, retrying transient transport failures
// with exponential backoff (1s, 2s, 4s) while ctx is alive.
func (c *Client) uploadPartRetry(ctx context.Context, s *uploadSession, mimeType string, part int, chunk []byte, hctx string) (string, error) {
	var lastErr error
	for attempt := 0; ; attempt++ {
		etag, err := c.uploadPart(ctx, s, mimeType, part, chunk, hctx)
		if err == nil {
			return etag, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if attempt >= partMaxRetries || !isTransientNetErr(err) {
			break
		}
		t := time.NewTimer(partBaseBackoff << attempt)
		select {
		case <-ctx.Done():
			t.Stop()
			return "", ctx.Err()
		case <-t.C:
		}
	}
	return "", lastErr
}

// isTransientNetErr reports transport-level failures worth retrying
// (connection reset/refused, broken pipe, unexpected EOF, network
// timeouts). HTTP status errors, API errors and context cancellation are
// not transient.
func isTransientNetErr(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var he *HTTPError
	var ae *APIError
	if errors.As(err, &he) || errors.As(err, &ae) {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.EPIPE) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// partAlreadyExistETag handles an OSS 409 PartAlreadyExist response. The
// reported PartEtag is accepted only if PartNumber (when present) matches
// and it equals the MD5 of the bytes we meant to upload (OSS part ETags are
// the hex MD5 of the part data); otherwise the existing part is not ours
// and the upload fails rather than committing foreign data.
func partAlreadyExistETag(resp *http.Response, part int, chunk []byte) (string, error) {
	op := "upload part " + strconv.Itoa(part)
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBytes))
	var x struct {
		XMLName    xml.Name `xml:"Error"`
		Code       string   `xml:"Code"`
		PartEtag   string   `xml:"PartEtag"`
		PartNumber string   `xml:"PartNumber"`
	}
	if err := xml.Unmarshal(body, &x); err != nil || x.Code != "PartAlreadyExist" {
		return "", &HTTPError{Op: op, StatusCode: resp.StatusCode, Body: truncate(body)}
	}
	if x.PartNumber != "" && strings.TrimSpace(x.PartNumber) != strconv.Itoa(part) {
		return "", fmt.Errorf("quark: %s: PartAlreadyExist for part %q", op, x.PartNumber)
	}
	got := strings.Trim(strings.TrimSpace(x.PartEtag), "\"")
	sum := md5.Sum(chunk)
	want := hex.EncodeToString(sum[:])
	if got == "" || !strings.EqualFold(got, want) {
		return "", fmt.Errorf("quark: %s: existing part ETag does not match local data", op)
	}
	return "\"" + got + "\"", nil
}
