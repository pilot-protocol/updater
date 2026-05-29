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
    Repo:          "TeoSlayer/pilotprotocol",
    InstallDir:    "/home/user/.pilot/bin",
    Version:       "v1.10.5",
    CheckInterval: 1 * time.Hour,
})
u.Start()
```

### Pinning a version

Set `PinnedVersion` to lock the updater to a specific release tag. When
set, the updater fetches the exact release (via
`/releases/tags/{tag}`), applies it if it differs from the current
install, then idles — it will **not** chase the latest release. Clear
`PinnedVersion` (set to `""`) to resume auto-updating.

```go
u := updater.New(updater.Config{
    Repo:          "TeoSlayer/pilotprotocol",
    InstallDir:    "/home/user/.pilot/bin",
    PinnedVersion: "v1.10.5",  // stay on this version
})
u.Start()
```

The in-process `Service` adapter is used when embedding into the
daemon; a standalone sidecar binary built from this package is also
supported.

## Layout

| File | What it does |
|---|---|
| `updater.go` | `Updater` — GitHub release polling, download, SHA verify, atomic rename. |
| `version.go` | SemVer parsing and comparison. |
| `service.go` | `*Service` — `coreapi.Service` adapter. Build tag `!no_updater`. |
| `service_disabled.go` | Stub when build tag `no_updater` is set. |

## Build tags

| Tag | Effect |
|---|---|
| `no_updater` | Compiles a stub whose `Start` is a no-op. |

## License

AGPL-3.0-or-later. See [LICENSE](LICENSE).
