// SPDX-License-Identifier: AGPL-3.0-or-later

package updater

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestApplyUpdate_FailsWhenChecksumsAssetMissing pins the P0 RCE-vector
// fix: a GitHub release that ships ONLY the archive (no checksums.txt
// asset) must NOT install the binary unverified.
//
// Before the fix: updater.go line 241 (`if checksumsURL != ""`) silently
// skipped verification with no warning or error, and the binary was
// installed blind. An attacker with GitHub repo write access could
// publish just the malicious archive (omitting checksums.txt) and
// every Pilot node would auto-install it.
func TestApplyUpdate_FailsWhenChecksumsAssetMissing(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	installDir := filepath.Join(tmpDir, "bin")
	os.MkdirAll(installDir, 0755)
	os.WriteFile(filepath.Join(installDir, ".pilot-version"), []byte("v1.0.0\n"), 0644)

	archiveDir := t.TempDir()
	archiveName := fmt.Sprintf("pilot-%s-%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	archivePath := filepath.Join(archiveDir, archiveName)
	createTestTarGz(t, archivePath, map[string]string{
		"daemon": "malicious-content",
	})
	archiveContent, _ := os.ReadFile(archivePath)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/download/" + archiveName:
			w.Write(archiveContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	u := &Updater{
		config: Config{InstallDir: installDir},
		client: srv.Client(),
		stopCh: make(chan struct{}),
		exitFn: func(int) {},
	}

	// Release with archive but NO checksums.txt asset.
	release := &GitHubRelease{
		TagName: "v1.1.0",
		Assets: []GitHubAsset{
			{Name: archiveName, BrowserDownloadURL: srv.URL + "/download/" + archiveName},
		},
	}

	err := u.applyUpdate(release)
	if err == nil {
		// Confirm the malicious binary was installed unverified.
		got, _ := os.ReadFile(filepath.Join(installDir, "pilot-daemon"))
		t.Fatalf(
			"BUG: applyUpdate returned nil with no checksums.txt — "+
				"binary installed unverified (content len=%d). "+
				"Fix: refuse install when checksums.txt is missing.",
			len(got),
		)
	}
	if !strings.Contains(err.Error(), "checksums") {
		t.Fatalf("expected error mentioning 'checksums', got: %v", err)
	}
}

// TestApplyUpdate_FailsWhenChecksumsDownloadFails pins the second P0:
// if the checksums.txt download itself fails (server flake, MITM
// dropping just that request, asset URL moved), the updater currently
// logs slog.Warn and CONTINUES to install the unverified binary.
//
// Before the fix: updater.go line 244 — `slog.Warn(...)` then falls
// through to extraction with no return.
func TestApplyUpdate_FailsWhenChecksumsDownloadFails(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	installDir := filepath.Join(tmpDir, "bin")
	os.MkdirAll(installDir, 0755)
	os.WriteFile(filepath.Join(installDir, ".pilot-version"), []byte("v1.0.0\n"), 0644)

	archiveDir := t.TempDir()
	archiveName := fmt.Sprintf("pilot-%s-%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	archivePath := filepath.Join(archiveDir, archiveName)
	createTestTarGz(t, archivePath, map[string]string{
		"daemon": "malicious-content",
	})
	archiveContent, _ := os.ReadFile(archivePath)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/download/" + archiveName:
			w.Write(archiveContent)
		case "/download/checksums.txt":
			// Simulate MITM dropping or server flake on the checksums fetch.
			http.Error(w, "internal server error", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	u := &Updater{
		config: Config{InstallDir: installDir},
		client: srv.Client(),
		stopCh: make(chan struct{}),
		exitFn: func(int) {},
	}

	release := &GitHubRelease{
		TagName: "v1.1.0",
		Assets: []GitHubAsset{
			{Name: archiveName, BrowserDownloadURL: srv.URL + "/download/" + archiveName},
			{Name: "checksums.txt", BrowserDownloadURL: srv.URL + "/download/checksums.txt"},
		},
	}

	err := u.applyUpdate(release)
	if err == nil {
		got, _ := os.ReadFile(filepath.Join(installDir, "pilot-daemon"))
		t.Fatalf(
			"BUG: applyUpdate returned nil when checksums download "+
				"failed — binary installed unverified (content len=%d). "+
				"Fix: treat checksums download failure as fatal.",
			len(got),
		)
	}
	if !strings.Contains(err.Error(), "checksums") {
		t.Fatalf("expected error mentioning 'checksums', got: %v", err)
	}
}

// TestApplyUpdate_PassesWithValidChecksums is the green-path control:
// when checksums.txt is present, downloads cleanly, AND matches the
// archive, the install must still succeed. Prevents the fixes above
// from accidentally over-rejecting the normal path.
func TestApplyUpdate_PassesWithValidChecksums(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	installDir := filepath.Join(tmpDir, "bin")
	os.MkdirAll(installDir, 0755)
	os.WriteFile(filepath.Join(installDir, ".pilot-version"), []byte("v1.0.0\n"), 0644)

	archiveDir := t.TempDir()
	archiveName := fmt.Sprintf("pilot-%s-%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	archivePath := filepath.Join(archiveDir, archiveName)
	createTestTarGz(t, archivePath, map[string]string{
		"daemon": "good-content",
	})
	archiveContent, _ := os.ReadFile(archivePath)
	hash := sha256.Sum256(archiveContent)
	checksumsContent := fmt.Sprintf("%x  %s\n", hash, archiveName)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/download/" + archiveName:
			w.Write(archiveContent)
		case "/download/checksums.txt":
			w.Write([]byte(checksumsContent))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	u := &Updater{
		config: Config{InstallDir: installDir},
		client: srv.Client(),
		stopCh: make(chan struct{}),
		exitFn: func(int) {},
	}

	release := &GitHubRelease{
		TagName: "v1.1.0",
		Assets: []GitHubAsset{
			{Name: archiveName, BrowserDownloadURL: srv.URL + "/download/" + archiveName},
			{Name: "checksums.txt", BrowserDownloadURL: srv.URL + "/download/checksums.txt"},
		},
	}

	if err := u.applyUpdate(release); err != nil {
		t.Fatalf("green-path install failed: %v", err)
	}
}
