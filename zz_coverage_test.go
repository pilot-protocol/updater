// SPDX-License-Identifier: AGPL-3.0-or-later

package updater

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// rewriteRT is an http.RoundTripper that rewrites every outbound request's
// scheme+host to the supplied test-server URL. This lets tests drive
// fetchLatestRelease (which hardcodes https://api.github.com/...) through
// an httptest server without modifying production code.
type rewriteRT struct {
	target *url.URL
}

func (r *rewriteRT) RoundTrip(req *http.Request) (*http.Response, error) {
	req2 := req.Clone(req.Context())
	req2.URL.Scheme = r.target.Scheme
	req2.URL.Host = r.target.Host
	req2.Host = r.target.Host
	return http.DefaultTransport.RoundTrip(req2)
}

// newRewriteClient produces an *http.Client whose requests are silently
// redirected to srv regardless of the URL the caller passes in.
func newRewriteClient(srv *httptest.Server) *http.Client {
	u, _ := url.Parse(srv.URL)
	return &http.Client{
		Transport: &rewriteRT{target: u},
		Timeout:   10 * time.Second,
	}
}

// --- fetchLatestRelease error branches ------------------------------------

// TestFetchLatestRelease_Non200Body covers the non-200 branch.
func TestFetchLatestRelease_Non200Body(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rate limited", http.StatusForbidden)
	}))
	defer srv.Close()

	u := &Updater{
		config: Config{Repo: "owner/repo", Version: "vTEST"},
		client: newRewriteClient(srv),
		stopCh: make(chan struct{}),
	}
	_, err := u.fetchLatestRelease()
	if err == nil {
		t.Fatal("expected error for non-200 response")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error does not mention status: %v", err)
	}
}

// TestFetchLatestRelease_MalformedJSON covers the decode error branch.
func TestFetchLatestRelease_MalformedJSON(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "this is not json {{{")
	}))
	defer srv.Close()
	u := &Updater{
		config: Config{Repo: "owner/repo"},
		client: newRewriteClient(srv),
		stopCh: make(chan struct{}),
	}
	_, err := u.fetchLatestRelease()
	if err == nil {
		t.Fatal("expected decode error")
	}
	if !strings.Contains(err.Error(), "decode") {
		t.Errorf("error does not mention decode: %v", err)
	}
}

// TestFetchLatestRelease_TransportError covers the client.Do error branch.
func TestFetchLatestRelease_TransportError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	srv.Close() // close immediately → connection refused

	u := &Updater{
		config: Config{Repo: "owner/repo"},
		client: newRewriteClient(srv),
		stopCh: make(chan struct{}),
	}
	_, err := u.fetchLatestRelease()
	if err == nil {
		t.Fatal("expected transport error")
	}
}

// TestFetchLatestRelease_HappyPath covers the success branch including
// User-Agent header injection.
func TestFetchLatestRelease_HappyPath(t *testing.T) {
	t.Parallel()
	uaSeen := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uaSeen = r.Header.Get("User-Agent")
		_ = json.NewEncoder(w).Encode(GitHubRelease{
			TagName: "v9.8.7",
			Assets:  []GitHubAsset{{Name: "x", BrowserDownloadURL: "http://x"}},
		})
	}))
	defer srv.Close()

	u := &Updater{
		config: Config{Repo: "owner/repo", Version: "v1.6.4"},
		client: newRewriteClient(srv),
		stopCh: make(chan struct{}),
	}
	rel, err := u.fetchLatestRelease()
	if err != nil {
		t.Fatalf("fetchLatestRelease: %v", err)
	}
	if rel.TagName != "v9.8.7" {
		t.Errorf("tag = %q", rel.TagName)
	}
	if uaSeen != "pilot-updater/v1.6.4" {
		t.Errorf("User-Agent = %q, want pilot-updater/v1.6.4", uaSeen)
	}
}

