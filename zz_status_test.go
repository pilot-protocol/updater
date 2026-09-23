// SPDX-License-Identifier: AGPL-3.0-or-later

package updater

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// restartSpy captures daemon-restart attempts on both platforms: launchctl
// on macOS (runCmd) and SIGTERM via a fake /proc on Linux (killFn). The fake
// /proc holds one daemon whose exe reads "<path> (deleted)", which is what a
// real daemon looks like right after its binary was renamed over.
type restartSpy struct {
	mu    sync.Mutex
	calls []string
	fail  error
}

func (r *restartSpy) install(t *testing.T, u *Updater) {
	t.Helper()
	u.runCmd = func(name string, args ...string) ([]byte, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.calls = append(r.calls, name+" "+strings.Join(args, " "))
		if r.fail != nil {
			return []byte("launchd says no"), r.fail
		}
		return nil, nil
	}
	proc := t.TempDir()
	if err := os.MkdirAll(filepath.Join(proc, "3999999"), 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(u.config.InstallDir, "pilot-daemon") + " (deleted)"
	if err := os.Symlink(exe, filepath.Join(proc, "3999999", "exe")); err != nil {
		t.Fatal(err)
	}
	u.procRoot = proc
	u.killFn = func(pid int, sig syscall.Signal) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.calls = append(r.calls, fmt.Sprintf("kill %d %d", pid, sig))
		return r.fail
	}
}

func (r *restartSpy) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// releaseServer serves a GitHub-like API for owner/repo whose latest release
// is tag, with an archive containing files (plus checksums.txt). apiStatus,
// when non-zero, makes the release endpoints fail with that status instead.
type releaseServer struct {
	*httptest.Server
	apiStatus atomic.Int32
	apiHits   atomic.Int32
}

