// SPDX-License-Identifier: AGPL-3.0-or-later

package updater

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

// archiveToInstall maps binary names in the release archive to the filenames
// used by install.sh in the install directory. The archive uses short names;
// install.sh adds the "pilot-" prefix to avoid conflicts with system binaries.
var archiveToInstall = map[string]string{
	"daemon":   "pilot-daemon",
	"gateway":  "pilot-gateway",
	"updater":  "pilot-updater",
	"pilotctl": "pilotctl",
}

// maxDownloadBytes caps the size of any single downloaded file.
const maxDownloadBytes = 256 * 1024 * 1024

// Config holds the updater configuration.
type Config struct {
	CheckInterval time.Duration
	Repo          string // "owner/repo"
	InstallDir    string
	Version       string // updater's own version (used for user-agent)

	// PinnedVersion locks the updater to a specific release tag
	// (e.g. "v1.10.5"). When set, the updater installs exactly that
	// version — regardless of whether it is newer, older, or already
	// current — and will not chase the latest release. An empty
	// string (default) preserves the existing "always follow latest"
	// behaviour. Set to an empty string to un-pin and resume
	// auto-updating to the latest stable.
	PinnedVersion string

	// SkipAttestation disables SLSA attestation verification of
	// checksums.txt. This is the ONLY way to bypass attestation: when
	// false (the default), the updater fetches the GitHub SLSA provenance
	// bundle and verifies it in-process with sigstore-go (no gh CLI, no
	// external tooling required); if it cannot verify, the update is
	// refused (fails closed). Intended for test environments where the
	// test repos do not have real attestations. Leaving it false in
	// production keeps provenance verification mandatory. The SHA256
	// checksums.txt match is always enforced regardless of this flag.
	SkipAttestation bool

	// StatePath, when non-empty, points to a JSON control file
	// {"enabled": bool} that gates the AUTOMATIC update loop and is
	// re-read on every tick. When the file is absent or {"enabled":
	// false} the loop performs NO updates — so any deployment that sets
	// StatePath is OFF BY DEFAULT until explicitly enabled (e.g. via
	// `pilotctl update enable`). A manual one-shot RunOnce always runs,
	// ignoring this gate. An empty StatePath preserves the legacy
	// always-on loop behaviour for backward compatibility.
	StatePath string

	// StatusPath is where the updater records the outcome of every check
	// (see Status): last check time and result, last error, consecutive
	// failures, installed and latest versions, and whether the loop is
	// running. Empty means "update-state.json next to StatePath"; with
	// neither set, nothing is written (LastStatus still reports it).
	StatusPath string
}

// Updater periodically checks GitHub Releases for new versions and optionally applies them.
type Updater struct {
	config Config
	client *http.Client // GitHub API calls (short total timeout)
	stopCh chan struct{}
	wg     sync.WaitGroup
	exitFn func(int) // injectable for testing; defaults to os.Exit

	// dlClient downloads release assets. It has no total timeout: see
	// downloadFile. Nil falls back to client without its total timeout.
	dlClient       *http.Client
	dlIdleTimeout  time.Duration // 0 = defaultDownloadIdleTimeout
	dlRetryBackoff time.Duration // base pause between download attempts

	// Process hooks, injectable for tests. Zero values mean the real thing.
	runCmd      func(name string, args ...string) ([]byte, error)
	killFn      func(pid int, sig syscall.Signal) error
	procRoot    string        // "" = /proc
	goos        string        // "" = runtime.GOOS; selects the restart mechanism
	systemdDirs []string      // nil = systemdSystemUnitDirs
	restartWait time.Duration // 0 = defaultRestartWait
	restartPoll time.Duration // 0 = defaultRestartPoll

	statusMu       sync.Mutex
	status         Status
	statusLockWait time.Duration // 0 = defaultStatusLockWait
}

