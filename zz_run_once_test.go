// SPDX-License-Identifier: AGPL-3.0-or-later

package updater

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

// seedInstallDir writes the minimum files for an Updater test: a dummy
// pilot-daemon binary at the given version and a .daemon-last-restart record
// stamped AFTER the binary so recoverPendingRestart skips the launchctl path.
func seedInstallDir(t *testing.T, dir, version string) {
	t.Helper()
	bin := filepath.Join(dir, "pilot-daemon")
	os.WriteFile(bin, []byte("stub-"+version), 0755)
	os.WriteFile(filepath.Join(dir, ".pilot-version"), []byte(version+"\n"), 0644)
	// Touch the restart record one second in the future so the binary appears
	// older than the record — recoverPendingRestart will skip the restart.
	future := time.Now().Add(time.Second)
	record := filepath.Join(dir, ".daemon-last-restart")
	os.WriteFile(record, []byte(future.Format(time.RFC3339)+"\n"), 0644)
	os.Chtimes(record, future, future)
}

// TestRunOnce_IsSynchronous verifies that RunOnce blocks until the check
// completes and returns to the caller (not fire-and-forget like Start).
func TestRunOnce_IsSynchronous(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	seedInstallDir(t, tmp, "v1.0.0")

	// Server introduces a small delay so a fire-and-forget RunOnce would
	// return before the flag is set.
	checkStarted := make(chan struct{})
	checkFinished := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(checkStarted)
		// Signal complete via channel so caller can wait.
		defer close(checkFinished)
		json.NewEncoder(w).Encode(GitHubRelease{TagName: "v1.0.0", Assets: []GitHubAsset{}})
	}))
	defer srv.Close()

	u := &Updater{
		config: Config{Repo: "owner/repo", InstallDir: tmp},
		client: newRewriteClient(srv),
		stopCh: make(chan struct{}),
	}

	// RunOnce must not return before the HTTP handler has finished.
	done := make(chan struct{})
	go func() {
		u.RunOnce()
		close(done)
	}()

	select {
	case <-checkStarted:
		// Good — check started. Now wait for both handler and RunOnce to finish.
	case <-done:
		// RunOnce returned without the server ever being hit — bug.
		t.Fatal("RunOnce returned before network check started")
	}

	<-checkFinished
	select {
	case <-done:
		// RunOnce returned after check finished — correct.
	case <-time.After(5 * time.Second):
		t.Error("RunOnce did not return within 5s after check completed")
	}
}

// TestRunOnce_SingleCheck verifies RunOnce makes exactly one check and does
// not enter the periodic ticker loop.
func TestRunOnce_SingleCheck(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	seedInstallDir(t, tmp, "v1.0.0")

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		json.NewEncoder(w).Encode(GitHubRelease{TagName: "v1.0.0", Assets: []GitHubAsset{}})
	}))
	defer srv.Close()

	u := &Updater{
		config: Config{Repo: "owner/repo", InstallDir: tmp},
		client: newRewriteClient(srv),
		stopCh: make(chan struct{}),
	}

	u.RunOnce()

	if n := hits.Load(); n != 1 {
		t.Errorf("expected exactly 1 GitHub API call, got %d", n)
	}
}

// TestRunOnce_AlreadyUpToDate verifies that RunOnce is a no-op when the
// current version matches the latest release.
func TestRunOnce_AlreadyUpToDate(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	seedInstallDir(t, tmp, "v2.0.0")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(GitHubRelease{TagName: "v2.0.0", Assets: []GitHubAsset{}})
	}))
	defer srv.Close()

	u := &Updater{
		config: Config{Repo: "owner/repo", InstallDir: tmp},
		client: newRewriteClient(srv),
		stopCh: make(chan struct{}),
	}

	u.RunOnce()

	// Daemon binary must not have changed.
	data, _ := os.ReadFile(filepath.Join(tmp, "pilot-daemon"))
	if string(data) != "stub-v2.0.0" {
		t.Errorf("pilot-daemon was modified when already up to date; got %q", string(data))
	}
}

