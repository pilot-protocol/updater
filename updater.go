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
	// false (the default) and the gh CLI is absent, verification fails
	// closed and the update is refused. Intended for test environments
	// where the test repos do not have real attestations. Leaving it
	// false in production keeps provenance verification mandatory.
	// The SHA256 checksums.txt match is always enforced regardless of
	// this flag.
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
}

// Updater periodically checks GitHub Releases for new versions and optionally applies them.
type Updater struct {
	config Config
	client *http.Client
	stopCh chan struct{}
	wg     sync.WaitGroup
	exitFn func(int) // injectable for testing; defaults to os.Exit
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
		config: cfg,
		client: &http.Client{Timeout: 30 * time.Second},
		stopCh: make(chan struct{}),
		exitFn: os.Exit,
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
func (u *Updater) RunOnce() {
	u.recoverPendingRestart()
	u.checkOnce()
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

	// On startup, catch any missed daemon restart from a previous update cycle
	// (e.g. old macOS updater replaced the binary but never called launchctl).
	u.recoverPendingRestart()

	// Run once immediately on start (only if auto-update is enabled).
	if u.enabled() {
		u.checkOnce()
	} else {
		slog.Info("auto-update disabled; loop idle until enabled", "state_path", u.config.StatePath)
	}

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
				u.checkOnce()
			} else {
				slog.Debug("auto-update disabled; skipping tick")
			}
		case <-u.stopCh:
			return
		}
	}
}

func (u *Updater) checkOnce() {
	slog.Debug("checking for updates")

	// Pinned-version path: install a specific version regardless of
	// whether it is newer or older than the current install. Once the
	// pinned version is installed, subsequent ticks are no-ops until
	// the pin is changed or cleared.
	if u.config.PinnedVersion != "" {
		u.checkPinnedVersion()
		return
	}

	// Default path: follow the latest release.
	release, err := u.fetchLatestRelease()
	if err != nil {
		slog.Error("failed to fetch latest release", "error", err)
		return
	}

	latest, err := ParseSemver(release.TagName)
	if err != nil {
		slog.Error("failed to parse release tag", "tag", release.TagName, "error", err)
		return
	}

	current, err := u.currentVersion()
	if err != nil {
		slog.Error("failed to get current version", "error", err)
		return
	}

	slog.Info("version check", "current", current.String(), "latest", latest.String())

	if !latest.NewerThan(current) {
		slog.Debug("already up to date")
		return
	}

	slog.Info("new version available, updating", "current", current.String(), "latest", latest.String())

	if err := u.applyUpdate(release); err != nil {
		slog.Error("failed to apply update", "error", err)
		return
	}

	slog.Info("update applied successfully", "version", latest.String())
	u.touchRestartRecord()
}

// checkPinnedVersion installs the exact release specified by
// Config.PinnedVersion if it is not already installed. Unlike the
// default latest-following path, it does not compare versions — it
// fetches the named release and applies it unconditionally when the
// current install differs from the pin.
func (u *Updater) checkPinnedVersion() {
	pinned, err := ParseSemver(u.config.PinnedVersion)
	if err != nil {
		slog.Error("invalid pinned version", "version", u.config.PinnedVersion, "error", err)
		return
	}

	current, err := u.currentVersion()
	if err != nil {
		slog.Error("failed to get current version", "error", err)
		return
	}

	if current == pinned {
		slog.Info("pinned version already installed", "version", pinned.String())
		return
	}

	slog.Info("pinned version requested, installing",
		"current", current.String(),
		"pinned", pinned.String(),
	)

	release, err := u.fetchReleaseByTag(u.config.PinnedVersion)
	if err != nil {
		slog.Error("failed to fetch pinned release", "tag", u.config.PinnedVersion, "error", err)
		return
	}

	if err := u.applyUpdate(release); err != nil {
		slog.Error("failed to apply pinned update", "error", err)
		return
	}

	slog.Info("pinned version installed", "version", pinned.String())
	u.touchRestartRecord()
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

	resp, err := u.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, string(body))
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

