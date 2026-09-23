// SPDX-License-Identifier: AGPL-3.0-or-later

package updater

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// payload returns n deterministic bytes.
func payload(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/251)
	}
	return b
}

// rangeStart parses "bytes=N-" and returns N (0 when absent).
func rangeStart(t *testing.T, r *http.Request) int {
	t.Helper()
	h := r.Header.Get("Range")
	if h == "" {
		return 0
	}
	v := strings.TrimSuffix(strings.TrimPrefix(h, "bytes="), "-")
	n, err := strconv.Atoi(v)
	if err != nil {
		t.Errorf("bad Range header %q", h)
	}
	return n
}

// servePartial writes body[from:] as a 206 (or a 200 when from == 0).
func servePartial(w http.ResponseWriter, body []byte, from int) {
	if from > 0 {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", from, len(body)-1, len(body)))
		w.Header().Set("Content-Length", strconv.Itoa(len(body)-from))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	}
	_, _ = w.Write(body[from:])
}

// dropAfter sends the headers for the full body plus its first n bytes, then
// kills the connection, as a flaky link or CDN edge does.
func dropAfter(t *testing.T, w http.ResponseWriter, body []byte, n int) {
	t.Helper()
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body[:n])
	w.(http.Flusher).Flush()
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		t.Errorf("hijack: %v", err)
		return
	}
	conn.Close()
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestDownload_ResumesAfterDroppedConnection: a dropped connection used to
// fail the update and wait an hour. It now resumes with a Range request from
// the bytes already on disk.
func TestDownload_ResumesAfterDroppedConnection(t *testing.T) {
	t.Parallel()
	body := payload(256 * 1024)
	var hits atomic.Int32
	var mu sync.Mutex
	var ranges []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		ranges = append(ranges, r.Header.Get("Range"))
		mu.Unlock()
		if hits.Add(1) == 1 {
			dropAfter(t, w, body, 100*1024)
			return
		}
		servePartial(w, body, rangeStart(t, r))
	}))
	defer srv.Close()

	u := &Updater{client: srv.Client()}
	dst := filepath.Join(t.TempDir(), "archive")
	if err := u.downloadFile(srv.URL, dst); err != nil {
		t.Fatalf("downloadFile: %v", err)
	}
	if got := readFile(t, dst); !bytes.Equal(got, body) {
		t.Fatalf("downloaded %d bytes, mismatch with %d-byte body", len(got), len(body))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(ranges) != 2 || ranges[0] != "" || ranges[1] != fmt.Sprintf("bytes=%d-", 100*1024) {
		t.Errorf("Range headers = %q, want [\"\" \"bytes=102400-\"]", ranges)
	}
}

// TestDownload_ServerIgnoringRangeRestartsCleanly: when the server answers a
// resume with a full 200, the file must be rewritten, not appended to.
func TestDownload_ServerIgnoringRangeRestartsCleanly(t *testing.T) {
	t.Parallel()
	body := payload(64 * 1024)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			dropAfter(t, w, body, 10*1024)
			return
		}
		servePartial(w, body, 0) // ignores Range
	}))
	defer srv.Close()

	u := &Updater{client: srv.Client()}
	dst := filepath.Join(t.TempDir(), "archive")
	if err := u.downloadFile(srv.URL, dst); err != nil {
		t.Fatalf("downloadFile: %v", err)
	}
	if got := readFile(t, dst); !bytes.Equal(got, body) {
		t.Fatalf("got %d bytes, want exactly the %d-byte body", len(got), len(body))
	}
}

// TestDownload_MismatchedContentRangeStartsOver: a 206 that does not start
// where we asked is discarded and the download restarts from byte 0.
func TestDownload_MismatchedContentRangeStartsOver(t *testing.T) {
	t.Parallel()
	body := payload(32 * 1024)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch hits.Add(1) {
		case 1:
			dropAfter(t, w, body, 8*1024)
		case 2:
			servePartial(w, body, 4*1024) // wrong offset
		default:
			servePartial(w, body, rangeStart(t, r))
		}
	}))
	defer srv.Close()

	u := &Updater{client: srv.Client()}
	dst := filepath.Join(t.TempDir(), "archive")
	if err := u.downloadFile(srv.URL, dst); err != nil {
		t.Fatalf("downloadFile: %v", err)
	}
	if got := readFile(t, dst); !bytes.Equal(got, body) {
		t.Fatalf("got %d bytes, want exactly the %d-byte body", len(got), len(body))
	}
}

// TestDownload_SlowButSteadyIsNotCutOff: the old client had a 30 s TOTAL
// timeout, so a 19 MB archive needed ~5 Mbit/s. Scaled down: the API client
// here times out after 300 ms, yet a download that keeps trickling for ~1 s
// must complete, because downloads are bounded by idleness, not duration.
func TestDownload_SlowButSteadyIsNotCutOff(t *testing.T) {
	t.Parallel()
	body := payload(10 * 1024)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		for i := 0; i < 10; i++ {
			_, _ = w.Write(body[i*1024 : (i+1)*1024])
			w.(http.Flusher).Flush()
			time.Sleep(100 * time.Millisecond)
		}
	}))
	defer srv.Close()

	api := srv.Client()
	api.Timeout = 300 * time.Millisecond
	u := &Updater{client: api, dlIdleTimeout: 2 * time.Second}
	dst := filepath.Join(t.TempDir(), "archive")
	if err := u.downloadFile(srv.URL, dst); err != nil {
		t.Fatalf("steady ~1s download failed under a 300ms API timeout: %v", err)
	}
	if got := readFile(t, dst); !bytes.Equal(got, body) {
		t.Fatal("content mismatch")
	}
}