// runCommand runs an external command and returns its combined output. The
// only command the updater ever runs is launchctl (daemon restart on macOS);
// it never runs gh or any other tool. Tests replace this so the suite cannot
// restart a real daemon on a developer machine.
var runCommand = func(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// GitHubRelease represents a subset of the GitHub release API response.
type GitHubRelease struct {
	TagName string        `json:"tag_name"`
	Assets  []GitHubAsset `json:"assets"`
}

// GitHubAsset represents a release asset.
type GitHubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// New creates a new Updater.
func New(cfg Config) *Updater {
	return &Updater{
		config:         cfg,
		client:         &http.Client{Timeout: apiTimeout, Transport: newTransport()},
		dlClient:       &http.Client{Transport: newTransport()},
		dlRetryBackoff: defaultDownloadRetryBackoff,
		stopCh:         make(chan struct{}),
		exitFn:         os.Exit,
	}
}

// Start begins the periodic check loop.
func (u *Updater) Start() {
	u.wg.Add(1)
	go u.checkLoop()
}

// Stop signals the check loop to stop and waits for it to finish.
func (u *Updater) Stop() {
	close(u.stopCh)
	u.wg.Wait()
}

// RunOnce runs the update check once synchronously and returns. Unlike
// Start, it does not enter a periodic loop — it performs a single check
// (checking the pinned version or latest release), applies the update if
// available, and returns. Useful for one-shot invocations from
// `pilotctl update` and similar CLI commands. It ALWAYS runs — the
// StatePath enabled-gate applies only to the automatic loop, so a manual
// `pilotctl update` works even when auto-update is disabled.
//
// It returns the check's error (nil when already up to date or when the
// update was installed), so callers can report failure and exit non-zero.
// LastStatus describes the outcome in detail, and the same record is written
// to the status file when one is configured.
//
// RunOnce never exits the calling process. When the release replaces the
// pilot-updater binary, the daemon is still restarted onto the new binaries;
// a separately running updater service picks up its new binary the next
// time its service manager restarts it.
func (u *Updater) RunOnce() error {
	u.recoverPendingRestart()
	return u.runCheck(TriggerManual)
}

// enabled reports whether the automatic update loop may apply updates. With
// no StatePath configured it returns true (legacy always-on). Otherwise it
// reads the JSON control file and defaults to false (off) when the file is
// missing, unreadable, or malformed — auto-update is strictly opt-in.
func (u *Updater) enabled() bool {
	if u.config.StatePath == "" {
		return true
	}
	data, err := os.ReadFile(u.config.StatePath)
	if err != nil {
		return false
	}
	var s struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return false
	}
	return s.Enabled
}

func (u *Updater) checkLoop() {
	defer u.wg.Done()

	// Record that an updater loop is running, so `pilotctl update status`
	// can tell "enabled and running" from "enabled but no updater process"
	// (e.g. a container or WSL host without systemd).
	started := time.Now().UTC()
	u.updateStatus(func(s *Status) {
		s.LoopPID = os.Getpid()
		s.LoopStartedAt = started
	})

	// On startup, catch any missed daemon restart from a previous update cycle
	// (e.g. old macOS updater replaced the binary but never called launchctl).
	u.recoverPendingRestart()

	// Run once immediately on start (only if auto-update is enabled).
	if u.enabled() {
		_ = u.runCheck(TriggerAuto)
	} else {
		slog.Info("auto-update disabled; loop idle until enabled", "state_path", u.config.StatePath)
	}
	u.markNextCheck()

	ticker := time.NewTicker(u.config.CheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			// Add 0-30s jitter to avoid thundering herd.
			// Use NewTimer + Stop so the goroutine is reclaimed immediately
			// on stopCh rather than lingering until the jitter duration
			// expires (time.After leak on early exit).
			jitter := time.Duration(rand.Int63n(int64(30 * time.Second)))
			jitterTimer := time.NewTimer(jitter)
			select {
			case <-jitterTimer.C:
			case <-u.stopCh:
				jitterTimer.Stop()
				return
			}
			// Re-read the gate each tick so `pilotctl update enable/disable`
			// takes effect without restarting the updater.
			if u.enabled() {
				_ = u.runCheck(TriggerAuto)
			} else {
				slog.Debug("auto-update disabled; skipping tick")
			}
			u.markNextCheck()
		case <-u.stopCh:
			return
		}
	}
}

// markNextCheck records when the loop wakes up next. It doubles as the
// loop's heartbeat: a NextCheckAt far in the past means no loop is running.
func (u *Updater) markNextCheck() {
	next := time.Now().UTC().Add(u.config.CheckInterval)
	u.updateStatus(func(s *Status) { s.NextCheckAt = next })
}

// checkOnce runs one automatic (loop) check. See runCheck.
func (u *Updater) checkOnce() error {
	return u.runCheck(TriggerAuto)
}