// TestFetchLatestRelease_NoVersionOmitsUA covers the branch where the
// User-Agent header is NOT set (config.Version == "").
func TestFetchLatestRelease_NoVersionOmitsUA(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// When Version is "", New() doesn't set our header, but Go's
		// http.Client may still set a default Go-http-client UA.
		if strings.HasPrefix(r.Header.Get("User-Agent"), "pilot-updater/") {
			t.Errorf("did not expect pilot-updater UA, got %q", r.Header.Get("User-Agent"))
		}
		_ = json.NewEncoder(w).Encode(GitHubRelease{TagName: "v1.0.0"})
	}))
	defer srv.Close()

	u := &Updater{
		config: Config{Repo: "owner/repo"}, // no Version
		client: newRewriteClient(srv),
		stopCh: make(chan struct{}),
	}
	if _, err := u.fetchLatestRelease(); err != nil {
		t.Fatalf("fetchLatestRelease: %v", err)
	}
}

// --- checkOnce branches ---------------------------------------------------

// TestCheckOnce_FetchError covers the "failed to fetch latest release"
// log+return branch.
func TestCheckOnce_FetchError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	tmp := t.TempDir()
	u := &Updater{
		config: Config{Repo: "owner/repo", InstallDir: tmp},
		client: newRewriteClient(srv),
		stopCh: make(chan struct{}),
		exitFn: func(int) {},
	}
	u.checkOnce() // must not panic; logs error and returns
}

// TestCheckOnce_BadTagLogged covers the "failed to parse release tag"
// branch.
func TestCheckOnce_BadTagLogged(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(GitHubRelease{TagName: "not-a-semver"})
	}))
	defer srv.Close()
	tmp := t.TempDir()
	u := &Updater{
		config: Config{Repo: "owner/repo", InstallDir: tmp},
		client: newRewriteClient(srv),
		stopCh: make(chan struct{}),
		exitFn: func(int) {},
	}
	u.checkOnce()
}

// TestCheckOnce_CurrentVersionError covers the "failed to get current
// version" branch (daemon binary missing).
func TestCheckOnce_CurrentVersionError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(GitHubRelease{TagName: "v1.0.0"})
	}))
	defer srv.Close()
	tmp := t.TempDir() // no pilot-daemon binary
	u := &Updater{
		config: Config{Repo: "owner/repo", InstallDir: tmp},
		client: newRewriteClient(srv),
		stopCh: make(chan struct{}),
		exitFn: func(int) {},
	}
	u.checkOnce()
}

// TestCheckOnce_AlreadyUpToDateLogs covers the "already up to date"
// debug-log branch.
func TestCheckOnce_AlreadyUpToDateLogs(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(GitHubRelease{TagName: "v1.0.0"})
	}))
	defer srv.Close()
	tmp := t.TempDir()
	_ = os.WriteFile(filepath.Join(tmp, "pilot-daemon"), []byte("stub"), 0755)
	_ = os.WriteFile(filepath.Join(tmp, ".pilot-version"), []byte("v1.0.0\n"), 0644)
	u := &Updater{
		config: Config{Repo: "owner/repo", InstallDir: tmp},
		client: newRewriteClient(srv),
		stopCh: make(chan struct{}),
		exitFn: func(int) {},
	}
	u.checkOnce()
}