func (u *Updater) applyUpdate(release *GitHubRelease) error {
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
		return fmt.Errorf("no asset %q in release %s", archiveName, release.TagName)
	}

	tmpDir, err := os.MkdirTemp("", "pilot-update-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Download archive.
	archivePath := filepath.Join(tmpDir, archiveName)
	if err := u.downloadFile(archiveURL, archivePath); err != nil {
		return fmt.Errorf("download archive: %w", err)
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
		return fmt.Errorf("release %s has no checksums.txt asset; refusing to install unverified binary", release.TagName)
	}
	checksumsPath := filepath.Join(tmpDir, "checksums.txt")
	if err := u.downloadFile(checksumsURL, checksumsPath); err != nil {
		return fmt.Errorf("download checksums: %w", err)
	}

	// Verify checksums.txt provenance via GitHub SLSA attestation.
	// The release workflow attests checksums.txt via
	// actions/attest-build-provenance@v2 (PILOT-120, PR #166).
	// This closes the "attacker publishes matched fake binary +
	// fake checksums.txt" gap — the attestation ties checksums.txt
	// to the trusted CI workflow. Fails closed when the gh CLI is
	// unavailable unless SkipAttestation is explicitly set.
	if err := u.verifyChecksumsAttestation(checksumsPath); err != nil {
		return fmt.Errorf("checksums attestation verification failed: %w", err)
	}

	if err := VerifyChecksum(archivePath, archiveName, checksumsPath); err != nil {
		return fmt.Errorf("checksum verification failed: %w", err)
	}
	slog.Info("checksum verified", "archive", archiveName)

	// Extract to staging directory.
	stagingDir := filepath.Join(tmpDir, "staging")
	if err := os.MkdirAll(stagingDir, 0755); err != nil {
		return fmt.Errorf("create staging dir: %w", err)
	}
	if err := extractTarGz(archivePath, stagingDir); err != nil {
		return fmt.Errorf("extract archive: %w", err)
	}

	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		return fmt.Errorf("read staging dir: %w", err)
	}

	updaterReplaced := false
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
			return fmt.Errorf("replace %s: %w", installName, err)
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

	// If the updater binary itself was replaced, exit so launchd/systemd
	// restarts the process with the new binary. On startup the new process
	// runs recoverPendingRestart() which will handle the daemon restart.
	// Explicitly clean up tmpDir first since defer won't run after os.Exit.
	if updaterReplaced {
		os.RemoveAll(tmpDir)
		slog.Info("updater binary replaced — exiting for process manager to restart with new binary")
		u.exitFn(0)
	}

	// Signal daemon to restart (SIGTERM for graceful shutdown).
	u.signalDaemonRestart()

	return nil
}

func (u *Updater) downloadFile(url, dst string) error {
	resp, err := u.client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d for %s", resp.StatusCode, url)
	}

	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()

	// Read one byte past the cap so we can distinguish "exactly at the limit"
	// from "exceeded the limit". A plain io.LimitReader(maxDownloadBytes) would
	// silently truncate oversize archives — the SHA256 check would then fail
	// with a confusing "checksum mismatch" instead of telling the operator
	// the archive is too large.
	n, err := io.Copy(f, io.LimitReader(resp.Body, maxDownloadBytes+1))
	if err != nil {
		return err
	}
	if n > maxDownloadBytes {
		return fmt.Errorf("archive exceeds max download size %d bytes", maxDownloadBytes)
	}
	return nil
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

		// Sanitize path — prevent directory traversal.
		name := filepath.Base(hdr.Name)
		if name == "." || name == ".." {
			continue
		}

		dst := filepath.Join(destDir, name)
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
		u.signalDaemonRestart()
		u.touchRestartRecord()
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

func (u *Updater) signalDaemonRestart() {
	if runtime.GOOS == "darwin" {
		u.signalDaemonRestartDarwin()
		return
	}
	u.signalDaemonRestartLinux()
}