// runCheck performs one update check, restarts the daemon when an update was
// installed, records the outcome in the status file and returns the check's
// error.
func (u *Updater) runCheck(trigger string) error {
	out, err := u.check()
	if out.installed == "" {
		// A daemon now running the installed binary (restarted by hand after
		// an earlier restart failure) settles restart_error.
		out.daemonCurrent = u.daemonOnInstalledBinary()
	}
	if err != nil {
		slog.Error("update check failed", "trigger", trigger, "error", err)
		u.recordCheck(trigger, out, err)
		return err
	}
	if out.installed == "" {
		u.recordCheck(trigger, out, nil)
		return nil
	}
	slog.Info("update applied successfully", "version", out.installed)

	if out.updaterReplaced && trigger == TriggerAuto {
		// This process is now running a stale updater. Record the update,
		// then exit so launchd/systemd restarts the process with the new
		// binary. On startup the new process runs recoverPendingRestart(),
		// which restarts the daemon (and records the result).
		u.recordCheck(trigger, out, nil)
		slog.Info("updater binary replaced — exiting for process manager to restart with new binary")
		exit := u.exitFn
		if exit == nil {
			exit = os.Exit
		}
		exit(0)
		return nil
	}
	if out.updaterReplaced {
		slog.Info("pilot-updater binary replaced; a running updater service uses it after its next restart")
	}

	// Restart the daemon onto the new binaries (SIGTERM / launchctl).
	out.restartErr = u.signalDaemonRestart()
	u.touchRestartRecord()
	u.recordCheck(trigger, out, nil)
	return nil
}

// check fetches the target release (latest, or the pinned tag) and installs
// it when needed. It does not restart anything or exit.
func (u *Updater) check() (checkOutcome, error) {
	slog.Debug("checking for updates")

	// Pinned-version path: install a specific version regardless of
	// whether it is newer or older than the current install. Once the
	// pinned version is installed, subsequent ticks are no-ops until
	// the pin is changed or cleared.
	if u.config.PinnedVersion != "" {
		return u.checkPinnedVersion()
	}

	// Default path: follow the latest release.
	var out checkOutcome
	release, err := u.fetchLatestRelease()
	if err != nil {
		return out, fmt.Errorf("fetch latest release: %w", err)
	}

	latest, err := ParseSemver(release.TagName)
	if err != nil {
		return out, fmt.Errorf("parse release tag %q: %w", release.TagName, err)
	}
	out.latest = release.TagName

	current, err := u.currentVersion()
	if err != nil {
		return out, fmt.Errorf("read installed version: %w", err)
	}
	out.current = current.String()

	slog.Info("version check", "current", current.String(), "latest", latest.String())

	if !latest.NewerThan(current) {
		slog.Debug("already up to date")
		return out, nil
	}

	slog.Info("new version available, updating", "current", current.String(), "latest", latest.String())

	replaced, err := u.applyUpdate(release)
	if err != nil {
		return out, fmt.Errorf("apply update %s: %w", release.TagName, err)
	}
	out.installed = release.TagName
	out.updaterReplaced = replaced
	return out, nil
}

// checkPinnedVersion installs the exact release specified by
// Config.PinnedVersion if it is not already installed. Unlike the
// default latest-following path, it does not compare versions — it
// fetches the named release and applies it unconditionally when the
// current install differs from the pin.
func (u *Updater) checkPinnedVersion() (checkOutcome, error) {
	out := checkOutcome{latest: u.config.PinnedVersion}
	pinned, err := ParseSemver(u.config.PinnedVersion)
	if err != nil {
		return out, fmt.Errorf("invalid pinned version %q: %w", u.config.PinnedVersion, err)
	}

	current, err := u.currentVersion()
	if err != nil {
		return out, fmt.Errorf("read installed version: %w", err)
	}
	out.current = current.String()

	if current == pinned {
		slog.Info("pinned version already installed", "version", pinned.String())
		return out, nil
	}

	slog.Info("pinned version requested, installing",
		"current", current.String(),
		"pinned", pinned.String(),
	)

	release, err := u.fetchReleaseByTag(u.config.PinnedVersion)
	if err != nil {
		return out, fmt.Errorf("fetch pinned release %s: %w", u.config.PinnedVersion, err)
	}

	replaced, err := u.applyUpdate(release)
	if err != nil {
		return out, fmt.Errorf("apply pinned update %s: %w", u.config.PinnedVersion, err)
	}

	slog.Info("pinned version installed", "version", pinned.String())
	out.installed = release.TagName
	out.updaterReplaced = replaced
	return out, nil
}