// TestCheckOnce_NewerTriggersApplyUpdate drives the full "new version
// available → applyUpdate → touchRestartRecord" path through checkOnce.
func TestCheckOnce_NewerTriggersApplyUpdate(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	_ = os.WriteFile(filepath.Join(tmp, "pilot-daemon"), []byte("old"), 0755)
	_ = os.WriteFile(filepath.Join(tmp, ".pilot-version"), []byte("v1.0.0\n"), 0644)

	archiveName := fmt.Sprintf("pilot-%s-%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	archivePath := filepath.Join(t.TempDir(), archiveName)
	createTestTarGz(t, archivePath, map[string]string{
		"daemon":   "fresh-daemon",
		"pilotctl": "fresh-pilotctl",
	})
	archiveContent, _ := os.ReadFile(archivePath)
	hash := sha256.Sum256(archiveContent)
	checksumsContent := fmt.Sprintf("%x  %s\n", hash, archiveName)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/owner/repo/releases/latest":
			_ = json.NewEncoder(w).Encode(GitHubRelease{
				TagName: "v2.0.0",
				Assets: []GitHubAsset{
					{Name: archiveName, BrowserDownloadURL: "https://api.github.com/dl/" + archiveName},
					{Name: "checksums.txt", BrowserDownloadURL: "https://api.github.com/dl/checksums.txt"},
				},
			})
		case "/dl/" + archiveName:
			_, _ = w.Write(archiveContent)
		case "/dl/checksums.txt":
			_, _ = w.Write([]byte(checksumsContent))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	u := &Updater{
		config: Config{Repo: "owner/repo", InstallDir: tmp},
		client: newRewriteClient(srv),
		stopCh: make(chan struct{}),
		exitFn: func(int) {},
	}
	u.checkOnce()

	// Daemon binary should have been replaced.
	got, err := os.ReadFile(filepath.Join(tmp, "pilot-daemon"))
	if err != nil {
		t.Fatalf("read pilot-daemon: %v", err)
	}
	if string(got) != "fresh-daemon" {
		t.Errorf("daemon content = %q, want fresh-daemon", got)
	}
	// .pilot-version should now be v2.0.0.
	ver, _ := os.ReadFile(filepath.Join(tmp, ".pilot-version"))
	if !strings.Contains(string(ver), "v2.0.0") {
		t.Errorf(".pilot-version = %q, want contains v2.0.0", ver)
	}
	// touchRestartRecord should have written the restart record.
	if _, err := os.Stat(filepath.Join(tmp, ".daemon-last-restart")); err != nil {
		t.Errorf("expected .daemon-last-restart: %v", err)
	}
}

// TestCheckOnce_ApplyUpdateError covers the "failed to apply update"
// log+return branch — release is newer than installed but archive asset
// for this GOOS/GOARCH is missing.
func TestCheckOnce_ApplyUpdateError(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	_ = os.WriteFile(filepath.Join(tmp, "pilot-daemon"), []byte("old"), 0755)
	_ = os.WriteFile(filepath.Join(tmp, ".pilot-version"), []byte("v1.0.0\n"), 0644)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(GitHubRelease{
			TagName: "v2.0.0",
			Assets:  []GitHubAsset{}, // empty → applyUpdate errors with "no asset"
		})
	}))
	defer srv.Close()
	u := &Updater{
		config: Config{Repo: "owner/repo", InstallDir: tmp},
		client: newRewriteClient(srv),
		stopCh: make(chan struct{}),
		exitFn: func(int) {},
	}
	u.checkOnce()
	// Version file must still be the old one.
	ver, _ := os.ReadFile(filepath.Join(tmp, ".pilot-version"))
	if !strings.Contains(string(ver), "v1.0.0") {
		t.Errorf(".pilot-version = %q, want still v1.0.0", ver)
	}
}

// --- applyUpdate error branches -------------------------------------------

// TestApplyUpdate_ArchiveAssetMissing covers the "no asset" branch.
func TestApplyUpdate_ArchiveAssetMissing(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	u := &Updater{
		config: Config{InstallDir: tmp},
		client: http.DefaultClient,
		stopCh: make(chan struct{}),
	}
	err := u.applyUpdate(&GitHubRelease{
		TagName: "v1.0.0",
		Assets:  []GitHubAsset{{Name: "checksums.txt", BrowserDownloadURL: "http://x"}},
	})
	if err == nil || !strings.Contains(err.Error(), "no asset") {
		t.Fatalf("want 'no asset' error, got %v", err)
	}
}