// TestRunOnce_AppliesUpdate verifies that RunOnce downloads and applies a
// newer release when one is available, identical to what checkOnce does.
func TestRunOnce_AppliesUpdate(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	seedInstallDir(t, tmp, "v1.0.0")
	os.WriteFile(filepath.Join(tmp, "pilotctl"), []byte("old-pilotctl"), 0755)

	archiveName := fmt.Sprintf("pilot-%s-%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	archiveDir := t.TempDir()
	archivePath := filepath.Join(archiveDir, archiveName)
	createTestTarGz(t, archivePath, map[string]string{
		"daemon":   "new-daemon-v2",
		"pilotctl": "new-pilotctl-v2",
	})
	archiveContent, _ := os.ReadFile(archivePath)
	archiveHash := sha256.Sum256(archiveContent)
	checksums := fmt.Sprintf("%x  %s\n", archiveHash, archiveName)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/owner/repo/releases/latest":
			json.NewEncoder(w).Encode(GitHubRelease{
				TagName: "v2.0.0",
				Assets: []GitHubAsset{
					{Name: archiveName, BrowserDownloadURL: "http://" + r.Host + "/dl/" + archiveName},
					{Name: "checksums.txt", BrowserDownloadURL: "http://" + r.Host + "/dl/checksums.txt"},
				},
			})
		case "/dl/" + archiveName:
			w.Write(archiveContent)
		case "/dl/checksums.txt":
			w.Write([]byte(checksums))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	exitCalled := false
	u := &Updater{
		config: Config{Repo: "owner/repo", InstallDir: tmp},
		client: newRewriteClient(srv),
		stopCh: make(chan struct{}),
		exitFn: func(int) { exitCalled = true },
	}

	u.RunOnce()

	data, err := os.ReadFile(filepath.Join(tmp, "pilot-daemon"))
	if err != nil {
		t.Fatalf("read pilot-daemon: %v", err)
	}
	if string(data) != "new-daemon-v2" {
		t.Errorf("pilot-daemon = %q, want 'new-daemon-v2'", string(data))
	}
	_ = exitCalled // updater self-replacement may or may not fire depending on binary name
}

// TestRunOnce_PinnedVersion verifies RunOnce respects Config.PinnedVersion,
// fetching the exact pinned tag rather than the latest release.
func TestRunOnce_PinnedVersion(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	seedInstallDir(t, tmp, "v1.0.0")
	os.WriteFile(filepath.Join(tmp, "pilotctl"), []byte("old"), 0755)

	archiveName := fmt.Sprintf("pilot-%s-%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	archiveDir := t.TempDir()
	archivePath := filepath.Join(archiveDir, archiveName)
	createTestTarGz(t, archivePath, map[string]string{
		"daemon":   "pinned-daemon",
		"pilotctl": "pinned-pilotctl",
	})
	archiveContent, _ := os.ReadFile(archivePath)
	archiveHash := sha256.Sum256(archiveContent)
	checksums := fmt.Sprintf("%x  %s\n", archiveHash, archiveName)

	var tagHit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/owner/repo/releases/tags/v1.5.0":
			tagHit = true
			json.NewEncoder(w).Encode(GitHubRelease{
				TagName: "v1.5.0",
				Assets: []GitHubAsset{
					{Name: archiveName, BrowserDownloadURL: "http://" + r.Host + "/dl/" + archiveName},
					{Name: "checksums.txt", BrowserDownloadURL: "http://" + r.Host + "/dl/checksums.txt"},
				},
			})
		case r.URL.Path == "/dl/"+archiveName:
			w.Write(archiveContent)
		case r.URL.Path == "/dl/checksums.txt":
			w.Write([]byte(checksums))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	u := &Updater{
		config: Config{Repo: "owner/repo", InstallDir: tmp, PinnedVersion: "v1.5.0"},
		client: newRewriteClient(srv),
		stopCh: make(chan struct{}),
		exitFn: func(int) {},
	}

	u.RunOnce()

	if !tagHit {
		t.Error("RunOnce with PinnedVersion did not call the by-tag API endpoint")
	}
	data, _ := os.ReadFile(filepath.Join(tmp, "pilot-daemon"))
	if string(data) != "pinned-daemon" {
		t.Errorf("pilot-daemon = %q, want 'pinned-daemon'", string(data))
	}
}