// TestDownload_StallIsRetriedThenResumed: a stream that stops sending is
// cut after the idle timeout and resumed.
func TestDownload_StallIsRetriedThenResumed(t *testing.T) {
	t.Parallel()
	body := payload(20 * 1024)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body[:5*1024])
			w.(http.Flusher).Flush()
			<-r.Context().Done() // stall until the client gives up
			return
		}
		servePartial(w, body, rangeStart(t, r))
	}))
	defer srv.Close()

	u := &Updater{client: srv.Client(), dlIdleTimeout: 200 * time.Millisecond}
	dst := filepath.Join(t.TempDir(), "archive")
	start := time.Now()
	if err := u.downloadFile(srv.URL, dst); err != nil {
		t.Fatalf("downloadFile: %v", err)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Errorf("stall recovery took %v", el)
	}
	if got := readFile(t, dst); !bytes.Equal(got, body) {
		t.Fatal("content mismatch after resume")
	}
	if hits.Load() != 2 {
		t.Errorf("requests = %d, want 2", hits.Load())
	}
}

// TestDownload_PersistentStallFailsWithClearError: after the last attempt
// the error says the download stalled, not "context canceled".
func TestDownload_PersistentStallFailsWithClearError(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	u := &Updater{client: srv.Client(), dlIdleTimeout: 100 * time.Millisecond}
	err := u.downloadFile(srv.URL, filepath.Join(t.TempDir(), "archive"))
	if !errors.Is(err, errDownloadStalled) {
		t.Fatalf("err = %v, want errDownloadStalled", err)
	}
	if !strings.Contains(err.Error(), "no data received for 100ms") {
		t.Errorf("error %q does not explain the stall", err)
	}
	if hits.Load() != downloadMaxAttempts {
		t.Errorf("attempts = %d, want %d", hits.Load(), downloadMaxAttempts)
	}
}

// TestDownload_ClientErrorsAreNotRetried: a 404 will not fix itself.
func TestDownload_ClientErrorsAreNotRetried(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	u := &Updater{client: srv.Client()}
	err := u.downloadFile(srv.URL, filepath.Join(t.TempDir(), "archive"))
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("err = %v, want HTTP 404", err)
	}
	if hits.Load() != 1 {
		t.Errorf("attempts = %d, want 1", hits.Load())
	}
}

// TestDownload_ServerErrorsAreRetried: a 503 from a CDN edge is transient.
func TestDownload_ServerErrorsAreRetried(t *testing.T) {
	t.Parallel()
	body := payload(4096)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		servePartial(w, body, 0)
	}))
	defer srv.Close()

	u := &Updater{client: srv.Client()}
	dst := filepath.Join(t.TempDir(), "archive")
	if err := u.downloadFile(srv.URL, dst); err != nil {
		t.Fatalf("downloadFile: %v", err)
	}
	if got := readFile(t, dst); !bytes.Equal(got, body) {
		t.Fatal("content mismatch")
	}
}

// TestDownload_StopAbortsPromptly: Stop (SIGTERM from the service manager)
// must not wait out a slow download.
func TestDownload_StopAbortsPromptly(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	u := &Updater{client: srv.Client(), stopCh: make(chan struct{}), dlIdleTimeout: time.Minute}
	time.AfterFunc(100*time.Millisecond, func() { close(u.stopCh) })
	start := time.Now()
	err := u.downloadFile(srv.URL, filepath.Join(t.TempDir(), "archive"))
	if err == nil || !strings.Contains(err.Error(), "updater stopping") {
		t.Fatalf("err = %v, want an abort because the updater is stopping", err)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Errorf("stop took %v to abort the download", el)
	}
}

// TestNew_ClientsTimeouts pins the split: short total timeout for API calls,
// none for downloads (idle + max-duration bounds apply instead).
func TestNew_ClientsTimeouts(t *testing.T) {
	t.Parallel()
	u := New(Config{})
	if u.client.Timeout != apiTimeout {
		t.Errorf("API client timeout = %v, want %v", u.client.Timeout, apiTimeout)
	}
	if u.dlClient == nil || u.dlClient.Timeout != 0 {
		t.Errorf("download client must have no total timeout, got %+v", u.dlClient)
	}
	if u.downloadClient() != u.dlClient {
		t.Error("downloadClient() should return dlClient when set")
	}
	tr, ok := u.dlClient.Transport.(*http.Transport)
	if !ok || tr.TLSHandshakeTimeout < 20*time.Second || tr.ResponseHeaderTimeout == 0 {
		t.Errorf("download transport timeouts not set: %+v", u.dlClient.Transport)
	}
	// A literal Updater (as tests build) derives a download client from
	// client with the total timeout removed.
	lit := &Updater{client: &http.Client{Timeout: time.Second}}
	if lit.downloadClient().Timeout != 0 {
		t.Error("derived download client kept the total timeout")
	}
	if (&Updater{}).downloadClient() == nil {
		t.Error("nil client must still yield a download client")
	}
}

func TestContentRangeStart(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want int64
		ok   bool
	}{
		{"bytes 100-199/200", 100, true},
		{" bytes 0-9/10 ", 0, true},
		{"bytes */200", 0, false},
		{"items 1-2/3", 0, false},
		{"", 0, false},
		{"bytes -5-9/10", 0, false},
	} {
		got, ok := contentRangeStart(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("contentRangeStart(%q) = %d,%v want %d,%v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}
