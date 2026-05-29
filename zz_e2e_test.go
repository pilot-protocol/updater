// SPDX-License-Identifier: AGPL-3.0-or-later

package updater

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestE2E_PinnedVersionLifecycle exercises the full lifecycle:
//  1. Node starts on v1.0.0, pinned to v2.0.0 → installs v2.0.0
//  2. Pin cleared → updater follows latest (v3.0.0)
//  3. Downgrade pin to v1.0.0 → installs v1.0.0
//  4. Pin cleared again → back to latest (v3.0.0)
func TestE2E_PinnedVersionLifecycle(t *testing.T) {
	t.Parallel()

	installDir := t.TempDir()
	archiveName := fmt.Sprintf("pilot-%s-%s.tar.gz", runtime.GOOS, runtime.GOARCH)

	// Seed install directory with v1.0.0 binaries.
	os.WriteFile(filepath.Join(installDir, "pilot-daemon"), []byte("daemon-v1.0.0"), 0755)
	os.WriteFile(filepath.Join(installDir, "pilotctl"), []byte("pilotctl-v1.0.0"), 0755)
	os.WriteFile(filepath.Join(installDir, ".pilot-version"), []byte("v1.0.0\n"), 0644)

	// Build three release archives.
	type releaseData struct {
		archivePath string
		hash        string
	}
	releaseArchives := map[string]releaseData{}
	for _, tag := range []string{"v1.0.0", "v2.0.0", "v3.0.0"} {
		archivePath := filepath.Join(installDir, fmt.Sprintf("archive-%s.tar.gz", tag))
		files := map[string]string{
			"daemon":   fmt.Sprintf("daemon-%s", tag),
			"pilotctl": fmt.Sprintf("pilotctl-%s", tag),
		}
		createTarGzFile(t, archivePath, files)
		data, _ := os.ReadFile(archivePath)
		h := sha256.Sum256(data)
		releaseArchives[tag] = releaseData{archivePath, fmt.Sprintf("%x", h)}
	}

	// Mock GitHub API serving 3 releases.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		base := "http://" + r.Host

		// Asset downloads.
		for tag, rd := range releaseArchives {
			if path == "/download/archive-"+tag {
				http.ServeFile(w, r, rd.archivePath)
				return
			}
			if path == "/download/checksums-"+tag {
				w.Write([]byte(fmt.Sprintf("%s  %s\n", rd.hash, archiveName)))
				return
			}
		}

		w.Header().Set("Content-Type", "application/json")

		// /releases/latest → v3.0.0
		if path == "/repos/test/repo/releases/latest" {
			json.NewEncoder(w).Encode(GitHubRelease{
				TagName: "v3.0.0",
				Assets: []GitHubAsset{
					{Name: archiveName, BrowserDownloadURL: base + "/download/archive-v3.0.0"},
					{Name: "checksums.txt", BrowserDownloadURL: base + "/download/checksums-v3.0.0"},
				},
			})
			return
		}

		// /releases/tags/{tag}
		for tag := range releaseArchives {
			if path == "/repos/test/repo/releases/tags/"+tag {
				json.NewEncoder(w).Encode(GitHubRelease{
					TagName: tag,
					Assets: []GitHubAsset{
						{Name: archiveName, BrowserDownloadURL: base + "/download/archive-" + tag},
						{Name: "checksums.txt", BrowserDownloadURL: base + "/download/checksums-" + tag},
					},
				})
				return
			}
		}

		http.NotFound(w, r)
	}))
	defer srv.Close()

	newUpdater := func(pinned string) *Updater {
		return &Updater{
			config: Config{
				Repo:          "test/repo",
				InstallDir:    installDir,
				PinnedVersion: pinned,
			},
			client: newRewriteClient(srv),
			stopCh: make(chan struct{}),
			exitFn: func(int) {}, // suppress os.Exit
		}
	}

	readDaemon := func() string {
		data, err := os.ReadFile(filepath.Join(installDir, "pilot-daemon"))
		if err != nil {
			t.Fatalf("read daemon: %v", err)
		}
		return string(data)
	}

	readVersion := func() string {
		data, err := os.ReadFile(filepath.Join(installDir, ".pilot-version"))
		if err != nil {
			t.Fatalf("read version: %v", err)
		}
		return string(data)
	}

	// ── Step 1: Pin v2.0.0, current is v1.0.0 → should install v2.0.0 ──
	t.Log("Step 1: pinning to v2.0.0 (current v1.0.0)")
	u := newUpdater("v2.0.0")
	u.checkOnce()
	if got := readDaemon(); got != "daemon-v2.0.0" {
		t.Errorf("Step 1 daemon = %q, want daemon-v2.0.0", got)
	}
	if got := readVersion(); got != "v2.0.0\n" {
		t.Errorf("Step 1 version file = %q, want v2.0.0", got)
	}

	// ── Step 2: Un-pin → should follow latest (v3.0.0) ──
	t.Log("Step 2: un-pinning (should follow latest → v3.0.0)")
	u = newUpdater("") // empty = follow latest
	u.checkOnce()
	if got := readDaemon(); got != "daemon-v3.0.0" {
		t.Errorf("Step 2 daemon = %q, want daemon-v3.0.0", got)
	}
	if got := readVersion(); got != "v3.0.0\n" {
		t.Errorf("Step 2 version file = %q, want v3.0.0", got)
	}

	// ── Step 3: Downgrade pin to v1.0.0 ──
	t.Log("Step 3: downgrade pin to v1.0.0")
	u = newUpdater("v1.0.0")
	u.checkOnce()
	if got := readDaemon(); got != "daemon-v1.0.0" {
		t.Errorf("Step 3 daemon = %q, want daemon-v1.0.0", got)
	}
	if got := readVersion(); got != "v1.0.0\n" {
		t.Errorf("Step 3 version file = %q, want v1.0.0", got)
	}

	// ── Step 4: Un-pin again → should go back to latest (v3.0.0) ──
	t.Log("Step 4: un-pin again → back to latest (v3.0.0)")
	u = newUpdater("")
	u.checkOnce()
	if got := readDaemon(); got != "daemon-v3.0.0" {
		t.Errorf("Step 4 daemon = %q, want daemon-v3.0.0", got)
	}
	if got := readVersion(); got != "v3.0.0\n" {
		t.Errorf("Step 4 version file = %q, want v3.0.0", got)
	}

	// ── Step 5: Already on pinned version → no-op ──
	t.Log("Step 5: pin to already-installed v3.0.0 → no-op")
	u = newUpdater("v3.0.0")
	u.checkOnce()
	// No changes expected — the binary should still be daemon-v3.0.0.
	if got := readDaemon(); got != "daemon-v3.0.0" {
		t.Errorf("Step 5 daemon = %q, want daemon-v3.0.0 (unchanged)", got)
	}

	t.Log("All steps passed — pinned version lifecycle works correctly")
}

