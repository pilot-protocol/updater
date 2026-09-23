// SPDX-License-Identifier: AGPL-3.0-or-later

package updater

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Release archives are 17-20 MB. A fixed total timeout (the old 30 s
// http.Client.Timeout) needs a sustained ~5 Mbit/s and restarts the whole
// download an hour later on anything slower. Downloads now fail only when
// they stop making progress, and resume where they stopped.
const (
	// defaultDownloadIdleTimeout aborts an attempt that receives no bytes for
	// this long (also bounds connect + TLS + response headers).
	defaultDownloadIdleTimeout = 60 * time.Second
	// downloadMaxDuration caps one file download across all attempts, so a
	// connection that trickles a byte a minute cannot pin the updater forever.
	downloadMaxDuration = 30 * time.Minute
	// downloadMaxAttempts is the number of tries per file (1 + resumes).
	downloadMaxAttempts = 4
	// defaultDownloadRetryBackoff is the base pause between attempts; the
	// n-th retry waits n times this.
	defaultDownloadRetryBackoff = 2 * time.Second
	// apiTimeout bounds a GitHub REST API call (small JSON responses).
	apiTimeout = 30 * time.Second
)

// errDownloadStalled is the cause recorded when an attempt receives no data
// for the idle timeout.
var errDownloadStalled = errors.New("download stalled")

// newTransport returns the HTTP transport used for API calls and downloads:
// the default transport with a longer TLS handshake budget (the default 10 s
// tripped on slow links) and an explicit response-header timeout.
func newTransport() *http.Transport {
	var t *http.Transport
	if dt, ok := http.DefaultTransport.(*http.Transport); ok {
		t = dt.Clone()
	} else {
		t = &http.Transport{Proxy: http.ProxyFromEnvironment}
	}
	t.TLSHandshakeTimeout = 20 * time.Second
	t.ResponseHeaderTimeout = 30 * time.Second
	return t
}

// downloadClient returns the client used for release asset downloads. It
// must have no total Timeout: progress is enforced by the idle timeout and
// the overall cap in downloadFile instead.
func (u *Updater) downloadClient() *http.Client {
	if u.dlClient != nil {
		return u.dlClient
	}
	if u.client == nil {
		return &http.Client{Transport: newTransport()}
	}
	c := *u.client
	c.Timeout = 0
	return &c
}

func (u *Updater) downloadIdleTimeout() time.Duration {
	if u.dlIdleTimeout > 0 {
		return u.dlIdleTimeout
	}
	return defaultDownloadIdleTimeout
}

// downloadFile fetches url into dst. An attempt that stalls, drops the
// connection or gets a 5xx is retried, resuming with an HTTP Range request
// from the bytes already on disk (or starting over when the server ignores
// Range). Integrity is still enforced afterwards by the SHA256 check.
func (u *Updater) downloadFile(url, dst string) error {
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()

	ctx, cancel := context.WithTimeout(context.Background(), downloadMaxDuration)
	defer cancel()
	if u.stopCh != nil {
		// Abort promptly when the updater is being stopped (SIGTERM from the
		// service manager) instead of holding shutdown for up to 30 minutes.
		go func() {
			select {
			case <-u.stopCh:
				cancel()
			case <-ctx.Done():
			}
		}()
	}

	client := u.downloadClient()
	var have int64
	var lastErr error
	for attempt := 1; attempt <= downloadMaxAttempts; attempt++ {
		if attempt > 1 {
			if err := sleepCtx(ctx, time.Duration(attempt-1)*u.dlRetryBackoff); err != nil {
				break
			}
			slog.Warn("download interrupted, retrying",
				"url", url, "attempt", attempt, "resume_from", have, "error", lastErr)
		}
		var retry bool
		have, retry, err = u.downloadAttempt(ctx, client, url, f, have)
		if err == nil {
			return nil
		}
		lastErr = err
		if !retry || ctx.Err() != nil {
			break
		}
	}
	if ctx.Err() != nil && !errors.Is(lastErr, errDownloadStalled) {
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			return fmt.Errorf("download did not finish within %s (%d bytes received): %w", downloadMaxDuration, have, lastErr)
		default:
			return fmt.Errorf("download aborted (updater stopping): %w", lastErr)
		}
	}
	return lastErr
}

// downloadAttempt makes one GET for url, resuming at offset when offset > 0,
// and appends the body to f. It returns the number of bytes now in f,
// whether a failure is worth retrying, and the error.
func (u *Updater) downloadAttempt(ctx context.Context, client *http.Client, url string, f *os.File, offset int64) (int64, bool, error) {
	idle := u.downloadIdleTimeout()
	actx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	timer := time.AfterFunc(idle, func() { cancel(errDownloadStalled) })
	defer timer.Stop()

	// explain turns a context cancellation caused by the idle timer into a
	// clear message instead of a bare "context canceled".
	explain := func(err error) error {
		if errors.Is(context.Cause(actx), errDownloadStalled) {
			return fmt.Errorf("no data received for %s (%d bytes so far): %w", idle, offset, errDownloadStalled)
		}
		return err
	}

	req, err := http.NewRequestWithContext(actx, http.MethodGet, url, nil)
	if err != nil {
		return offset, false, err
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := client.Do(req)
	if err != nil {
		return offset, true, explain(err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		if offset > 0 {
			// The server ignored the Range header: start over.
			if offset, err = restartFile(f); err != nil {
				return offset, false, err
			}
		}
	case resp.StatusCode == http.StatusPartialContent:
		start, ok := contentRangeStart(resp.Header.Get("Content-Range"))
		if !ok || start != offset {
			off, err := restartFile(f)
			if err != nil {
				return off, false, err
			}
			return off, true, fmt.Errorf("server resumed at an unexpected offset (Content-Range %q, wanted %d)", resp.Header.Get("Content-Range"), offset)
		}
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable && offset > 0:
		off, err := restartFile(f)
		if err != nil {
			return off, false, err
		}
		return off, true, fmt.Errorf("server refused to resume at byte %d (HTTP 416)", offset)
	default:
		return offset, resp.StatusCode >= 500, fmt.Errorf("HTTP %d for %s", resp.StatusCode, url)
	}

	// Read one byte past the cap so we can distinguish "exactly at the limit"
	// from "exceeded the limit". Silently truncating an oversize archive
	// would surface as a confusing "checksum mismatch" instead.
	body := &progressReader{r: resp.Body, onRead: func() { timer.Reset(idle) }}
	n, err := io.Copy(f, io.LimitReader(body, maxDownloadBytes+1-offset))
	offset += n
	if err != nil {
		return offset, true, explain(err)
	}
	if offset > maxDownloadBytes {
		return offset, false, fmt.Errorf("archive exceeds max download size %d bytes", maxDownloadBytes)
	}
	return offset, false, nil
}

// restartFile truncates f so the next write starts at byte 0.
func restartFile(f *os.File) (int64, error) {
	if err := f.Truncate(0); err != nil {
		return 0, err
	}
	_, err := f.Seek(0, io.SeekStart)
	return 0, err
}

// contentRangeStart parses the first byte position of a
// "bytes <start>-<end>/<total>" Content-Range header.
func contentRangeStart(h string) (int64, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(h), "bytes ")
	if !ok {
		return 0, false
	}
	startStr, _, ok := strings.Cut(rest, "-")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(startStr), 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// progressReader calls onRead after every read that returned data, which
// re-arms the idle timer.
type progressReader struct {
	r      io.Reader
	onRead func()
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.onRead()
	}
	return n, err
}

// sleepCtx waits for d or until ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
