// SPDX-License-Identifier: AGPL-3.0-or-later

package updater

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	sgbundle "github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// githubActionsOIDCIssuer is the OIDC issuer that signs the ephemeral Fulcio
// certificates used by GitHub Actions build-provenance attestations. It is a
// fixed, well-known value; requiring it prevents accepting an attestation
// signed under some other identity provider.
const githubActionsOIDCIssuer = "https://token.actions.githubusercontent.com"

// attestationAPIBase is the GitHub REST endpoint that returns the SLSA
// attestation bundles registered for an artifact digest under a repository.
// GitHub only serves attestations it recorded for THAT repository, so the
// TLS-authenticated response is itself a first trust anchor; the sigstore
// bundle verification below is the cryptographic one. Declared as a var so
// tests can point it at an httptest.Server.
var attestationAPIBase = "https://api.github.com"

// trustedRootOnce caches the sigstore public-good trusted root for the process
// lifetime. Fetching it hits the Sigstore TUF repository (cached on disk by the
// TUF client); we additionally memoise it in-process so a busy update loop does
// not re-fetch on every tick.
var (
	trustedRootOnce sync.Once
	trustedRoot     root.TrustedMaterial
	trustedRootErr  error
)

// githubToken returns a GitHub API token from the environment, if any. Optional:
// used only to raise the API rate limit (see fetchAttestations). GITHUB_TOKEN is
// the conventional name; GH_TOKEN is gh's alias.
func githubToken() string {
	for _, k := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

// githubAttestationsResponse is the subset of the GitHub attestations API
// response we consume. Each attestation carries a full sigstore bundle.
type githubAttestationsResponse struct {
	Attestations []struct {
		Bundle json.RawMessage `json:"bundle"`
	} `json:"attestations"`
}

// getTrustedRoot returns (and memoises) the sigstore public-good trusted root.
func getTrustedRoot() (root.TrustedMaterial, error) {
	trustedRootOnce.Do(func() {
		trustedRoot, trustedRootErr = root.FetchTrustedRootWithOptions(tuf.DefaultOptions())
	})
	return trustedRoot, trustedRootErr
}

// sha256Hex returns the lowercase hex SHA-256 of the file at path.
func sha256Hex(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// fetchAttestations retrieves the SLSA attestation bundles GitHub holds for the
// given repository + artifact digest. Unauthenticated: the endpoint is public
// for public repositories and requires no gh CLI or token.
func fetchAttestations(client *http.Client, repo, digestHex string) ([]json.RawMessage, error) {
	url := fmt.Sprintf("%s/repos/%s/attestations/sha256:%s", attestationAPIBase, repo, digestHex)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	// No token is required — the endpoint is public for public repositories.
	// If one is present in the environment we send it purely to lift the
	// 60-req/hr unauthenticated rate limit (5000/hr authenticated), so a busy
	// fleet does not intermittently fail closed on a 403 rate-limit. Absence of
	// a token never changes correctness, only the rate ceiling.
	if tok := githubToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch attestations: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("GitHub attestations API returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed githubAttestationsResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxDownloadBytes)).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode attestations response: %w", err)
	}
	bundles := make([]json.RawMessage, 0, len(parsed.Attestations))
	for _, a := range parsed.Attestations {
		if len(a.Bundle) > 0 {
			bundles = append(bundles, a.Bundle)
		}
	}
	return bundles, nil
}

// expectedSANPattern builds a regexp that a valid Fulcio certificate SAN must
// match. It binds the attestation to BOTH the exact repository and the exact
// release tag:
//
//	https://github.com/<owner>/<repo>/.github/workflows/<any>.yml@refs/tags/<tag>
//
// Pinning "@refs/tags/<tag>" is what closes the validated-rollback gap
// (finding C): an attacker who reattaches an older, still-validly-attested
// checksums.txt under a NEW release tag fails here, because that old
// attestation's certificate SAN carries the OLD tag's ref, not the tag we are
// installing. The workflow filename is left open (any file under
// .github/workflows/) so a routine rename of release.yml does not brick
// updates, while repo + tag remain cryptographically bound.
func expectedSANPattern(repo, tag string) string {
	return fmt.Sprintf(`^https://github\.com/%s/\.github/workflows/[^@]+@refs/tags/%s$`,
		regexp.QuoteMeta(repo), regexp.QuoteMeta(tag))
}

// realVerifyChecksumsAttestationFn is the production attestation check. It is
// gh-free: it fetches the SLSA provenance bundle straight from the GitHub
// attestations REST API and verifies it in-process with sigstore-go — the same
// verification library the gh CLI uses under the hood. Verification is FULL and
// cryptographic:
//
//  1. The DSSE signature chains to the Sigstore public-good trust root
//     (Fulcio CA + Rekor transparency log + timestamp), so the bundle must have
//     been produced by a genuine Sigstore signing flow.
//  2. The in-toto subject must include checksums.txt with a digest equal to the
//     SHA-256 of the file we downloaded (WithArtifactDigest).
//  3. The signing certificate identity must be a GitHub Actions OIDC identity
//     (issuer pinned) whose SAN names THIS repository and THIS release tag
//     (expectedSANPattern) — binding provenance to the exact release.
//
// Any failure returns an error and the update is refused. There is no implicit
// skip: bypass requires Config.SkipAttestation, handled by the caller.
func realVerifyChecksumsAttestationFn(repo, tag, checksumsPath string) error {
	if repo == "" {
		return fmt.Errorf("repo is required for attestation verification")
	}
	if tag == "" {
		return fmt.Errorf("release tag is required to bind the attestation to this release")
	}

	digestHex, err := sha256Hex(checksumsPath)
	if err != nil {
		return fmt.Errorf("hash checksums.txt: %w", err)
	}
	digestBytes, err := hex.DecodeString(digestHex)
	if err != nil {
		return fmt.Errorf("decode digest: %w", err)
	}

	trusted, err := getTrustedRoot()
	if err != nil {
		return fmt.Errorf("load sigstore trusted root: %w", err)
	}

	sev, err := verify.NewVerifier(trusted,
		verify.WithTransparencyLog(1),
		verify.WithObserverTimestamps(1),
	)
	if err != nil {
		return fmt.Errorf("construct verifier: %w", err)
	}

	certID, err := verify.NewShortCertificateIdentity(
		githubActionsOIDCIssuer, "", "", expectedSANPattern(repo, tag))
	if err != nil {
		return fmt.Errorf("build certificate identity policy: %w", err)
	}
	policy := verify.NewPolicy(
		verify.WithArtifactDigest("sha256", digestBytes),
		verify.WithCertificateIdentity(certID),
	)

	client := &http.Client{Timeout: 30 * time.Second}
	rawBundles, err := fetchAttestations(client, repo, digestHex)
	if err != nil {
		return err
	}
	if len(rawBundles) == 0 {
		return fmt.Errorf("no SLSA attestation found for checksums.txt (sha256:%s) under %s", digestHex, repo)
	}

	// Accept if ANY returned attestation verifies against the policy. Collect
	// errors so a total failure explains why.
	var errs []string
	for i, raw := range rawBundles {
		var b sgbundle.Bundle
		if err := b.UnmarshalJSON(raw); err != nil {
			errs = append(errs, fmt.Sprintf("bundle %d: parse: %v", i, err))
			continue
		}
		if _, err := sev.Verify(&b, policy); err != nil {
			errs = append(errs, fmt.Sprintf("bundle %d: verify: %v", i, err))
			continue
		}
		slog.Info("checksums provenance verified via SLSA attestation",
			"repo", repo, "tag", tag, "digest", "sha256:"+digestHex)
		return nil
	}
	return fmt.Errorf("no attestation for checksums.txt satisfied the repo+tag identity policy for %s@%s: %s",
		repo, tag, strings.Join(errs, "; "))
}
