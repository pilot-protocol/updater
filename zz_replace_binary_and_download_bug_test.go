// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !no_updater
// +build !no_updater

package updater

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestReplaceBinary_RejectsEmptySource ensures that a zero-byte staged
// binary never atomic-renames over the live destination. Previously the
// rename would succeed and brick the daemon on next start.
func TestReplaceBinary_RejectsEmptySource(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	src := filepath.Join(dir, "empty-src")
	if err := os.WriteFile(src, nil, 0755); err != nil {
		t.Fatalf("write empty src: %v", err)
	}

	dst := filepath.Join(dir, "live-bin")
	original := []byte("ORIGINAL DAEMON BINARY")
	if err := os.WriteFile(dst, original, 0755); err != nil {
		t.Fatalf("write dst: %v", err)
	}

	err := replaceBinary(src, dst)
	if err == nil {
		t.Fatal("replaceBinary returned nil for zero-byte source; expected error")
	}
	if !strings.Contains(err.Error(), "empty source") {
		t.Errorf("error %q does not mention empty source", err)
	}

	// Destination must be untouched — both contents and that no .new
	// scratch file was left behind beside it.
	got, readErr := os.ReadFile(dst)
	if readErr != nil {
		t.Fatalf("read dst after failed replace: %v", readErr)
	}
	if string(got) != string(original) {
		t.Errorf("dst was modified: got %q, want %q", got, original)
	}
	if _, err := os.Stat(dst + ".new"); err == nil {
		t.Errorf("scratch file %s.new was left behind", dst)
	}
}

// TestDownloadRejectsOversizedArchive ensures that downloadFile returns a
// clear "exceeds max download size" error instead of silently truncating
// the body (which previously surfaced as a confusing "checksum mismatch").
func TestDownloadRejectsOversizedArchive(t *testing.T) {
	t.Parallel()

	// Serve maxDownloadBytes + 1 KiB of zeros. We don't allocate the whole
	// body in memory — stream it instead, so the test stays cheap.
	const overshoot = 1024
	totalBytes := int64(maxDownloadBytes) + overshoot
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", totalBytes))
		w.WriteHeader(http.StatusOK)
		buf := make([]byte, 64*1024)
		var sent int64
		for sent < totalBytes {
			n := int64(len(buf))
			if totalBytes-sent < n {
				n = totalBytes - sent
			}
			if _, err := w.Write(buf[:n]); err != nil {
				return
			}
			sent += n
		}
	}))
	defer srv.Close()

	u := New(Config{})
	dst := filepath.Join(t.TempDir(), "oversize.tar.gz")
	err := u.downloadFile(srv.URL, dst)
	if err == nil {
		t.Fatal("downloadFile returned nil for oversize archive; expected error")
	}
	if !strings.Contains(err.Error(), "exceeds max download size") {
		t.Errorf("error %q does not mention exceeding max download size", err)
	}
	if strings.Contains(strings.ToLower(err.Error()), "checksum") {
		t.Errorf("error %q surfaces as a checksum problem; should fail explicitly on size", err)
	}
}
