# updater

Pilot Protocol auto-updater plugin. Polls the GitHub releases endpoint
hourly, hot-swaps the daemon/pilotctl/gateway binaries when a newer
SemVer tag appears, and exits so the supervisor restarts the new copy.

## Layout

| File | What it does |
|---|---|
| `updater.go` | `Updater` — GitHub release polling, download, SHA verify, atomic rename. |
| `version.go` | SemVer parsing + comparison. |
| `service.go` | `*Service` — `coreapi.Service` adapter. Build tag `!no_updater`. |
| `service_disabled.go` | Stub when build tag `no_updater` is set. |

## Import paths

```go
import "github.com/pilot-protocol/updater"

u := updater.New(updater.Config{
    Repo:        "TeoSlayer/pilotprotocol",
    CurrentVer:  "v1.10.5",
    BinaryNames: []string{"pilot-daemon", "pilotctl"},
})
u.Run(ctx)
```

The sidecar `cmd/updater/main.go` is the standalone entry point;
the in-process Service adapter is used when embedding into the daemon.

## Disabling

Pass `-tags no_updater` to compile a stub whose `Start` is a no-op.

## Releasing

Tag a SemVer version (e.g. `v0.1.0`); web4 pulls it in via
`require github.com/pilot-protocol/updater v0.1.0`. During
co-development the protocol repo uses `replace ../updater`.
