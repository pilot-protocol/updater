# updater

[![ci](https://github.com/pilot-protocol/updater/actions/workflows/ci.yml/badge.svg)](https://github.com/pilot-protocol/updater/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/pilot-protocol/updater/branch/main/graph/badge.svg)](https://codecov.io/gh/pilot-protocol/updater)
[![License: AGPL-3.0](https://img.shields.io/badge/License-AGPL_v3-blue.svg)](https://www.gnu.org/licenses/agpl-3.0)

Auto-updater plugin for the Pilot Protocol daemon. Polls the GitHub
releases endpoint hourly, hot-swaps the daemon, pilotctl, and gateway
binaries when a newer SemVer tag appears, and exits so the supervisor
restarts the new copy.

## Install

```go
import "github.com/pilot-protocol/updater"
```

## Usage

```go
u := updater.New(updater.Config{
    Repo:          "pilot-protocol/pilotprotocol",
    InstallDir:    "/home/user/.pilot/bin",
    Version:       "v1.10.5",
    CheckInterval: 1 * time.Hour,
    StatePath:     "/home/user/.pilot/auto-update.json", // {"enabled": bool}
})
u.Start()
```

One-shot check, for `pilotctl update` and similar commands:

```go
u := updater.New(updater.Config{
    Repo:       "pilot-protocol/pilotprotocol",
    InstallDir: "/home/user/.pilot/bin",
    StatusPath: "/home/user/.pilot/update-state.json", // record the run
})
if err := u.RunOnce(); err != nil {
    // The check failed: report it and exit non-zero.
}
st := u.LastStatus() // st.LastResult is "up_to_date" or "updated"
if st.RestartError != "" {
    // Installed, but the daemon still runs the old version. The message
    // says how to restart it.
}
```

`RunOnce` returns the check's error. It never exits the calling process. If
the release replaced `pilot-updater`, the daemon is still restarted onto the
new binaries. On macOS the updater service (launchd job
`network.pilotprotocol.pilot-updater`) is then restarted onto the new
`pilot-updater` as well. On Linux a running updater service switches to its
new binary the next time systemd restarts it.

A `RunOnce` caller writes the status file only when it sets `StatusPath` (or
`StatePath`); otherwise only `LastStatus()` has the result. `pilotctl update`
(web4 `cmd/pilotctl/updates.go`, since updater v0.2.5) passes
`StatusPath: ~/.pilot/update-state.json` and exits non-zero when `RunOnce`
returns an error.

### Restarting the daemon

After installing new binaries the updater restarts the daemon onto them, but
only when a service manager will start it again:

- **macOS:** `launchctl kickstart -k gui/<uid>/network.pilotprotocol.pilot-daemon`,
  but only when that launchd job runs the daemon. The updater reads the job
  with `launchctl print` and kickstarts it only when it is running and its
  program is the `pilot-daemon` that was just replaced. launchd sends the
  daemon SIGTERM (its graceful shutdown stops the apps it runs) and starts
  the job again on the new binary, whatever its `KeepAlive` policy. The
  updater then waits up to 60 s for launchd to run a new process and for that
  process to answer over IPC (`info`, on the job's `-socket`) with the
  installed version. The old `com.vulturelabs.pilot-daemon` label and the
  `user/<uid>` domain are found too; under `sudo` the invoking user's
  (`SUDO_UID`) domain is used.
- **Linux:** SIGTERM, but only when systemd will start the daemon again. The
  daemon must run as the main program (`ExecStart=`) of a system service whose
  unit restarts it after a clean exit. pilot-daemon exits 0 on SIGTERM, so
  that means `Restart=always` or `on-success`. The updater finds the service
  from `/proc/<pid>/cgroup` and reads the unit file and its drop-ins. It does
  not run `systemctl`. After the SIGTERM it waits up to 30 s for a new daemon
  process on the new binary.

On macOS the daemon is left running the old version, with `restart_error`
saying how to restart it, when:

- no launchd job runs it: it was started by hand (`pilot-daemon ...`) while
  the job was not loaded, or the job is loaded but runs no process. The
  updater tells this from a daemon answering on the socket with an older
  version. When no daemon answers, it is not running and its next start uses
  the new binary, which is not an error;
- the job starts another `pilot-daemon` (a second installation), so a
  restart would not run the new release.

It is also reported when `launchctl kickstart` fails, when launchd does not
start the job again, or when the new daemon does not answer with the
installed version within 60 s.

When a one-shot `RunOnce` (`pilotctl update`) replaced `pilot-updater`, it
restarts the updater job `network.pilotprotocol.pilot-updater` the same way,
after the daemon and only when the job runs the replaced binary. It never
restarts itself: the loop (`Start`) exits instead, and launchd starts it
again (`KeepAlive`). The result goes to `updater_restart_error` /
`updater_restarted_at`.

On Linux the daemon is left running the old version, with `restart_error`
saying how to restart it, when:

- it was started with `pilotctl daemon start`, which is how containers, WSL
  and CI run it because install.sh sets up no service there;
- its unit has `Restart=on-failure`, as written by install.sh up to v1.9.0.
  Updates never rewrite the unit, and systemd does not restart a clean exit
  under that policy. Re-running install.sh rewrites the unit with
  `Restart=always`;
- it runs under a systemd user service, or inside another service, such as a
  CI runner.

Stopping the daemon in any of those cases would take the node offline.

### Pinning a version

Set `PinnedVersion` to lock the updater to a specific release tag. When
set, the updater fetches the exact release (via
`/releases/tags/{tag}`), applies it if it differs from the current
install, then idles — it will **not** chase the latest release. Clear
`PinnedVersion` (set to `""`) to resume auto-updating.

```go
u := updater.New(updater.Config{
    Repo:          "pilot-protocol/pilotprotocol",
    InstallDir:    "/home/user/.pilot/bin",
    PinnedVersion: "v1.10.5",  // stay on this version
})
u.Start()
```

The in-process `Service` adapter is used when embedding into the
daemon; a standalone sidecar binary built from this package is also
supported.

## Update status file

After every check the updater writes `update-state.json` so that other tools
can tell whether updates actually work. Before this file existed,
`pilotctl update status` could only show whether auto-update was switched on,
and a node could fail every hourly check for weeks without anyone noticing.

The file goes to `Config.StatusPath`. When that is empty and `StatePath` is
set, it goes next to the control file. For a standard install that is
`~/.pilot/auto-update.json` → `~/.pilot/update-state.json`. With neither
set, nothing is written.

The long-running updater and any one-shot `RunOnce` caller configured with
the same path share the file, as the updater service and `pilotctl update`
do. Each writer takes an advisory lock on `update-state.json.lock` and
merges its fields into what is already there, so concurrent writers from
different processes do not lose each other's changes. Writes are atomic
(temp file + rename). A writer that cannot get the lock within 5 s writes
anyway and logs a warning.

```json
{
  "updater_version": "v1.13.10",
  "repo": "pilot-protocol/pilotprotocol",
  "last_check_at": "2026-09-24T10:00:12Z",
  "last_check_trigger": "auto",
  "last_result": "failed",
  "last_error": "fetch latest release: GitHub API returned 403 (rate limit exceeded, resets at 2026-09-24T10:41:03Z; ...)",
  "consecutive_failures": 3,
  "last_success_at": "2026-09-24T07:00:09Z",
  "current_version": "v1.13.9",
  "latest_version": "v1.13.10",
  "last_update_at": "2026-09-01T12:35:40Z",
  "last_update_version": "v1.13.9",
  "daemon_restarted_at": "2026-09-01T12:35:47Z",
  "daemon_restarted_by": "launchd gui/501/network.pilotprotocol.pilot-daemon",
  "daemon_restarted_version": "v1.13.9",
  "loop_pid": 812,
  "loop_started_at": "2026-09-20T08:11:02Z",
  "next_check_at": "2026-09-24T11:00:12Z"
}
```

| Field | Meaning |
|---|---|
| `last_result` | `up_to_date`, `updated` or `failed`. |
| `last_error` | Why the last check failed. Empty after a success. |
| `consecutive_failures` | Failed checks since the last success. |
| `last_success_at` | The last check that completed without error. |
| `current_version` / `latest_version` | The installed release, and the release it was compared against (the pin, when pinned). |
| `restart_error` | New binaries were installed but the daemon is not running them. Either it was left on the old version because stopping it would have left it down (see [Restarting the daemon](#restarting-the-daemon)) or the restart failed, or it was stopped and did not come back. Says how to restart it. Cleared by the next successful restart, and by the next check that finds the daemon running the installed release (Linux: from `/proc`; macOS: the daemon's IPC `info` version). |
| `daemon_restarted_at`, `daemon_restarted_by`, `daemon_restarted_version` | The last restart the updater made that brought the daemon back on new binaries, the service manager and job that did it, and (macOS) the version the daemon then reported over IPC. |
| `updater_restart_error`, `updater_restarted_at` | macOS only: a one-shot update replaced `pilot-updater` and could not (error) or did (time) restart the updater service's launchd job onto it. |
| `loop_pid`, `loop_started_at` | The long-running updater process. |
| `next_check_at` | When the loop wakes next. It is updated even while auto-update is disabled, so it doubles as a heartbeat. |

Suggested readings for `pilotctl update status` and similar tools:

- **Updates failing:** `consecutive_failures > 0`. Show `last_error`.
- **Updates stalled:** `last_success_at` is more than 48 hours old while auto-update is enabled.
- **No updater running:** auto-update is enabled, but the file is missing, `loop_pid` is not alive, or `next_check_at` is more than one interval in the past. This happens on Linux without systemd (containers, WSL, CI), where the installer cannot start the updater.
- **Daemon on the old version:** `restart_error` is set. Show it: it names the command that restarts the daemon.

Use `updater.ReadStatus(path)` to read the file. A missing file returns an
error matching `errors.Is(err, fs.ErrNotExist)`.

## GitHub API rate limits and `GITHUB_TOKEN`

The updater calls the GitHub REST API for the release list and for the build
attestation. Without a token GitHub allows 60 requests per hour per IP, and a
shared NAT, VPN or CI egress can use that up. When that happens the
check fails with an error that says so, and it is recorded in the status file.

If `GITHUB_TOKEN` (or `GH_TOKEN`) is set in the updater's environment, it is
sent to `api.github.com`, and only there, which raises the limit to 5,000 per
hour. The token needs no scopes because the repository is public: a classic
token with no scopes, or a fine-grained token with public read-only access,
is enough. If GitHub rejects the token (401), the updater retries without it,
so an expired token never stops updates.

To set it for the service:

- **systemd:** `sudo systemctl edit pilot-updater`, add
  `[Service]` / `Environment=GITHUB_TOKEN=<token>`, then
  `sudo systemctl restart pilot-updater`.
- **launchd:** add an `EnvironmentVariables` dict with `GITHUB_TOKEN` to
  `~/Library/LaunchAgents/network.pilotprotocol.pilot-updater.plist`, then
  reload it with `launchctl bootout` / `launchctl bootstrap`.

## Downloads

Release archives are 17–20 MB. Downloads no longer have a fixed total
timeout (the old 30 s limit needed a sustained ~5 Mbit/s). Instead:

- An attempt fails only when no data arrives for 60 s.
- A dropped or stalled transfer resumes with an HTTP `Range` request from the
  bytes already on disk, or starts over if the server ignores `Range`. Up to
  4 attempts are made. 5xx responses are retried and 4xx responses are not.
- One file download is capped at 30 minutes in total.
- `Stop()` aborts an in-flight download.

The SHA-256 match against the attested `checksums.txt` still gates every install.

## No external tools (no `gh`)

The updater never runs `gh` or any other external tool. It checks the SLSA
provenance of `checksums.txt` in-process with sigstore-go
(`attestation.go`). The only command it runs is `launchctl` (`print` and
`kickstart`), which reads and restarts the daemon and updater jobs under
launchd on macOS. It asks the daemon for its version over its IPC socket
directly. On Linux it reads `/proc` and the systemd unit files directly
instead of running `systemctl`.
`TestNoExternalToolDependency` fails the build if another exec is added.

### Nodes stuck on v1.12.2–v1.13.4 (updater v0.2.3)

pilotprotocol releases **v1.12.2 through v1.13.4** shipped updater v0.2.3,
which ran `gh attestation verify` and refused every update when `gh` was not
on its `PATH`. Service managers never put `gh` on the `PATH`: launchd's default is
`/usr/bin:/bin:/usr/sbin:/sbin`, and most Linux hosts have no `gh` at all. So
those nodes cannot update themselves. The updater log shows an hourly
`gh CLI required for SLSA attestation verification` error. On macOS the log
is `~/.pilot/updater.log`. On Linux, read it with `journalctl -u pilot-updater`.

The broken updater cannot fix itself. On an affected node, do one of these:

1. **Re-run the installer** (recommended). It installs the current binaries,
   including a gh-free updater, and restarts the services:

   ```sh
   curl -fsSL https://pilotprotocol.network/install.sh | sh
   ```

2. **Or** run `pilotctl update` once from an interactive shell where `gh` is
   installed and logged in (`gh auth status`). That one-shot update uses your
   shell's `PATH`, installs the current release and replaces `pilot-updater`
   with a gh-free version. The old `pilotctl update` exits silently after
   doing so. Then restart both services so they run the new binaries:
   `sudo systemctl restart pilot-updater pilot-daemon` on Linux, or on macOS
   `launchctl kickstart -k gui/$(id -u)/network.pilotprotocol.pilot-updater`
   and the same command for `network.pilotprotocol.pilot-daemon`.

Check the result with `pilotctl version` and `~/.pilot/bin/pilot-updater --version`.
Do **not** work around the problem with `PILOT_UPDATER_SKIP_ATTESTATION`,
because that switches off provenance checks.

## Layout

| File | What it does |
|---|---|
| `updater.go` | `Updater`: check loop, `RunOnce`, release polling, install, daemon restart. |
| `supervisor.go` | Linux restart safety: finds the daemon's systemd service and checks its restart policy before any SIGTERM. |
| `launchd.go` | macOS restart: finds the daemon and updater launchd jobs (`launchctl print`), kickstarts them and confirms the daemon's version over IPC. |
| `download.go` | Resumable asset downloads with idle and total-duration limits. |
| `github.go` | GitHub API calls: optional `GITHUB_TOKEN`, rate-limit errors. |
| `attestation.go` | In-process SLSA provenance verification of `checksums.txt` (sigstore-go). |
| `status.go` | `Status` / `update-state.json` persistence and `ReadStatus`. |
| `version.go` | SemVer parsing and comparison. |
| `service.go` | `*Service` — `coreapi.Service` adapter. Build tag `!no_updater`. |
| `service_disabled.go` | Stub when build tag `no_updater` is set. |

## Build tags

| Tag | Effect |
|---|---|
| `no_updater` | Compiles a stub whose `Start` is a no-op. |

## License

AGPL-3.0-or-later. See [LICENSE](LICENSE).