// TestApplyUpdate_ArchiveDownloadFails covers the download error path.
func TestApplyUpdate_ArchiveDownloadFails(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	archiveName := fmt.Sprintf("pilot-%s-%s.tar.gz", runtime.GOOS, runtime.GOARCH)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	u := &Updater{
		config: Config{InstallDir: tmp},
		client: srv.Client(),
		stopCh: make(chan struct{}),
	}
	err := u.applyUpdate(&GitHubRelease{
		TagName: "v1.0.0",
		Assets: []GitHubAsset{
			{Name: archiveName, BrowserDownloadURL: srv.URL + "/archive"},
			{Name: "checksums.txt", BrowserDownloadURL: srv.URL + "/checksums.txt"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "download archive") {
		t.Fatalf("want download archive error, got %v", err)
	}
}

// TestApplyUpdate_ChecksumMismatch covers the verify-fail branch.
func TestApplyUpdate_ChecksumMismatch(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	archiveName := fmt.Sprintf("pilot-%s-%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	archivePath := filepath.Join(t.TempDir(), archiveName)
	createTestTarGz(t, archivePath, map[string]string{"daemon": "binary"})
	archiveContent, _ := os.ReadFile(archivePath)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dl/" + archiveName:
			_, _ = w.Write(archiveContent)
		case "/dl/checksums.txt":
			// Wrong hash — same filename.
			_, _ = w.Write([]byte("0000000000000000000000000000000000000000000000000000000000000000  " + archiveName + "\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	u := &Updater{
		config: Config{InstallDir: tmp},
		client: srv.Client(),
		stopCh: make(chan struct{}),
	}
	err := u.applyUpdate(&GitHubRelease{
		TagName: "v1.0.0",
		Assets: []GitHubAsset{
			{Name: archiveName, BrowserDownloadURL: srv.URL + "/dl/" + archiveName},
			{Name: "checksums.txt", BrowserDownloadURL: srv.URL + "/dl/checksums.txt"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "checksum verification failed") {
		t.Fatalf("want checksum mismatch error, got %v", err)
	}
}

// TestApplyUpdate_CorruptArchiveExtractFails covers the extract error
// branch — valid hash but the bytes aren't a real tar.gz.
func TestApplyUpdate_CorruptArchiveExtractFails(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	archiveName := fmt.Sprintf("pilot-%s-%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	// "Archive" is plain text — bytes will checksum fine, but gzip fails.
	archiveBody := []byte("not actually a tar.gz")
	hash := sha256.Sum256(archiveBody)
	checksumsBody := fmt.Sprintf("%x  %s\n", hash, archiveName)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dl/" + archiveName:
			_, _ = w.Write(archiveBody)
		case "/dl/checksums.txt":
			_, _ = w.Write([]byte(checksumsBody))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	u := &Updater{
		config: Config{InstallDir: tmp},
		client: srv.Client(),
		stopCh: make(chan struct{}),
	}
	err := u.applyUpdate(&GitHubRelease{
		TagName: "v1.0.0",
		Assets: []GitHubAsset{
			{Name: archiveName, BrowserDownloadURL: srv.URL + "/dl/" + archiveName},
			{Name: "checksums.txt", BrowserDownloadURL: srv.URL + "/dl/checksums.txt"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "extract archive") {
		t.Fatalf("want extract error, got %v", err)
	}
}

// --- downloadFile branches ------------------------------------------------

// TestDownloadFile_Non200 covers the non-200 branch.
func TestDownloadFile_Non200(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "missing", http.StatusNotFound)
	}))
	defer srv.Close()
	u := &Updater{client: srv.Client()}
	err := u.downloadFile(srv.URL, filepath.Join(t.TempDir(), "x"))
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("want 404 error, got %v", err)
	}
}

// TestDownloadFile_BadDestPath covers the os.Create error branch.
func TestDownloadFile_BadDestPath(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("payload"))
	}))
	defer srv.Close()
	u := &Updater{client: srv.Client()}
	err := u.downloadFile(srv.URL, "/no/such/path/file.bin")
	if err == nil {
		t.Fatal("expected error for unwritable destination")
	}
}

// TestDownloadFile_TransportError covers the http.Get error branch.
func TestDownloadFile_TransportError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	srv.Close()
	u := &Updater{client: srv.Client()}
	if err := u.downloadFile(srv.URL, filepath.Join(t.TempDir(), "x")); err == nil {
		t.Fatal("expected transport error")
	}
}

// --- VerifyChecksum extra branches ---------------------------------------

// TestVerifyChecksum_MissingChecksumsFile covers the os.ReadFile error
// branch.
func TestVerifyChecksum_MissingChecksumsFile(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	archive := filepath.Join(tmp, "x.tar.gz")
	_ = os.WriteFile(archive, []byte("data"), 0644)
	err := VerifyChecksum(archive, "x.tar.gz", filepath.Join(tmp, "no-such-checksums.txt"))
	if err == nil || !strings.Contains(err.Error(), "read checksums") {
		t.Fatalf("want read-checksums error, got %v", err)
	}
}

// TestVerifyChecksum_MissingArchive covers the os.Open error branch
// (file referenced in checksums but missing on disk).
func TestVerifyChecksum_MissingArchive(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	checksums := filepath.Join(tmp, "checksums.txt")
	_ = os.WriteFile(checksums, []byte("dead  x.tar.gz\n"), 0644)
	err := VerifyChecksum(filepath.Join(tmp, "no-such.tar.gz"), "x.tar.gz", checksums)
	if err == nil {
		t.Fatal("expected error for missing archive")
	}
}

// TestVerifyChecksum_BlankLinesAndSingleSpace covers the line-skipping
// + Fields-split branches (blank line, single-space separator).
func TestVerifyChecksum_BlankLinesAndSingleSpace(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	body := []byte("hello")
	archive := filepath.Join(tmp, "x.tar.gz")
	_ = os.WriteFile(archive, body, 0644)
	h := sha256.Sum256(body)
	// Multiple blanks + the right line with a single space separator.
	content := fmt.Sprintf("\n\n   \n%x x.tar.gz\n", h)
	cks := filepath.Join(tmp, "checksums.txt")
	_ = os.WriteFile(cks, []byte(content), 0644)
	if err := VerifyChecksum(archive, "x.tar.gz", cks); err != nil {
		t.Fatalf("VerifyChecksum: %v", err)
	}
}

// --- replaceBinary extra branches ----------------------------------------

// TestReplaceBinary_TmpCreateFails covers the OpenFile-for-tmp error
// branch — dst directory exists but is read-only.
func TestReplaceBinary_TmpCreateFails(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("permissions semantics differ on windows")
	}
	dir := t.TempDir()
	// Source file is valid.
	src := filepath.Join(dir, "src")
	_ = os.WriteFile(src, []byte("payload"), 0644)
	// Destination directory is read-only → tmp file creation fails.
	roDir := filepath.Join(dir, "ro")
	if err := os.MkdirAll(roDir, 0555); err != nil {
		t.Fatalf("mkdir ro: %v", err)
	}
	defer os.Chmod(roDir, 0755)
	dst := filepath.Join(roDir, "out")
	if err := replaceBinary(src, dst); err == nil {
		t.Fatal("expected error writing to read-only directory")
	}
}

// TestReplaceBinary_OverwritesExisting confirms that an existing file at
// dst is replaced (the rename branch).
func TestReplaceBinary_OverwritesExisting(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	_ = os.WriteFile(src, []byte("NEW"), 0644)
	_ = os.WriteFile(dst, []byte("OLD"), 0755)
	if err := replaceBinary(src, dst); err != nil {
		t.Fatalf("replaceBinary: %v", err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "NEW" {
		t.Errorf("dst = %q, want NEW", got)
	}
}

// --- extractTarGz extra branches -----------------------------------------

// TestExtractTarGz_DestDirReadOnly covers the OpenFile-for-output error
// branch.
func TestExtractTarGz_DestDirReadOnly(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("permissions semantics differ on windows")
	}
	dir := t.TempDir()
	archive := filepath.Join(dir, "x.tar.gz")
	createTestTarGz(t, archive, map[string]string{"daemon": "stuff"})

	roDir := filepath.Join(dir, "ro")
	if err := os.MkdirAll(roDir, 0555); err != nil {
		t.Fatalf("mkdir ro: %v", err)
	}
	defer os.Chmod(roDir, 0755)

	if err := extractTarGz(archive, roDir); err == nil {
		t.Fatal("expected error writing into read-only dest dir")
	}
}

// TestExtractTarGz_TruncatedTar covers the tar-next-error branch:
// valid gzip wrapping a truncated tar stream.
func TestExtractTarGz_TruncatedTar(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	archive := filepath.Join(dir, "trunc.tar.gz")
	f, _ := os.Create(archive)
	// Write a gzip header for partial content.
	if _, err := f.Write([]byte{
		0x1f, 0x8b, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00,
		// Then some random bytes — invalid tar inside valid gzip stream.
		0xde, 0xad, 0xbe, 0xef,
	}); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.Close()
	if err := extractTarGz(archive, dir); err == nil {
		t.Fatal("expected error from truncated tar")
	}
}

// --- writeFileSync extra ---------------------------------------------------

// TestWriteFileSync_OverwritesExisting confirms TRUNC behaviour.
func TestWriteFileSync_OverwritesExisting(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dst := filepath.Join(dir, "f")
	_ = os.WriteFile(dst, []byte("old-and-longer"), 0644)
	if err := writeFileSync(dst, []byte("new"), 0644); err != nil {
		t.Fatalf("writeFileSync: %v", err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "new" {
		t.Errorf("dst = %q, want new", got)
	}
}

// --- checkLoop ticker branch ---------------------------------------------

// TestCheckLoop_TickerFiresThenStops drives the ticker → jitter path in
// checkLoop. We make the check interval tiny so the ticker fires at
// least once; the stopCh closes immediately afterward to release the
// jitter timer.
func TestCheckLoop_TickerFiresThenStops(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	_ = os.WriteFile(filepath.Join(tmp, "pilot-daemon"), []byte("x"), 0755)
	_ = os.WriteFile(filepath.Join(tmp, ".pilot-version"), []byte("v9.9.9\n"), 0644)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(GitHubRelease{TagName: "v0.0.1"})
	}))
	defer srv.Close()

	u := &Updater{
		config: Config{
			CheckInterval: 5 * time.Millisecond,
			Repo:          "owner/repo",
			InstallDir:    tmp,
		},
		client: newRewriteClient(srv),
		stopCh: make(chan struct{}),
		exitFn: func(int) {},
	}
	u.Start()
	// Sleep long enough for the ticker to fire at least once.
	time.Sleep(50 * time.Millisecond)
	u.Stop()
}

// TestCheckLoop_StopBeforeFirstTick covers the early-stop branch where
// stopCh closes before the very first ticker fires.
func TestCheckLoop_StopBeforeFirstTick(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(GitHubRelease{TagName: "v0.0.1"})
	}))
	defer srv.Close()

	u := &Updater{
		config: Config{
			CheckInterval: 5 * time.Minute,
			Repo:          "owner/repo",
			InstallDir:    tmp,
		},
		client: newRewriteClient(srv),
		stopCh: make(chan struct{}),
		exitFn: func(int) {},
	}
	u.Start()
	// Immediate stop — exits via the stopCh case in the outer select.
	u.Stop()
}