func createTarGzFile(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gw := gzip.NewWriter(f)
	defer gw.Close()
	tw := tar.NewWriter(gw)
	defer tw.Close()
	for name, content := range files {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0755, Size: int64(len(content))})
		tw.Write([]byte(content))
	}
}

// TestE2E_PinnedVersionUnchangedOnRestart verifies that when the pinned
// version is already installed, repeated checkOnce calls are no-ops
// (simulating multiple ticks / process restarts).
func TestE2E_PinnedVersionStaysPinnedAcrossTicks(t *testing.T) {
	t.Parallel()

	installDir := t.TempDir()
	archiveName := fmt.Sprintf("pilot-%s-%s.tar.gz", runtime.GOOS, runtime.GOARCH)

	// Current is v1.0.0, latest is v2.0.0, but we're pinned to v1.0.0.
	os.WriteFile(filepath.Join(installDir, "pilot-daemon"), []byte("daemon-v1.0.0"), 0755)
	os.WriteFile(filepath.Join(installDir, ".pilot-version"), []byte("v1.0.0\n"), 0644)

	// Build v2.0.0 archive (the one we do NOT want).
	archivePath := filepath.Join(installDir, "archive-v2.0.0.tar.gz")
	createTarGzFile(t, archivePath, map[string]string{
		"daemon": "daemon-v2.0.0-should-not-install",
	})
	data, _ := os.ReadFile(archivePath)
	h := sha256.Sum256(data)
	v2hash := fmt.Sprintf("%x", h)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := "http://" + r.Host
		w.Header().Set("Content-Type", "application/json")
		// Latest is v2.0.0 — but we're pinned to v1.0.0, so should never hit this.
		if r.URL.Path == "/repos/test/repo/releases/latest" {
			json.NewEncoder(w).Encode(GitHubRelease{
				TagName: "v2.0.0",
				Assets: []GitHubAsset{
					{Name: archiveName, BrowserDownloadURL: base + "/download/archive-v2.0.0"},
					{Name: "checksums.txt", BrowserDownloadURL: base + "/download/checksums-v2.0.0"},
				},
			})
			return
		}
		if r.URL.Path == "/download/archive-v2.0.0" {
			http.ServeFile(w, r, archivePath)
			return
		}
		if r.URL.Path == "/download/checksums-v2.0.0" {
			w.Write([]byte(fmt.Sprintf("%s  %s\n", v2hash, archiveName)))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	u := &Updater{
		config: Config{
			Repo:          "test/repo",
			InstallDir:    installDir,
			PinnedVersion: "v1.0.0",
		},
		client: newRewriteClient(srv),
		stopCh: make(chan struct{}),
		exitFn: func(int) {},
	}

	// Run 5 ticks — should all be no-ops.
	for i := 0; i < 5; i++ {
		u.checkOnce()
		data, _ := os.ReadFile(filepath.Join(installDir, "pilot-daemon"))
		if string(data) != "daemon-v1.0.0" {
			t.Fatalf("tick %d: daemon unexpectedly changed to %q", i, string(data))
		}
	}

	t.Log("Pinned version stayed frozen across 5 ticks — no unwanted auto-update")
}

// TestE2E_NoPinFollowsLatest verifies the default behaviour (no pin) still
// follows the latest release, including across multiple version jumps.
func TestE2E_NoPinFollowsLatest(t *testing.T) {
	t.Parallel()

	installDir := t.TempDir()
	archiveName := fmt.Sprintf("pilot-%s-%s.tar.gz", runtime.GOOS, runtime.GOARCH)

	os.WriteFile(filepath.Join(installDir, "pilot-daemon"), []byte("old"), 0755)
	os.WriteFile(filepath.Join(installDir, ".pilot-version"), []byte("v1.0.0\n"), 0644)

	archivePath := filepath.Join(installDir, "archive-v5.0.0.tar.gz")
	createTarGzFile(t, archivePath, map[string]string{"daemon": "daemon-v5.0.0"})
	data, _ := os.ReadFile(archivePath)
	h := sha256.Sum256(data)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := "http://" + r.Host
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/repos/test/repo/releases/latest":
			json.NewEncoder(w).Encode(GitHubRelease{
				TagName: "v5.0.0",
				Assets: []GitHubAsset{
					{Name: archiveName, BrowserDownloadURL: base + "/download/archive"},
					{Name: "checksums.txt", BrowserDownloadURL: base + "/download/checksums"},
				},
			})
		case r.URL.Path == "/download/archive":
			http.ServeFile(w, r, archivePath)
		case r.URL.Path == "/download/checksums":
			w.Write([]byte(fmt.Sprintf("%x  %s\n", h, archiveName)))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	var u *Updater
	u = &Updater{
		config: Config{
			Repo:       "test/repo",
			InstallDir: installDir,
		},
		client: newRewriteClient(srv),
		stopCh: make(chan struct{}),
		exitFn: func(int) {},
	}

	u.checkOnce()
	data, _ = os.ReadFile(filepath.Join(installDir, "pilot-daemon"))
	if string(data) != "daemon-v5.0.0" {
		t.Errorf("no-pin mode did not follow latest: got %q, want daemon-v5.0.0", string(data))
	}
	t.Log("No-pin mode correctly auto-updated to v5.0.0")
}