func newReleaseServer(t *testing.T, tag string, files map[string]string) *releaseServer {
	t.Helper()
	archiveName := fmt.Sprintf("pilot-%s-%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	archivePath := filepath.Join(t.TempDir(), archiveName)
	createTestTarGz(t, archivePath, files)
	archive, err := os.ReadFile(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	checksums := fmt.Sprintf("%x  %s\n", sha256.Sum256(archive), archiveName)

	rs := &releaseServer{}
	rs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/owner/repo/releases/latest", "/repos/owner/repo/releases/tags/" + tag:
			rs.apiHits.Add(1)
			if st := rs.apiStatus.Load(); st != 0 {
				w.Header().Set("X-RateLimit-Remaining", "0")
				http.Error(w, `{"message":"API rate limit exceeded for 203.0.113.9."}`, int(st))
				return
			}
			_ = json.NewEncoder(w).Encode(GitHubRelease{
				TagName: tag,
				Assets: []GitHubAsset{
					{Name: archiveName, BrowserDownloadURL: "https://api.github.com/dl/" + archiveName},
					{Name: "checksums.txt", BrowserDownloadURL: "https://api.github.com/dl/checksums.txt"},
				},
			})
		case "/dl/" + archiveName:
			_, _ = w.Write(archive)
		case "/dl/checksums.txt":
			_, _ = w.Write([]byte(checksums))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(rs.Close)
	return rs
}

// newTestUpdater returns an Updater wired to srv with a status file in a
// temp dir and an install dir seeded at version.
func newTestUpdater(t *testing.T, srv *httptest.Server, version string) (*Updater, string) {
	t.Helper()
	installDir := t.TempDir()
	seedInstallDir(t, installDir, version)
	statusPath := filepath.Join(t.TempDir(), StatusFileName)
	u := &Updater{
		config: Config{
			Repo:          "owner/repo",
			InstallDir:    installDir,
			Version:       "v0.3.0-test",
			StatusPath:    statusPath,
			CheckInterval: time.Hour,
		},
		client: newRewriteClient(srv),
		stopCh: make(chan struct{}),
		exitFn: func(int) { t.Error("unexpected exit") },
	}
	return u, statusPath
}

func mustReadStatus(t *testing.T, path string) Status {
	t.Helper()
	s, err := ReadStatus(path)
	if err != nil {
		t.Fatalf("ReadStatus(%s): %v", path, err)
	}
	return s
}

// TestRunOnce_FailureIsReturnedAndRecorded is the core of the "update
// failures are invisible" fix: a GitHub 403 used to be logged and swallowed
// (pilotctl printed status ok, exit 0). RunOnce now returns the error and the
// status file records it, with a failure streak that a success resets.
func TestRunOnce_FailureIsReturnedAndRecorded(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	srv := newReleaseServer(t, "v1.0.0", map[string]string{"daemon": "d"})
	srv.apiStatus.Store(http.StatusForbidden)
	u, statusPath := newTestUpdater(t, srv.Server, "v1.0.0")

	for i := 1; i <= 2; i++ {
		err := u.RunOnce()
		if err == nil {
			t.Fatalf("run %d: RunOnce returned nil for a GitHub 403", i)
		}
		for _, want := range []string{"fetch latest release", "returned 403", "rate limit exceeded", "GITHUB_TOKEN"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("run %d: error %q does not mention %q", i, err, want)
			}
		}
		st := mustReadStatus(t, statusPath)
		if st.LastResult != ResultFailed || st.ConsecutiveFailures != i {
			t.Errorf("run %d: status = %s/%d failures, want failed/%d", i, st.LastResult, st.ConsecutiveFailures, i)
		}
		if st.LastError != err.Error() {
			t.Errorf("run %d: last_error = %q, want %q", i, st.LastError, err)
		}
		if st.LastCheckTrigger != TriggerManual {
			t.Errorf("run %d: trigger = %q, want manual", i, st.LastCheckTrigger)
		}
		if !st.LastSuccessAt.IsZero() {
			t.Errorf("run %d: last_success_at set without a success: %v", i, st.LastSuccessAt)
		}
	}

	// GitHub recovers: the streak resets and the result is up to date.
	srv.apiStatus.Store(0)
	if err := u.RunOnce(); err != nil {
		t.Fatalf("RunOnce after recovery: %v", err)
	}
	st := mustReadStatus(t, statusPath)
	if st.LastResult != ResultUpToDate || st.ConsecutiveFailures != 0 || st.LastError != "" {
		t.Errorf("after recovery: %+v", st)
	}
	if st.LastSuccessAt.IsZero() || st.CurrentVersion != "v1.0.0" || st.LatestVersion != "v1.0.0" {
		t.Errorf("after recovery: success/version fields not set: %+v", st)
	}
	if st.UpdaterVersion != "v0.3.0-test" || st.Repo != "owner/repo" {
		t.Errorf("identity fields = %q/%q", st.UpdaterVersion, st.Repo)
	}
	if got := u.LastStatus(); got.LastResult != ResultUpToDate || !got.LastCheckAt.Equal(st.LastCheckAt) {
		t.Errorf("LastStatus() = %+v, want the persisted record", got)
	}
}

// TestRunOnce_UpdateRecordsStatusAndRestartsDaemon checks the success path:
// the update is installed, the daemon restart is requested and the status
// file says what was installed.
func TestRunOnce_UpdateRecordsStatusAndRestartsDaemon(t *testing.T) {
	t.Parallel()
	srv := newReleaseServer(t, "v2.0.0", map[string]string{"daemon": "new-daemon"})
	u, statusPath := newTestUpdater(t, srv.Server, "v1.0.0")
	spy := &restartSpy{}
	spy.install(t, u)

	if err := u.RunOnce(); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if spy.count() != 1 {
		t.Errorf("daemon restart requests = %d (%v), want 1", spy.count(), spy.calls)
	}
	st := mustReadStatus(t, statusPath)
	if st.LastResult != ResultUpdated || st.CurrentVersion != "v2.0.0" || st.LatestVersion != "v2.0.0" {
		t.Errorf("status = %+v", st)
	}
	if st.LastUpdateVersion != "v2.0.0" || st.LastUpdateAt.IsZero() || st.RestartError != "" {
		t.Errorf("update fields = %q/%v/%q", st.LastUpdateVersion, st.LastUpdateAt, st.RestartError)
	}
}

// TestRunOnce_RestartFailureIsRecorded: the binaries were installed but the
// daemon could not be restarted onto them. RunOnce still succeeds (the
// update is on disk) but the status says the daemon is on the old binary.
func TestRunOnce_RestartFailureIsRecorded(t *testing.T) {
	t.Parallel()
	srv := newReleaseServer(t, "v2.0.0", map[string]string{"daemon": "new-daemon"})
	u, statusPath := newTestUpdater(t, srv.Server, "v1.0.0")
	spy := &restartSpy{fail: errors.New("permission denied")}
	spy.install(t, u)

	if err := u.RunOnce(); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	st := mustReadStatus(t, statusPath)
	if st.LastResult != ResultUpdated {
		t.Errorf("last_result = %q, want updated", st.LastResult)
	}
	if !strings.Contains(st.RestartError, "permission denied") {
		t.Errorf("restart_error = %q, want the restart failure", st.RestartError)
	}
}

// TestRunOnce_DoesNotExitWhenUpdaterReplaced: RunOnce is called in-process
// by `pilotctl update`. When the release replaced pilot-updater it used to
// os.Exit(0) the pilotctl process before it printed anything and without
// restarting the daemon. It must return normally and restart the daemon.
func TestRunOnce_DoesNotExitWhenUpdaterReplaced(t *testing.T) {
	t.Parallel()
	srv := newReleaseServer(t, "v2.0.0", map[string]string{
		"daemon":  "new-daemon",
		"updater": "new-updater",
	})
	u, statusPath := newTestUpdater(t, srv.Server, "v1.0.0")
	spy := &restartSpy{}
	spy.install(t, u)
	var exited atomic.Bool
	u.exitFn = func(int) { exited.Store(true) }

	if err := u.RunOnce(); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if exited.Load() {
		t.Fatal("RunOnce exited the calling process")
	}
	if spy.count() != 1 {
		t.Errorf("daemon restart requests = %d, want 1", spy.count())
	}
	if got, _ := os.ReadFile(filepath.Join(u.config.InstallDir, "pilot-updater")); string(got) != "new-updater" {
		t.Errorf("pilot-updater = %q, want new-updater", got)
	}
	if st := mustReadStatus(t, statusPath); st.LastResult != ResultUpdated {
		t.Errorf("last_result = %q, want updated", st.LastResult)
	}
}

// TestLoop_RecordsUpdateBeforeSelfExit: the loop still exits after replacing
// its own binary (so the service manager restarts it), but only after the
// update has been recorded, and it leaves the daemon restart to the new
// process (recoverPendingRestart).
func TestLoop_RecordsUpdateBeforeSelfExit(t *testing.T) {
	t.Parallel()
	srv := newReleaseServer(t, "v2.0.0", map[string]string{
		"daemon":  "new-daemon",
		"updater": "new-updater",
	})
	u, statusPath := newTestUpdater(t, srv.Server, "v1.0.0")
	spy := &restartSpy{}
	spy.install(t, u)
	var atExit Status
	var exits atomic.Int32
	u.exitFn = func(code int) {
		exits.Add(1)
		if code != 0 {
			t.Errorf("exit code = %d, want 0", code)
		}
		atExit = mustReadStatus(t, statusPath)
	}

	if err := u.checkOnce(); err != nil {
		t.Fatalf("checkOnce: %v", err)
	}
	if exits.Load() != 1 {
		t.Fatalf("exit calls = %d, want 1", exits.Load())
	}
	if atExit.LastResult != ResultUpdated || atExit.LastCheckTrigger != TriggerAuto || atExit.CurrentVersion != "v2.0.0" {
		t.Errorf("status at exit = %+v, want updated/auto/v2.0.0", atExit)
	}
	if spy.count() != 0 {
		t.Errorf("stale updater restarted the daemon (%v); the new process must do it", spy.calls)
	}
}

// TestRecoverPendingRestart_RecordsRestartResult: the restart made by the
// new updater process after a self-update is reflected in restart_error.
func TestRecoverPendingRestart_RecordsRestartResult(t *testing.T) {
	t.Parallel()
	installDir := t.TempDir()
	statusPath := filepath.Join(t.TempDir(), StatusFileName)
	if err := os.WriteFile(filepath.Join(installDir, ".pilot-version"), []byte("v2.0.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Daemon binary newer than the (absent) restart record → restart due.
	if err := os.WriteFile(filepath.Join(installDir, "pilot-daemon"), []byte("d"), 0o755); err != nil {
		t.Fatal(err)
	}
	u := &Updater{config: Config{InstallDir: installDir, StatusPath: statusPath}}
	spy := &restartSpy{fail: errors.New("no such service")}
	spy.install(t, u)

	u.recoverPendingRestart()
	if st := mustReadStatus(t, statusPath); !strings.Contains(st.RestartError, "no such service") {
		t.Fatalf("restart_error = %q, want recorded failure", st.RestartError)
	}

	// Next binary update, restart succeeds → error cleared.
	spy.fail = nil
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(installDir, "pilot-daemon"), future, future); err != nil {
		t.Fatal(err)
	}
	u.recoverPendingRestart()
	if st := mustReadStatus(t, statusPath); st.RestartError != "" {
		t.Errorf("restart_error = %q, want cleared after a successful restart", st.RestartError)
	}
}

// TestRunOnce_InvalidPinReturnsError: a bad --pin used to be logged only.
func TestRunOnce_InvalidPinReturnsError(t *testing.T) {
	t.Parallel()
	srv := newReleaseServer(t, "v1.0.0", map[string]string{"daemon": "d"})
	u, statusPath := newTestUpdater(t, srv.Server, "v1.0.0")
	u.config.PinnedVersion = "not-a-version"

	err := u.RunOnce()
	if err == nil || !strings.Contains(err.Error(), "invalid pinned version") {
		t.Fatalf("RunOnce = %v, want invalid pinned version error", err)
	}
	st := mustReadStatus(t, statusPath)
	if st.LastResult != ResultFailed || st.PinnedVersion != "not-a-version" {
		t.Errorf("status = %+v", st)
	}
}

// TestRunOnce_PinnedAlreadyInstalledIsUpToDate covers the pinned no-op path.
func TestRunOnce_PinnedAlreadyInstalledIsUpToDate(t *testing.T) {
	t.Parallel()
	srv := newReleaseServer(t, "v1.5.0", map[string]string{"daemon": "d"})
	u, statusPath := newTestUpdater(t, srv.Server, "v1.5.0")
	u.config.PinnedVersion = "v1.5.0"

	if err := u.RunOnce(); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if srv.apiHits.Load() != 0 {
		t.Errorf("pinned+installed made %d API calls, want 0", srv.apiHits.Load())
	}
	st := mustReadStatus(t, statusPath)
	if st.LastResult != ResultUpToDate || st.LatestVersion != "v1.5.0" || st.PinnedVersion != "v1.5.0" {
		t.Errorf("status = %+v", st)
	}
}

// TestRunOnce_ApplyFailureIsReturned: a release without an asset for this
// platform is a failed check, not a silent no-op.
func TestRunOnce_ApplyFailureIsReturned(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(GitHubRelease{TagName: "v2.0.0"})
	}))
	defer srv.Close()
	u, statusPath := newTestUpdater(t, srv, "v1.0.0")

	err := u.RunOnce()
	if err == nil || !strings.Contains(err.Error(), "apply update v2.0.0") {
		t.Fatalf("RunOnce = %v, want apply error", err)
	}
	st := mustReadStatus(t, statusPath)
	if st.LastResult != ResultFailed || st.LatestVersion != "v2.0.0" || st.CurrentVersion != "v1.0.0" {
		t.Errorf("status = %+v", st)
	}
}