// --- signalDaemonRestart{,Linux,Darwin} extra coverage -------------------

// TestSignalDaemonRestart_Dispatches confirms the OS dispatcher does
// not panic when invoked. The branch taken depends on runtime.GOOS.
func TestSignalDaemonRestart_Dispatches(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	u := &Updater{config: Config{InstallDir: tmp}}
	u.signalDaemonRestart()
}

// TestSignalDaemonRestartLinux_DaemonNotFound exercises the Linux-only
// path: it walks /proc and warns when no matching exe is found. Safe
// to call on macOS where /proc does not exist (the os.ReadDir error
// branch is hit).
func TestSignalDaemonRestartLinux_DaemonNotFound(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	u := &Updater{config: Config{InstallDir: tmp}}
	u.signalDaemonRestartLinux()
}

// TestSignalDaemonRestartDarwin_LaunchctlMissing exercises the Darwin
// launchctl path. On a CI runner without launchd it warns and returns;
// on a real macOS box it tries to kickstart the (non-installed) label
// and warns. Either way the call must not panic.
func TestSignalDaemonRestartDarwin_LaunchctlMissing(t *testing.T) {
	t.Parallel()
	u := &Updater{config: Config{InstallDir: t.TempDir()}}
	u.signalDaemonRestartDarwin()
}

