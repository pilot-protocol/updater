# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `update-state.json` status file written after every check (`Config.StatusPath`,
  default: next to `StatePath`). It records the last check time, result and
  error, the run of consecutive failures, the installed and latest versions,
  the last update, any daemon restart failure, and a heartbeat for the loop
  (`loop_pid`, `next_check_at`). Read it with `ReadStatus`. `LastStatus()`
  returns the same record in-process.
- `GITHUB_TOKEN` / `GH_TOKEN` is now used for the releases API as well as for
  attestations. It is sent only to `api.github.com`, and the call is retried
  without it if GitHub rejects it with a 401. Rate-limit errors say when the
  limit resets and how to raise it.
- Downloads resume with HTTP `Range` after a dropped or stalled transfer (up to
  4 attempts, 5xx responses retried) and are aborted by `Stop()`.

### Changed

- **`RunOnce()` now returns `error`.** Callers such as `pilotctl update` can
  report a failed update and exit non-zero instead of printing "ok". Existing
  call sites that ignore the result still compile.
- `RunOnce()` no longer exits the calling process when a release replaces
  `pilot-updater`. It restarts the daemon and returns. The loop (`Start`)
  still exits for its service manager, after recording the update.
- Asset downloads have no fixed 30 s total timeout. They fail after 60 s
  without data, or after 30 minutes in total. The TLS handshake timeout is
  now 20 s.
- `go` directive raised to 1.25.13 (standard-library security fixes).

### Fixed

- On Linux the daemon is now restarted after an update. The running daemon's
  `/proc/<pid>/exe` reads `<path> (deleted)` once its binary is renamed over,
  so the old exact-path match never found it and daemons kept running the old
  version.
- The test suite no longer runs the real `launchctl`. On a developer Mac with
  Pilot installed, it used to restart the running daemon.

## [v0.1.0]

Initial release.
