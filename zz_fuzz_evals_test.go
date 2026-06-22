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

// FuzzVerifyChecksum exercises the checksums.txt line parser inside
// VerifyChecksum against arbitrary, attacker-controlled checksums file
// contents. The parser walks untrusted release-asset bytes; a panic
// there would be a denial-of-service (and any "accidentally matched"
// path would be a verification bypass). The invariants asserted:
//
//   - VerifyChecksum never panics, regardless of input (malformed
//     lines, embedded NULs, giant fields, CR/LF noise, no trailing
//     newline, duplicate filenames, etc.).
//   - It only ever returns nil when the parsed expected hash equals the
//     SHA256 of the archive on disk. Any malformed/short/garbage line
//     for our archive name must be rejected (non-nil error) — never a
//     silent pass.
//
// The fuzzer cannot produce a real preimage for a fixed archive, so the
// only legitimate success path is the deterministic correct-line seed.
func FuzzVerifyChecksum(f *testing.F) {
	dir := f.TempDir()
	archiveName := fmt.Sprintf("pilot-%s-%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	archivePath := filepath.Join(dir, archiveName)
	if err := os.WriteFile(archivePath, []byte("fuzz-archive-bytes"), 0644); err != nil {
		f.Fatal(err)
	}
	sum := sha256.Sum256([]byte("fuzz-archive-bytes"))
	correctHash := fmt.Sprintf("%x", sum)
	correctLine := fmt.Sprintf("%s  %s\n", correctHash, archiveName)

	// Seed corpus: the valid line plus a spread of adversarial shapes.
	f.Add(correctLine)
	f.Add("")
	f.Add("\n\n\n")
	f.Add("deadbeef")                                       // single field, no filename
	f.Add(correctHash)                                      // hash only, no filename
	f.Add(correctHash + " " + archiveName)                  // single-space variant
	f.Add(correctHash + "\t" + archiveName)                 // tab separator
	f.Add("   " + correctHash + "   " + archiveName + "  ") // surrounding whitespace
	f.Add(strings.Repeat("a", 64) + "  " + archiveName)     // wrong hash, right name
	f.Add(correctHash + "  other-file.tar.gz")              // right hash, wrong name
	f.Add("# comment line\n" + correctLine)                 // comment then valid
	f.Add(correctLine + "garbage trailing line")            // valid then garbage
	f.Add(correctHash + "  " + archiveName + "\x00evil")    // embedded NUL
	f.Add(strings.Repeat(correctLine, 1000))                // many duplicate lines
	f.Add(strings.Repeat("x ", 100000))                     // very long single line

	f.Fuzz(func(t *testing.T, checksums string) {
		checksumsPath := filepath.Join(t.TempDir(), "checksums.txt")
		if err := os.WriteFile(checksumsPath, []byte(checksums), 0644); err != nil {
			t.Skip() // path/fs hiccup, not a parser property
		}

		// Must never panic.
		err := VerifyChecksum(archivePath, archiveName, checksumsPath)

		// If it returned nil, the file MUST have contained a line that
		// parsed to exactly the correct hash for our archive name.
		// Verify that independently — a nil return on any other input is
		// a verification bypass.
		if err == nil {
			if !checksumsLineMatches(checksums, archiveName, correctHash) {
				t.Fatalf("VerifyChecksum accepted input with no valid line for %q:\n%q", archiveName, checksums)
			}
		}
	})
}

// checksumsLineMatches is an independent re-implementation of the
// accept condition, used only by the fuzzer to cross-check that a nil
// return corresponds to a genuinely correct line. Kept deliberately
// simple and separate from the production parser.
func checksumsLineMatches(checksums, archiveName, correctHash string) bool {
	for _, line := range strings.Split(checksums, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) >= 2 && parts[1] == archiveName {
			return parts[0] == correctHash
		}
	}
	return false
}

// TestApplyUpdate_TamperedArchiveRefused is eval (e): a well-formed
// release whose checksums.txt is internally consistent with the
// ORIGINAL archive, but where the archive bytes served to the client
// have been swapped (MITM / compromised CDN object). The SHA256 of the
// delivered bytes no longer matches the attested checksum, so the
// update must be REFUSED and nothing installed — distinct from
// TestApplyUpdate_ChecksumMismatch in that the checksums file here is a
// real, valid checksums file for a different (legitimate) artifact.
func TestApplyUpdate_TamperedArchiveRefused(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	installDir := filepath.Join(tmpDir, "bin")
	os.MkdirAll(installDir, 0755)
	os.WriteFile(filepath.Join(installDir, ".pilot-version"), []byte("v1.0.0\n"), 0644)
	// Pre-seed a daemon binary so we can prove it is left untouched.
	sentinel := []byte("ORIGINAL-DAEMON-DO-NOT-OVERWRITE")
	os.WriteFile(filepath.Join(installDir, "pilot-daemon"), sentinel, 0755)

	archiveName := fmt.Sprintf("pilot-%s-%s.tar.gz", runtime.GOOS, runtime.GOARCH)

	// Build the LEGITIMATE archive and compute its real checksum — this
	// is what the publisher signed/attested.
	legitDir := t.TempDir()
	legitPath := filepath.Join(legitDir, archiveName)
	createTestTarGz(t, legitPath, map[string]string{"daemon": "legit-daemon-content"})
	legitBytes, _ := os.ReadFile(legitPath)
	legitHash := sha256.Sum256(legitBytes)
	checksumsContent := fmt.Sprintf("%x  %s\n", legitHash, archiveName)

	// Build a DIFFERENT (tampered) archive the attacker actually serves.
	// Same filename, malicious payload, different bytes => different hash.
	tamperedDir := t.TempDir()
	tamperedPath := filepath.Join(tamperedDir, archiveName)
	createTestTarGz(t, tamperedPath, map[string]string{"daemon": "MALICIOUS-PAYLOAD"})
	tamperedBytes, _ := os.ReadFile(tamperedPath)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/download/" + archiveName:
			w.Write(tamperedBytes) // serve tampered bytes
		case "/download/checksums.txt":
			w.Write([]byte(checksumsContent)) // valid checksums for the LEGIT archive
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	u := &Updater{
		config: Config{InstallDir: installDir, SkipAttestation: true},
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
		t.Fatal("tampered archive (bytes swapped, checksums valid for original) must be refused")
	}
	if !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("expected checksum verification failure, got: %v", err)
	}
	// The on-disk daemon must be untouched.
	got, _ := os.ReadFile(filepath.Join(installDir, "pilot-daemon"))
	if string(got) != string(sentinel) {
		t.Fatalf("daemon binary was overwritten despite tamper detection; got %q", got)
	}
}
