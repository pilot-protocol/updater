// SPDX-License-Identifier: AGPL-3.0-or-later

package updater

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// These tests set GITHUB_TOKEN/GH_TOKEN with t.Setenv, so none of them may
// run in parallel.

func newAPIUpdater(srv *httptest.Server) *Updater {
	return &Updater{
		config: Config{Repo: "owner/repo", Version: "vTEST"},
		client: newRewriteClient(srv), // requests keep Host api.github.com
		stopCh: make(chan struct{}),
	}
}

// TestFetchRelease_SendsGitHubToken: the releases call used to be
// unauthenticated even when a token was available, so a shared NAT/VPN
// egress hit the 60/hour limit and updates silently stopped.
func TestFetchRelease_SendsGitHubToken(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "tok-123")
	var auth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.Store(r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(GitHubRelease{TagName: "v1.0.0"})
	}))
	defer srv.Close()

	if _, err := newAPIUpdater(srv).fetchLatestRelease(); err != nil {
		t.Fatalf("fetchLatestRelease: %v", err)
	}
	if got := auth.Load(); got != "Bearer tok-123" {
		t.Errorf("Authorization = %q, want Bearer tok-123", got)
	}
}

// TestFetchRelease_GHTokenAlias: gh's GH_TOKEN works too.
func TestFetchRelease_GHTokenAlias(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "gh-tok")
	var auth atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.Store(r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(GitHubRelease{TagName: "v1.0.0"})
	}))
	defer srv.Close()

	if _, err := newAPIUpdater(srv).fetchReleaseByTag("v1.0.0"); err != nil {
		t.Fatalf("fetchReleaseByTag: %v", err)
	}
	if got := auth.Load(); got != "Bearer gh-tok" {
		t.Errorf("Authorization = %q, want Bearer gh-tok", got)
	}
}

// TestFetchRelease_NoTokenNoHeader: without a token nothing is sent.
func TestFetchRelease_NoTokenNoHeader(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	var auth atomic.Value
	auth.Store("unset")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.Store(r.Header.Get("Authorization"))
		_ = json.NewEncoder(w).Encode(GitHubRelease{TagName: "v1.0.0"})
	}))
	defer srv.Close()

	if _, err := newAPIUpdater(srv).fetchLatestRelease(); err != nil {
		t.Fatalf("fetchLatestRelease: %v", err)
	}
	if got := auth.Load(); got != "" {
		t.Errorf("Authorization = %q, want none", got)
	}
}

// TestFetchRelease_RejectedTokenFallsBack: an expired or revoked token must
// not block updates of a public repo; the call is retried without it.
func TestFetchRelease_RejectedTokenFallsBack(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "revoked")
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != "" {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(GitHubRelease{TagName: "v1.2.3"})
	}))
	defer srv.Close()

	rel, err := newAPIUpdater(srv).fetchLatestRelease()
	if err != nil {
		t.Fatalf("fetchLatestRelease with a rejected token: %v", err)
	}
	if rel.TagName != "v1.2.3" || hits.Load() != 2 {
		t.Errorf("tag=%q hits=%d, want v1.2.3 after 2 requests", rel.TagName, hits.Load())
	}
}

// TestGitHubToken_NeverSentToOtherHosts: the token goes to api.github.com
// only — not to a mirror, a test server or an asset host.
func TestGitHubToken_NeverSentToOtherHosts(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "secret")
	var auth atomic.Value
	auth.Store("unset")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.Store(r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"attestations":[]}`))
	}))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/repos/o/r/attestations/sha256:00", nil)
	resp, err := doGitHubAPI(srv.Client(), req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := auth.Load(); got != "" {
		t.Errorf("token leaked to %s: Authorization = %q", srv.URL, got)
	}
}

// TestFetchRelease_RateLimitErrorIsActionable: the 403 says it is a rate
// limit, when it resets, and how to raise it.
func TestFetchRelease_RateLimitErrorIsActionable(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	reset := time.Date(2026, 9, 24, 13, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		http.Error(w, `{"message":"API rate limit exceeded for 203.0.113.9."}`, http.StatusForbidden)
	}))
	defer srv.Close()

	_, err := newAPIUpdater(srv).fetchLatestRelease()
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"returned 403", "rate limit exceeded", "resets at 2026-09-24T13:00:00Z", "set GITHUB_TOKEN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}

	// With a token already set, don't tell the user to set one.
	t.Setenv("GITHUB_TOKEN", "tok")
	_, err = newAPIUpdater(srv).fetchLatestRelease()
	if err == nil || strings.Contains(err.Error(), "set GITHUB_TOKEN") {
		t.Errorf("with a token set, error = %v", err)
	}
}

// TestGitHubAPIError_SecondaryLimitAndPlainErrors covers the 429 "abuse
// detection" body and a non-rate-limit 403.
func TestGitHubAPIError_SecondaryLimitAndPlainErrors(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_TOKEN", "")
	mk := func(code int, body string) *http.Response {
		rec := httptest.NewRecorder()
		rec.WriteHeader(code)
		_, _ = rec.WriteString(body)
		return rec.Result()
	}
	err := githubAPIError("GitHub API", mk(http.StatusTooManyRequests, "You have triggered an abuse detection mechanism"))
	if !strings.Contains(err.Error(), "returned 429 (rate limit exceeded") {
		t.Errorf("429 secondary limit: %v", err)
	}
	err = githubAPIError("GitHub API", mk(http.StatusForbidden, "Resource not accessible"))
	if strings.Contains(err.Error(), "rate limit") || !strings.Contains(err.Error(), "returned 403: Resource not accessible") {
		t.Errorf("plain 403: %v", err)
	}
	err = githubAPIError("GitHub API", mk(http.StatusNotFound, "Not Found"))
	if err.Error() != "GitHub API returned 404: Not Found" {
		t.Errorf("404: %v", err)
	}
}