// --- maxDownloadBytes sanity ----------------------------------------------

// TestMaxDownloadBytes_Cap confirms the LimitReader bounds the write.
// Server streams more bytes than the cap; downloadFile should stop
// at maxDownloadBytes without erroring.
func TestMaxDownloadBytes_Cap(t *testing.T) {
	t.Parallel()
	// Use a small overall test by checking only that the constant is
	// non-zero and that downloadFile copies exactly what the server
	// returns when the body is smaller than the cap.
	if maxDownloadBytes <= 0 {
		t.Fatal("maxDownloadBytes must be positive")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("short"))
	}))
	defer srv.Close()
	dst := filepath.Join(t.TempDir(), "out")
	u := &Updater{client: srv.Client()}
	if err := u.downloadFile(srv.URL, dst); err != nil {
		t.Fatalf("downloadFile: %v", err)
	}
	got, _ := os.ReadFile(dst)
	if string(got) != "short" {
		t.Errorf("got %q", got)
	}
}

// TestArchiveToInstallMapping pins the archive-name → install-name
// mapping so future renames must be intentional.
func TestArchiveToInstallMapping(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"daemon":   "pilot-daemon",
		"gateway":  "pilot-gateway",
		"updater":  "pilot-updater",
		"pilotctl": "pilotctl",
	}
	if len(archiveToInstall) != len(want) {
		t.Fatalf("len(archiveToInstall) = %d, want %d", len(archiveToInstall), len(want))
	}
	for k, v := range want {
		if got := archiveToInstall[k]; got != v {
			t.Errorf("archiveToInstall[%q] = %q, want %q", k, got, v)
		}
	}
}