// TestStatus_MergesWithFileOnDisk: a manual run must not wipe what the loop
// (another process) recorded, and must continue its failure streak.
func TestStatus_MergesWithFileOnDisk(t *testing.T) {
	t.Parallel()
	srv := newReleaseServer(t, "v1.0.0", map[string]string{"daemon": "d"})
	srv.apiStatus.Store(http.StatusBadGateway)
	u, statusPath := newTestUpdater(t, srv.Server, "v1.0.0")

	next := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	started := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	if err := writeStatusFile(statusPath, Status{
		LoopPID:             4242,
		LoopStartedAt:       started,
		NextCheckAt:         next,
		ConsecutiveFailures: 3,
		LastResult:          ResultFailed,
	}); err != nil {
		t.Fatal(err)
	}

	if err := u.RunOnce(); err == nil {
		t.Fatal("RunOnce returned nil for a 502")
	}
	st := mustReadStatus(t, statusPath)
	if st.LoopPID != 4242 || !st.LoopStartedAt.Equal(started) || !st.NextCheckAt.Equal(next) {
		t.Errorf("loop fields not preserved: %+v", st)
	}
	if st.ConsecutiveFailures != 4 {
		t.Errorf("consecutive_failures = %d, want 4", st.ConsecutiveFailures)
	}
}