func (u *Updater) signalDaemonRestartDarwin() {
	// On macOS the daemon is managed by launchd. Use launchctl kickstart -k
	// to kill the running instance and restart it immediately. The label
	// matches the plist written by install.sh.
	uid := os.Getuid()
	label := "network.pilotprotocol.pilot-daemon"
	target := fmt.Sprintf("gui/%d/%s", uid, label)
	out, err := exec.Command("launchctl", "kickstart", "-k", target).CombinedOutput()
	if err != nil {
		slog.Warn("launchctl kickstart failed — restart daemon manually",
			"target", target, "err", err, "output", strings.TrimSpace(string(out)))
		return
	}
	slog.Info("daemon restarted via launchctl", "target", target)
}

func (u *Updater) signalDaemonRestartLinux() {
	// On Linux, find the daemon process via /proc/<pid>/exe and send SIGTERM.
	// systemd Restart=on-failure will relaunch it automatically.
	daemonPath := filepath.Join(u.config.InstallDir, "pilot-daemon")
	entries, err := os.ReadDir("/proc")
	if err != nil {
		slog.Warn("cannot read /proc — restart daemon manually")
		return
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		exe, err := os.Readlink(filepath.Join("/proc", entry.Name(), "exe"))
		if err != nil {
			continue
		}
		if exe == daemonPath {
			pid := 0
			fmt.Sscanf(entry.Name(), "%d", &pid)
			if pid > 0 {
				slog.Info("sending SIGTERM to daemon", "pid", pid)
				syscall.Kill(pid, syscall.SIGTERM)
				return
			}
		}
	}
	slog.Warn("daemon process not found — restart daemon manually")
}

// verifyChecksumsAttestation verifies the SLSA provenance of checksums.txt
// via the GitHub CLI's attestation verify command. The release workflow
// (release.yml) attests checksums.txt via actions/attest-build-provenance@v2
// (PILOT-120, PR #166). This closes the "attacker publishes matched fake
// binary + fake checksums.txt" gap — the attestation ties checksums.txt to
// the trusted CI workflow identity.
//
// This gate FAILS CLOSED: if the gh CLI is not on PATH, verification returns
// an error and the update does not proceed. The only way to skip attestation
// is to set Config.SkipAttestation explicitly — there is no implicit skip.
// This is deliberate: the prior "gh absent => pass" behaviour silently
// disabled the entire provenance gate on headless production hosts (install.sh
// never installs gh), collapsing auto-update integrity back to "anyone with
// GitHub repo-write access can ship a malicious release."
func (u *Updater) verifyChecksumsAttestation(checksumsPath string) error {
	if u.config.SkipAttestation {
		slog.Warn("SLSA attestation verification disabled (SkipAttestation=true) — checksums provenance is NOT verified")
		return nil
	}
	return verifyChecksumsAttestationFn(u.config.Repo, checksumsPath)
}

// verifyChecksumsAttestationFn is the attestation verification implementation
// used by the updater. Tests may replace it to avoid requiring a real GitHub
// repo with SLSA attestations; they restore realVerifyChecksumsAttestationFn
// to exercise the production path.
var verifyChecksumsAttestationFn = realVerifyChecksumsAttestationFn

// realVerifyChecksumsAttestationFn is the production attestation check. It
// fails closed when gh is absent: callers only reach this function when
// SkipAttestation is false, so a missing gh means provenance cannot be
// established and the update must be refused.
func realVerifyChecksumsAttestationFn(repo, checksumsPath string) error {
	ghPath, err := exec.LookPath("gh")
	if err != nil {
		return fmt.Errorf("gh CLI required for SLSA attestation verification; install gh (https://cli.github.com/) or set SkipAttestation to bypass: %w", err)
	}

	cmd := exec.Command(ghPath, "attestation", "verify", checksumsPath, "--repo", repo)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh attestation verify: %s: %w", strings.TrimSpace(string(output)), err)
	}
	slog.Info("checksums provenance verified via SLSA attestation")
	return nil
}