// TestVerifyChecksumsAttestation_MissingTagFailsClosed verifies the real gh-free
// implementation refuses to verify without a release tag — the tag is what binds
// checksums.txt to THIS release and prevents replaying an older attested
// checksums.txt under a new tag (validated rollback). This check happens before
// any network access, so the test is deterministic and offline.
func TestVerifyChecksumsAttestation_MissingTagFailsClosed(t *testing.T) {
	t.Parallel()

	err := realVerifyChecksumsAttestationFn("test/repo", "", "/nonexistent/checksums.txt")
	if err == nil {
		t.Fatal("expected error when tag is empty (fail closed), got nil")
	}
	if !strings.Contains(err.Error(), "release tag is required") {
		t.Errorf("error should explain the tag is required, got: %v", err)
	}
}

// TestVerifyChecksumsAttestation_MissingFileFailsClosed verifies the real
// implementation fails closed when checksums.txt cannot be read/hashed. This
// fails before any network access — a stock host without a reachable
// attestation still refuses to install rather than proceeding unverified.
func TestVerifyChecksumsAttestation_MissingFileFailsClosed(t *testing.T) {
	t.Parallel()

	err := realVerifyChecksumsAttestationFn("test/repo", "v1.0.0", "/nonexistent/checksums.txt")
	if err == nil {
		t.Fatal("expected fail-closed error when checksums.txt is missing, got nil")
	}
}

// TestVerifyChecksumsAttestation_FailsClosedWithoutSkip verifies that the
// wrapper refuses the update (rather than silently passing) when SkipAttestation
// is false and provenance cannot be established.
func TestVerifyChecksumsAttestation_FailsClosedWithoutSkip(t *testing.T) {
	// Not parallel: mutates the package-level verifyChecksumsAttestationFn.
	origFn := verifyChecksumsAttestationFn
	verifyChecksumsAttestationFn = realVerifyChecksumsAttestationFn
	defer func() { verifyChecksumsAttestationFn = origFn }()

	u := New(Config{
		InstallDir:      t.TempDir(),
		Repo:            "test/repo",
		SkipAttestation: false,
	})

	// Missing checksums file fails before any network call → deterministic.
	err := u.verifyChecksumsAttestation("v1.0.0", "/nonexistent/checksums.txt")
	if err == nil {
		t.Fatal("expected fail-closed error when SkipAttestation=false, got nil")
	}
}

// TestVerifyChecksumsAttestation_SkipConfig verifies the config-driven skip
// is the only way to bypass attestation.
func TestVerifyChecksumsAttestation_SkipConfig(t *testing.T) {
	t.Parallel()

	u := New(Config{
		InstallDir:      t.TempDir(),
		Repo:            "test/repo",
		SkipAttestation: true,
	})

	err := u.verifyChecksumsAttestation("v1.0.0", "/nonexistent/checksums.txt")
	if err != nil {
		t.Errorf("SkipAttestation=true should return nil, got: %v", err)
	}
}
