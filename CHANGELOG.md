# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

- **macOS: the daemon is restarted onto the new binaries only when launchd
  runs it, and the restart is confirmed.** Before, the updater ran `launchctl
  kickstart -k gui/<uid>/network.pilotprotocol.pilot-daemon` blindly and
  reported success as soon as launchctl returned. It now reads the job with
  `launchctl print` first and kickstarts it only when the job is running and
  its program is the `pilot-daemon` that was just replaced. It then waits (up
  to 60 s) for launchd to run a new process and for that process to answer
  over IPC (`info`) with the installed version. Otherwise the daemon is left
  running and `restart_error` says why and how to restart it: no launchd job
  runs it (started by hand), the job runs another binary, kickstart failed,
  launchd did not start the job again, or the new daemon did not answer with
  the installed version in time. A daemon that is not running is not an
  error: its next start uses the new binary. The old `com.vulturelabs.*`
  label and the `user/<uid>` domain are found too, and under `sudo` the
  invoking user's domain (`SUDO_UID`) is used.
- **macOS: a one-shot update (`pilotctl update`) that replaces
  `pilot-updater` restarts the updater service** (launchd job
  `network.pilotprotocol.pilot-updater`) after the daemon, so the service
  runs the new binary instead of the old one until the next login. It is
  restarted only when its program is the replaced binary, and never when the
  caller is the service itself. The result is recorded in
  `updater_restart_error` / `updater_restarted_at`.
- A one-shot update that replaced `pilot-updater` (every release does) now
  records the daemon restart it made. Before, `restart_error` from an earlier
  run was kept even though this run restarted the daemon.
- On macOS a check clears `restart_error` once the daemon answers over IPC
  with the installed version, as Linux already did from `/proc`. Before, it
  stayed until the next release. The daemon is only asked while a
  `restart_error` is on record.
- Writers of `update-state.json` keep the fields they do not define. The
  file is shared by `pilotctl update` and the updater service, which can run
  different updater versions. On a macOS runner the freshly restarted
  updater service (updater v0.2.5, from v1.13.10) rewrote the file 40 ms
  after `pilotctl update` and dropped `daemon_restarted_*` and
  `updater_restarted_at`. From this version on, an older writer passes newer
  fields through. v0.2.5 and earlier still drop them.

### Added

- `daemon_restarted_at`, `daemon_restarted_by` and `daemon_restarted_version`
  in `update-state.json`: when the updater last restarted the daemon onto new
  binaries and saw it come back, which service manager did it (`launchd
  gui/501/network.pilotprotocol.pilot-daemon`, `systemd pilot-daemon.service`)
  and, on macOS, the version the daemon reported.
- `updater_restart_error` and `updater_restarted_at` in `update-state.json`
  (see above).

### Changed

- The updater now connects to the daemon's IPC socket (the job's `-socket`
  argument, default `/tmp/pilot.sock`) to read its version on macOS. It still
  runs no external tool other than `launchctl`, now `print` as well as
  `kickstart`.

## [v0.2.5] - 2026-09-24

### Added

- `update-state.json` status file written after every check (`Config.StatusPath`,
  default: next to `StatePath`). It records the last check time, result and
  error, the run of consecutive failures, the installed and latest versions,
  the last update, any daemon restart failure, and a heartbeat for the loop
  (`loop_pid`, `next_check_at`). Read it with `ReadStatus`. `LastStatus()`
  returns the same record in-process. Writers in different processes
  serialise on an advisory lock (`update-state.json.lock`), so none of them
  lose each other's updates. A `RunOnce` caller writes the file only when it
  sets `StatusPath` or `StatePath`. `pilotctl update` does not do that yet.
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
- The updater sends that SIGTERM only when systemd will start the daemon
  again: the daemon runs as `ExecStart=` of a system service with
  `Restart=always` or `on-success`, read from `/proc/<pid>/cgroup` and the unit
  files. Otherwise the daemon keeps running and `restart_error` says how to
  restart it. This covers a daemon started with `pilotctl daemon start`
  (containers, WSL, CI) and units from install.sh up to v1.9.0, which have
  `Restart=on-failure`. Stopping those daemons would take the node offline
  while the status file reported `updated`. After a SIGTERM the updater waits
  up to 30 s for a new daemon on the new binary, and reports a failure if none
  appears.
- On Linux, `restart_error` is cleared once a check finds the daemon running
  the installed binary, for example after a manual restart. Before, it stayed
  until the next release.
- The test suite no longer runs the real `launchctl`. On a developer Mac with
  Pilot installed, it used to restart the running daemon.

## [v0.1.0]

Initial release.
