// SPDX-License-Identifier: AGPL-3.0-or-later

package updater

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// githubAPIHost is the only host a GitHub token is ever sent to. Release
// assets are served from other hosts (objects.githubusercontent.com via
// redirects) and never see the token.
const githubAPIHost = "api.github.com"

// doGitHubAPI sends req to the GitHub REST API. When GITHUB_TOKEN (or
// GH_TOKEN) is set and the request goes to api.github.com, the token is sent
// to lift the unauthenticated limit of 60 requests/hour per IP, which a
// shared NAT or VPN egress exhausts (the updater then stops updating). The
// releases and attestations endpoints are public, so a token is never
// required; if GitHub rejects the token (401: expired, revoked, mistyped) the
// request is retried once without it, so a stale token cannot stop updates.
func doGitHubAPI(client *http.Client, req *http.Request) (*http.Response, error) {
	tok := githubToken()
	if tok == "" || req.URL.Hostname() != githubAPIHost {
		return client.Do(req)
	}
	authed := req.Clone(req.Context())
	authed.Header.Set("Authorization", "Bearer "+tok)
	resp, err := client.Do(authed)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	slog.Warn("GitHub rejected GITHUB_TOKEN/GH_TOKEN (HTTP 401); retrying without it — fix or unset the token")
	return client.Do(req)
}

// githubAPIError builds the error for a non-200 GitHub API response. The
// message always starts with "<what> returned <status>". A rate-limit
// response additionally says so plainly, when the limit resets and how to
// raise it, so the failure is actionable when it surfaces in
// `pilotctl update` or the status file.
func githubAPIError(what string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	msg := strings.TrimSpace(string(body))
	if isGitHubRateLimit(resp, msg) {
		detail := "rate limit exceeded"
		if reset := rateLimitReset(resp); !reset.IsZero() {
			detail += ", resets at " + reset.UTC().Format(time.RFC3339)
		}
		if githubToken() == "" {
			detail += "; unauthenticated requests are limited to 60/hour per IP — set GITHUB_TOKEN in the updater's environment to raise the limit"
		}
		return fmt.Errorf("%s returned %d (%s): %s", what, resp.StatusCode, detail, msg)
	}
	return fmt.Errorf("%s returned %d: %s", what, resp.StatusCode, msg)
}

// isGitHubRateLimit reports whether resp is GitHub throttling the caller
// (primary limit: 403/429 with X-RateLimit-Remaining: 0; secondary limit:
// 403/429 whose body mentions a rate limit).
func isGitHubRateLimit(resp *http.Response, body string) bool {
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return false
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		return true
	}
	b := strings.ToLower(body)
	return strings.Contains(b, "rate limit") || strings.Contains(b, "abuse detection")
}

// rateLimitReset returns when the primary rate limit resets, or the zero time
// when GitHub did not say.
func rateLimitReset(resp *http.Response) time.Time {
	v := resp.Header.Get("X-RateLimit-Reset")
	if v == "" {
		return time.Time{}
	}
	secs, err := strconv.ParseInt(v, 10, 64)
	if err != nil || secs <= 0 {
		return time.Time{}
	}
	return time.Unix(secs, 0)
}