func (u *Updater) fetchLatestRelease() (*GitHubRelease, error) {
	return u.fetchRelease("")
}

// fetchReleaseByTag fetches a specific release by its Git tag.
// Example tag: "v1.10.5".
func (u *Updater) fetchReleaseByTag(tag string) (*GitHubRelease, error) {
	return u.fetchRelease(tag)
}

// fetchRelease returns the GitHub release for the given tag. If tag is
// empty it fetches the latest release.
func (u *Updater) fetchRelease(tag string) (*GitHubRelease, error) {
	var url string
	if tag == "" {
		url = fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", u.config.Repo)
	} else {
		url = fmt.Sprintf("https://api.github.com/repos/%s/releases/tags/%s", u.config.Repo, tag)
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if u.config.Version != "" {
		req.Header.Set("User-Agent", "pilot-updater/"+u.config.Version)
	}

	// Sends GITHUB_TOKEN/GH_TOKEN when set (rate limit only; never required).
	resp, err := doGitHubAPI(u.client, req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, githubAPIError("GitHub API", resp)
	}

	var release GitHubRelease
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &release, nil
}

func (u *Updater) currentVersion() (Semver, error) {
	daemonPath := filepath.Join(u.config.InstallDir, "pilot-daemon")
	if _, err := os.Stat(daemonPath); err != nil {
		return Semver{}, fmt.Errorf("daemon binary not found at %s: %w", daemonPath, err)
	}

	// Read the version file we write after each update.
	versionFile := filepath.Join(u.config.InstallDir, ".pilot-version")
	data, err := os.ReadFile(versionFile)
	if err != nil {
		// No version file means this is a pre-updater install. Treat as 0.0.0
		// so any published release triggers an update immediately.
		slog.Warn("no version file, treating current version as 0.0.0", "path", versionFile)
		return Semver{}, nil
	}
	return ParseSemver(strings.TrimSpace(string(data)))
}

// applyUpdate downloads, verifies and installs release into InstallDir. It
// reports whether the pilot-updater binary itself was replaced. It does not
// restart the daemon or exit: runCheck decides that, after recording the
// outcome.
func (u *Updater) applyUpdate(release *GitHubRelease) (updaterReplaced bool, err error) {
	archiveName := fmt.Sprintf("pilot-%s-%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	var archiveURL, checksumsURL string

	for _, a := range release.Assets {
		switch a.Name {
		case archiveName:
			archiveURL = a.BrowserDownloadURL
		case "checksums.txt":
			checksumsURL = a.BrowserDownloadURL
		}
	}

	if archiveURL == "" {
		return false, fmt.Errorf("no asset %q in release %s", archiveName, release.TagName)
	}

	tmpDir, err := os.MkdirTemp("", "pilot-update-*")
	if err != nil {
		return false, fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Download archive.
	archivePath := filepath.Join(tmpDir, archiveName)
	if err := u.downloadFile(archiveURL, archivePath); err != nil {
		return false, fmt.Errorf("download archive: %w", err)
	}

	// Verify checksums. Both the asset's presence in the release AND
	// a successful download are now mandatory — silently skipping
	// verification was the P0 RCE vector reported in 2026-05-26:
	// an attacker with GitHub repo write access (compromised PAT,
	// supply-chain compromise) could publish a release with just the
	// malicious archive and no checksums.txt, and every Pilot node
	// would auto-install it unverified. A network MITM dropping just
	// the checksums.txt fetch had the same effect.
	//
	// The checksums.txt file itself is now attested via SLSA
	// (actions/attest-build-provenance@v2 in release.yml, PILOT-120
	// PR #166). verifyChecksumsAttestation (below) checks provenance
	// before trusting the checksums file, closing the "matched fake
	// binary + fake checksums" gap.
	if checksumsURL == "" {
		return false, fmt.Errorf("release %s has no checksums.txt asset; refusing to install unverified binary", release.TagName)
	}
	checksumsPath := filepath.Join(tmpDir, "checksums.txt")
	if err := u.downloadFile(checksumsURL, checksumsPath); err != nil {
		return false, fmt.Errorf("download checksums: %w", err)
	}

	// Verify checksums.txt provenance via GitHub SLSA attestation.
	// The release workflow attests checksums.txt via
	// actions/attest-build-provenance (PILOT-120, PR #166). This closes
	// the "attacker publishes matched fake binary + fake checksums.txt"
	// gap — the attestation ties checksums.txt to the trusted CI
	// workflow. Verification is done in-process via sigstore-go (no gh
	// CLI required) and is bound to release.TagName so an older,
	// still-attested checksums.txt cannot be replayed under a new tag
	// (validated rollback). Fails closed unless SkipAttestation is set.
	if err := u.verifyChecksumsAttestation(release.TagName, checksumsPath); err != nil {
		return false, fmt.Errorf("checksums attestation verification failed: %w", err)
	}

	if err := VerifyChecksum(archivePath, archiveName, checksumsPath); err != nil {
		return false, fmt.Errorf("checksum verification failed: %w", err)
	}
	slog.Info("checksum verified", "archive", archiveName)

	// Extract to staging directory.
	stagingDir := filepath.Join(tmpDir, "staging")
	if err := os.MkdirAll(stagingDir, 0755); err != nil {
		return false, fmt.Errorf("create staging dir: %w", err)
	}
	if err := extractTarGz(archivePath, stagingDir); err != nil {
		return false, fmt.Errorf("extract archive: %w", err)
	}

	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		return false, fmt.Errorf("read staging dir: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		installName, ok := archiveToInstall[entry.Name()]
		if !ok {
			slog.Debug("skipping server binary", "name", entry.Name())
			continue
		}
		src := filepath.Join(stagingDir, entry.Name())
		dst := filepath.Join(u.config.InstallDir, installName)
		if err := replaceBinary(src, dst); err != nil {
			return false, fmt.Errorf("replace %s: %w", installName, err)
		}
		slog.Info("replaced binary", "name", installName)
		if installName == "pilot-updater" {
			updaterReplaced = true
		}
	}

	// Write version file before any exit path so the new process doesn't
	// re-download the same release when it starts. Fsync ensures the write
	// survives an immediate os.Exit(0).
	if err := writeFileSync(
		filepath.Join(u.config.InstallDir, ".pilot-version"),
		[]byte(release.TagName+"\n"),
		0644,
	); err != nil {
		slog.Warn("failed to write version file", "error", err)
	}

	return updaterReplaced, nil
}

// VerifyChecksum checks the SHA256 of archivePath against the checksums file.
func VerifyChecksum(archivePath, archiveName, checksumsPath string) error {
	// Read checksums file.
	data, err := os.ReadFile(checksumsPath)
	if err != nil {
		return fmt.Errorf("read checksums: %w", err)
	}

	// Find the line for our archive.
	var expectedHash string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Format: "hash  filename" or "hash filename"
		parts := strings.Fields(line)
		if len(parts) >= 2 && parts[1] == archiveName {
			expectedHash = parts[0]
			break
		}
	}
	if expectedHash == "" {
		return fmt.Errorf("no checksum found for %s", archiveName)
	}

	// Compute actual hash.
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	actualHash := hex.EncodeToString(h.Sum(nil))

	if actualHash != expectedHash {
		return fmt.Errorf("checksum mismatch: expected %s, got %s", expectedHash, actualHash)
	}
	return nil
}

func extractTarGz(archivePath, destDir string) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip reader: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar next: %w", err)
		}

		// Only extract regular files, skip directories and symlinks.
		if hdr.Typeflag != tar.TypeReg {
			continue
		}

		// Sanitize path — prevent directory traversal. filepath.Base strips
		// any leading path / ".." elements from the archive entry name.
		name := filepath.Base(hdr.Name)
		if name == "." || name == ".." {
			continue
		}

		dst := filepath.Join(destDir, name)
		// Defence in depth: reject any entry whose cleaned destination would
		// escape destDir (Zip Slip / CWE-022). filepath.Base already prevents
		// this, but the explicit containment check makes the invariant
		// auditable and is the sanitizer CodeQL's go/zipslip dataflow expects.
		cleanDest := filepath.Clean(destDir) + string(os.PathSeparator)
		if !strings.HasPrefix(filepath.Clean(dst)+string(os.PathSeparator), cleanDest) {
			return fmt.Errorf("archive entry %q escapes destination dir", hdr.Name)
		}
		out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755) //nolint:gosec // G302: extracted files are executables and must be 0755
		if err != nil {
			return fmt.Errorf("create %s: %w", name, err)
		}
		// Cap per-entry extraction to bound a decompression bomb: a
		// crafted archive could expand far beyond maxDownloadBytes even
		// after the on-disk size was capped at download time. Copy one
		// byte past the cap so we can tell "exactly at limit" from
		// "exceeded".
		n, err := io.Copy(out, io.LimitReader(tr, maxDownloadBytes+1))
		if err != nil {
			out.Close()
			return fmt.Errorf("write %s: %w", name, err)
		}
		if n > maxDownloadBytes {
			out.Close()
			return fmt.Errorf("archive entry %q exceeds max extract size %d bytes", name, maxDownloadBytes)
		}
		out.Close()
	}
	return nil
}