// TestStatusPath_Resolution pins where the status file goes.
func TestStatusPath_Resolution(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if got := New(Config{StatePath: filepath.Join(dir, "auto-update.json")}).statusPath(); got != filepath.Join(dir, "update-state.json") {
		t.Errorf("default status path = %q, want next to the control file", got)
	}
	if got := New(Config{StatePath: filepath.Join(dir, "a.json"), StatusPath: "/x/s.json"}).statusPath(); got != "/x/s.json" {
		t.Errorf("explicit StatusPath = %q", got)
	}
	if got := New(Config{}).statusPath(); got != "" {
		t.Errorf("no paths configured → %q, want none", got)
	}
}

// TestStatus_WriteFailureDoesNotFailCheck: an unwritable status path is
// logged, never turned into a failed update.
func TestStatus_WriteFailureDoesNotFailCheck(t *testing.T) {
	t.Parallel()
	srv := newReleaseServer(t, "v1.0.0", map[string]string{"daemon": "d"})
	u, _ := newTestUpdater(t, srv.Server, "v1.0.0")
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	u.config.StatusPath = filepath.Join(blocker, "sub", StatusFileName) // parent is a file

	if err := u.RunOnce(); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if u.LastStatus().LastResult != ResultUpToDate {
		t.Errorf("in-memory status = %+v", u.LastStatus())
	}
}

