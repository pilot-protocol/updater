// SPDX-License-Identifier: AGPL-3.0-or-later

package updater

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

// TestExpectedSANPattern_BindsRepoAndTag verifies the certificate-identity
// regexp binds BOTH the repository and the exact release tag, and rejects a
// mismatched tag (the validated-rollback defense, finding C).
func TestExpectedSANPattern_BindsRepoAndTag(t *testing.T) {
	t.Parallel()

	repo := "pilot-protocol/pilotprotocol"
	pat := expectedSANPattern(repo, "v1.12.5")
	re := regexp.MustCompile(pat)

	// The real SAN GitHub Actions stamps into the Fulcio cert for v1.12.5.
	good := "https://github.com/pilot-protocol/pilotprotocol/.github/workflows/release.yml@refs/tags/v1.12.5"
	if !re.MatchString(good) {
		t.Errorf("pattern %q should match the real SAN %q", pat, good)
	}

	// A different, older tag must NOT match — this is what stops an attacker
	// reattaching an older attested checksums.txt under a new release.
	rollback := "https://github.com/pilot-protocol/pilotprotocol/.github/workflows/release.yml@refs/tags/v1.11.0"
	if re.MatchString(rollback) {
		t.Errorf("pattern %q must NOT match a different tag %q (validated rollback)", pat, rollback)
	}

	// A different repository must NOT match.
	otherRepo := "https://github.com/evil/pilotprotocol/.github/workflows/release.yml@refs/tags/v1.12.5"
	if re.MatchString(otherRepo) {
		t.Errorf("pattern %q must NOT match a foreign repo %q", pat, otherRepo)
	}

	// A non-workflow OIDC identity (e.g. a branch push, or a non-workflow
	// path) must NOT match.
	branch := "https://github.com/pilot-protocol/pilotprotocol/.github/workflows/release.yml@refs/heads/main"
	if re.MatchString(branch) {
		t.Errorf("pattern %q must NOT match a branch ref %q", pat, branch)
	}
}

// TestExpectedSANPattern_RegexSafe verifies tags/repos with regex metacharacters
// are quoted so the pattern can't be subverted or fail to compile.
func TestExpectedSANPattern_RegexSafe(t *testing.T) {
	t.Parallel()
	pat := expectedSANPattern("owner/re.po", "v1.2.3+build")
	re, err := regexp.Compile(pat)
	if err != nil {
		t.Fatalf("pattern must compile: %v", err)
	}
	// The literal dot in the repo must not act as a wildcard.
	if re.MatchString("https://github.com/owner/reXpo/.github/workflows/x.yml@refs/tags/v1.2.3+build") {
		t.Error("unescaped '.' in repo acted as a wildcard")
	}
}

// TestSha256Hex verifies the file hash helper.
func TestSha256Hex(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	p := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(p, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := sha256Hex(p)
	if err != nil {
		t.Fatal(err)
	}
	// echo hello | sha256sum
	want := "5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03"
	if got != want {
		t.Errorf("sha256Hex = %s, want %s", got, want)
	}
	if _, err := sha256Hex(filepath.Join(dir, "missing")); err == nil {
		t.Error("expected error hashing a missing file")
	}
}

// TestFetchAttestations_ParsesBundles verifies bundle extraction from the
// GitHub attestations API shape, using a mock server via attestationAPIBase.
func TestFetchAttestations_ParsesBundles(t *testing.T) {
	origBase := attestationAPIBase
	defer func() { attestationAPIBase = origBase }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantPath := "/repos/o/r/attestations/sha256:deadbeef"
		if r.URL.Path != wantPath {
			t.Errorf("path = %q, want %q", r.URL.Path, wantPath)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"attestations":[{"bundle":{"mediaType":"x","a":1}},{"bundle":{"b":2}}]}`)
	}))
	defer srv.Close()
	attestationAPIBase = srv.URL

	bundles, err := fetchAttestations(srv.Client(), "o/r", "deadbeef")
	if err != nil {
		t.Fatal(err)
	}
	if len(bundles) != 2 {
		t.Fatalf("got %d bundles, want 2", len(bundles))
	}
}

// TestFetchAttestations_HTTPError verifies a non-200 response is an error
// (fail closed — we never treat a missing/failed attestation as "verified").
func TestFetchAttestations_HTTPError(t *testing.T) {
	origBase := attestationAPIBase
	defer func() { attestationAPIBase = origBase }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
	}))
	defer srv.Close()
	attestationAPIBase = srv.URL

	if _, err := fetchAttestations(srv.Client(), "o/r", "deadbeef"); err == nil {
		t.Fatal("expected error on 404, got nil")
	}
}

// TestRealVerify_NoAttestationsFailsClosed verifies the full production path
// refuses to verify when the API returns zero attestations for the digest —
// exercising the network path deterministically against a mock server.
func TestRealVerify_NoAttestationsFailsClosed(t *testing.T) {
	origBase := attestationAPIBase
	defer func() { attestationAPIBase = origBase }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"attestations":[]}`)
	}))
	defer srv.Close()
	attestationAPIBase = srv.URL

	dir := t.TempDir()
	cks := filepath.Join(dir, "checksums.txt")
	if err := os.WriteFile(cks, []byte("abc  pilot-linux-amd64.tar.gz\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := realVerifyChecksumsAttestationFn("o/r", "v1.0.0", cks)
	if err == nil {
		t.Fatal("expected fail-closed error when no attestations are returned")
	}
}

// TestRealVerify_EndToEnd_v1_12_5 is an integration test: it verifies the REAL
// checksums.txt attestation for the live v1.12.5 release, end-to-end, against
// the Sigstore public-good trust root. It proves the gh-free path actually
// verifies a genuine GitHub build-provenance attestation. Skipped in -short
// mode and when the network (GitHub API + Sigstore TUF) is unreachable.
func TestRealVerify_EndToEnd_v1_12_5(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping network integration test in -short mode")
	}
	if !networkUp() {
		t.Skip("network unreachable; skipping live attestation verification")
	}

	dir := t.TempDir()
	cks := filepath.Join(dir, "checksums.txt")
	url := "https://github.com/pilot-protocol/pilotprotocol/releases/download/v1.12.5/checksums.txt"
	if err := downloadTo(url, cks); err != nil {
		t.Skipf("could not download real checksums.txt (%v); skipping", err)
	}

	// Correct repo + tag → must verify. A 403 means the unauthenticated
	// GitHub API rate limit was hit (set GITHUB_TOKEN to avoid it); that's an
	// environmental condition, not a verification failure, so skip rather than
	// fail the build.
	if err := realVerifyChecksumsAttestationFn("pilot-protocol/pilotprotocol", "v1.12.5", cks); err != nil {
		if isRateLimited(err) {
			t.Skipf("GitHub API rate limited (set GITHUB_TOKEN to run): %v", err)
		}
		t.Fatalf("live v1.12.5 attestation should verify gh-free, got: %v", err)
	}

	// Wrong tag → must fail (validated-rollback defense binds the tag).
	if err := realVerifyChecksumsAttestationFn("pilot-protocol/pilotprotocol", "v1.11.0", cks); err == nil {
		t.Fatal("verification with a mismatched tag must fail (rollback defense)")
	}
}

func isRateLimited(err error) bool {
	return err != nil && (containsAny(err.Error(), "returned 403", "rate limit"))
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && len(s) >= len(sub) {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}

func networkUp() bool {
	c, err := net.DialTimeout("tcp", "api.github.com:443", 4*time.Second)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func downloadTo(url, dst string) error {
	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.ReadFrom(resp.Body)
	return err
}