func replaceBinary(src, dst string) error {
	// Refuse to swap in a zero-byte staged binary. A 0-byte rename over the
	// live daemon binary would brick the daemon on next start; better to fail
	// loudly here so the operator sees the update failed and the existing
	// binary keeps running.
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	if fi.Size() == 0 {
		return fmt.Errorf("refusing to replace binary with empty source: %s", src)
	}

	// Write to a temp file beside the destination, then atomically rename.
	// This avoids "text file busy" on Linux (rename unlinks the old inode
	// while the running process keeps its file descriptor open) and prevents
	// a partial write leaving a corrupt binary at the destination path.
	tmp := dst + ".new"
	srcFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	tmpFile, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(tmpFile, srcFile); err != nil {
		tmpFile.Close()
		os.Remove(tmp)
		return err
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	// Atomic swap — on the same filesystem this is a single syscall.
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func (u *Updater) recoverPendingRestart() {
	// Only attempt recovery if the updater has previously completed at least
	// one update cycle. On a fresh install there is no history to recover.
	versionFile := filepath.Join(u.config.InstallDir, ".pilot-version")
	if _, err := os.Stat(versionFile); err != nil {
		return
	}

	daemonBin := filepath.Join(u.config.InstallDir, "pilot-daemon")
	restartRecord := filepath.Join(u.config.InstallDir, ".daemon-last-restart")

	binStat, err := os.Stat(daemonBin)
	if err != nil {
		return
	}
	recordStat, err := os.Stat(restartRecord)
	if err != nil || binStat.ModTime().After(recordStat.ModTime()) {
		slog.Info("daemon binary updated since last restart, triggering restart")
		err := u.signalDaemonRestart()
		u.touchRestartRecord()
		u.recordRestart(err)
	}
}

func (u *Updater) touchRestartRecord() {
	path := filepath.Join(u.config.InstallDir, ".daemon-last-restart")
	if err := writeFileSync(path, []byte(time.Now().Format(time.RFC3339)+"\n"), 0644); err != nil {
		slog.Warn("failed to update restart record", "error", err)
	}
}

// writeFileSync writes data to path and fsyncs before returning so the write
// survives an immediately following os.Exit(0).
func writeFileSync(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// signalDaemonRestart restarts the daemon so it runs the newly installed
// binaries. It returns an error when the daemon may still be running the old
// binary; a daemon that is not running at all is not an error (its next
// start uses the new binary).
func (u *Updater) signalDaemonRestart() error {
	if u.targetOS() == "darwin" {
		return u.signalDaemonRestartDarwin()
	}
	return u.signalDaemonRestartLinux()
}

// command runs an external command via the injectable hook.
func (u *Updater) command(name string, args ...string) ([]byte, error) {
	if u.runCmd != nil {
		return u.runCmd(name, args...)
	}
	return runCommand(name, args...)
}

func (u *Updater) signalDaemonRestartDarwin() error {
	// On macOS the daemon is managed by launchd. Use launchctl kickstart -k
	// to kill the running instance and restart it immediately. The label
	// matches the plist written by install.sh.
	uid := os.Getuid()
	label := "network.pilotprotocol.pilot-daemon"
	target := fmt.Sprintf("gui/%d/%s", uid, label)
	out, err := u.command("launchctl", "kickstart", "-k", target)
	if err != nil {
		output := strings.TrimSpace(string(out))
		slog.Warn("launchctl kickstart failed — restart daemon manually",
			"target", target, "err", err, "output", output)
		return fmt.Errorf("restart daemon (launchctl kickstart -k %s): %v %s", target, err, output)
	}
	slog.Info("daemon restarted via launchctl", "target", target)
	return nil
}

func (u *Updater) signalDaemonRestartLinux() error {
	// On Linux, find the daemon process via /proc/<pid>/exe. When systemd
	// will start it again (see supervisor.go), send SIGTERM and wait for the
	// new process; otherwise leave it running and say how to restart it.
	daemonPath := filepath.Join(u.config.InstallDir, "pilot-daemon")
	procRoot := u.procDir()
	pid, err := findProcessByExe(procRoot, daemonPath)
	if err != nil {
		slog.Warn("cannot read /proc — restart daemon manually", "error", err)
		return fmt.Errorf("restart daemon: find process: %w", err)
	}
	if pid == 0 {
		// Not running: its next start uses the new binary.
		slog.Warn("daemon process not found — restart daemon manually if it is running", "path", daemonPath)
		return nil
	}
	unit, err := u.daemonSupervisor(procRoot, pid, daemonPath)
	if err != nil {
		slog.Warn("daemon not restarted onto the new binary", "pid", pid, "reason", err)
		return fmt.Errorf("restart daemon: %w", err)
	}
	slog.Info("sending SIGTERM to daemon; systemd starts it again", "pid", pid, "unit", unit)
	kill := u.killFn
	if kill == nil {
		kill = syscall.Kill
	}
	if err := kill(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("restart daemon: signal pid %d: %w", pid, err)
	}
	newPid, err := u.waitForDaemonRestart(procRoot, daemonPath, pid, unit)
	if err != nil {
		slog.Error("daemon did not come back after SIGTERM", "unit", unit, "error", err)
		return fmt.Errorf("restart daemon: %w", err)
	}
	slog.Info("daemon restarted on the new binary", "unit", unit, "pid", newPid)
	return nil
}

// findProcessByExe returns the pid of a process (other than this one) whose
// executable is exePath, or 0 when there is none. After an update the old
// binary has been renamed over, so the running daemon's /proc/<pid>/exe
// reads "<exePath> (deleted)". That must match too, or the daemon is never
// restarted onto the new version.
func findProcessByExe(procRoot, exePath string) (int, error) {
	found := 0
	err := forEachProcExe(procRoot, func(pid int, exe string) bool {
		if exe == exePath || exe == exePath+" (deleted)" {
			found = pid
			return false
		}
		return true
	})
	return found, err
}

// verifyChecksumsAttestation verifies the SLSA provenance of checksums.txt for
// the given release tag. The release workflow attests checksums.txt via
// actions/attest-build-provenance (PILOT-120, PR #166). This closes the
// "attacker publishes matched fake binary + fake checksums.txt" gap — the
// attestation ties checksums.txt to the trusted CI workflow identity — and
// binds to the tag so a previously-attested checksums.txt cannot be replayed
// under a different release.
//
// The gate FAILS CLOSED: verification is performed in-process with sigstore-go
// (no gh CLI, no external tooling). If the provenance bundle cannot be fetched
// or cryptographically verified, an error is returned and the update does not
// proceed. The only way to skip attestation is Config.SkipAttestation — there
// is no implicit skip. This is deliberate: the prior "gh absent => fail"
// behaviour combined with headless hosts that never install gh meant
// auto-update could never apply; verifying in-process keeps provenance
// mandatory AND makes auto-update actually work on a stock install.
func (u *Updater) verifyChecksumsAttestation(tag, checksumsPath string) error {
	if u.config.SkipAttestation {
		slog.Warn("SLSA attestation verification disabled (SkipAttestation=true) — checksums provenance is NOT verified")
		return nil
	}
	return verifyChecksumsAttestationFn(u.config.Repo, tag, checksumsPath)
}

// verifyChecksumsAttestationFn is the attestation verification implementation
// used by the updater. Tests may replace it to avoid requiring a real GitHub
// repo with SLSA attestations; they restore realVerifyChecksumsAttestationFn
// to exercise the production path. See attestation.go for the implementation.
var verifyChecksumsAttestationFn = realVerifyChecksumsAttestationFn