// TestReadStatus_Missing lets callers distinguish "never checked".
func TestReadStatus_Missing(t *testing.T) {
	t.Parallel()
	_, err := ReadStatus(filepath.Join(t.TempDir(), "nope.json"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("ReadStatus(missing) = %v, want fs.ErrNotExist", err)
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(bad, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadStatus(bad); err == nil {
		t.Fatal("ReadStatus(corrupt) = nil error")
	}
}

// TestStatus_JSONShape pins the field names pilotctl reads.
func TestStatus_JSONShape(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	data, err := json.Marshal(Status{
		LastCheckAt: now, LastResult: ResultFailed, LastError: "boom",
		ConsecutiveFailures: 2, CurrentVersion: "v1.0.0", LatestVersion: "v1.1.0",
		LoopPID: 7, NextCheckAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		`"last_check_at":"2026-09-24T12:00:00Z"`, `"last_result":"failed"`, `"last_error":"boom"`,
		`"consecutive_failures":2`, `"current_version":"v1.0.0"`, `"latest_version":"v1.1.0"`,
		`"loop_pid":7`, `"next_check_at":"2026-09-24T12:00:00Z"`,
	} {
		if !strings.Contains(string(data), key) {
			t.Errorf("JSON %s missing %s", data, key)
		}
	}
	// Zero times are omitted rather than written as year 1.
	if strings.Contains(string(data), "0001-01-01") || strings.Contains(string(data), "last_success_at") {
		t.Errorf("zero time serialized: %s", data)
	}
}

// TestCheckLoop_RecordsLoopHeartbeat: the loop records its pid and next
// wake-up even while auto-update is disabled, so `update status` can tell a
// running-but-disabled updater from a missing one.
func TestCheckLoop_RecordsLoopHeartbeat(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	installDir := t.TempDir()
	seedInstallDir(t, installDir, "v1.0.0")
	u := &Updater{
		config: Config{
			Repo:          "owner/repo",
			InstallDir:    installDir,
			CheckInterval: time.Hour,
			StatePath:     filepath.Join(dir, "auto-update.json"), // absent → disabled
		},
		client: http.DefaultClient,
		stopCh: make(chan struct{}),
		exitFn: func(int) { t.Error("unexpected exit") },
	}
	before := time.Now()
	u.Start()
	defer u.Stop()

	statusPath := filepath.Join(dir, StatusFileName)
	deadline := time.Now().Add(5 * time.Second)
	for {
		st, err := ReadStatus(statusPath)
		if err == nil && !st.NextCheckAt.IsZero() {
			if st.LoopPID != os.Getpid() || st.LoopStartedAt.Before(before.Add(-time.Second)) {
				t.Errorf("loop fields = pid %d started %v", st.LoopPID, st.LoopStartedAt)
			}
			if d := st.NextCheckAt.Sub(before); d < 59*time.Minute || d > 61*time.Minute {
				t.Errorf("next_check_at %v is not ~1h after start", st.NextCheckAt)
			}
			if st.LastResult != "" {
				t.Errorf("disabled loop recorded a check: %+v", st)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("loop never recorded its heartbeat (err=%v)", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
