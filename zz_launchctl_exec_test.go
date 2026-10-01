// SPDX-License-Identifier: AGPL-3.0-or-later

package updater

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pilot-protocol/common/ipcutil"
)

// IPC commands of the daemon's info call (common/driver).
const (
	ipcCmdInfo   byte = 0x0D
	ipcCmdInfoOK byte = 0x0E
)

// shortTempDir returns a temp dir with a short path: unix socket paths are
// limited to 104 bytes on macOS, which t.TempDir() can exceed.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "pu-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// serveFakeDaemonIPC answers the daemon's IPC info call on socket with the
// version version() returns. When silent, it accepts connections but never
// answers (a wedged daemon).
func serveFakeDaemonIPC(t *testing.T, socket string, version func() string, silent bool) {
	t.Helper()
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				for {
					msg, err := ipcutil.Read(c)
					if err != nil {
						return
					}
					if silent || len(msg) == 0 || msg[0] != ipcCmdInfo {
						continue
					}
					body, _ := json.Marshal(map[string]interface{}{"version": version(), "node_id": 42})
					if err := ipcutil.Write(c, append([]byte{ipcCmdInfoOK}, body...)); err != nil {
						return
					}
				}
			}(c)
		}
	}()
}

// TestProbeDaemonVersion exercises the real IPC probe against a fake daemon
// socket: an answer, no daemon, and a daemon that never answers (bounded by
// daemonProbeTimeout).
func TestProbeDaemonVersion(t *testing.T) {
	t.Parallel()
	dir := shortTempDir(t)

	answering := filepath.Join(dir, "a.sock")
	serveFakeDaemonIPC(t, answering, func() string { return "v1.13.10" }, false)
	if v, running := realProbeDaemonVersion(answering); !running || v != "v1.13.10" {
		t.Errorf("answering daemon = %q, %v", v, running)
	}

	if v, running := realProbeDaemonVersion(filepath.Join(dir, "none.sock")); running || v != "" {
		t.Errorf("no daemon = %q, %v; want not running", v, running)
	}

	silent := filepath.Join(dir, "s.sock")
	serveFakeDaemonIPC(t, silent, func() string { return "" }, true)
	start := time.Now()
	if v, running := realProbeDaemonVersion(silent); !running || v != "" {
		t.Errorf("wedged daemon = %q, %v; want running, unknown version", v, running)
	}
	if took := time.Since(start); took > daemonProbeTimeout+2*time.Second {
		t.Errorf("wedged daemon probe took %s", took)
	}
}

// fakeLaunchctlScript is a launchctl stand-in for one job, kept in state
// files under $S. It uses only shell builtins: PATH holds nothing else.
const fakeLaunchctlScript = `#!/bin/sh
S='@STATE@'
printf '%s\n' "$*" >> "$S/calls"
read -r target < "$S/target"
if [ "$2" != "$target" ] && [ "$3" != "$target" ]; then
	echo "Could not find service \"${2##*/}\" in domain for user gui: 501" >&2
	exit 113
fi
case "$1" in
print)
	read -r program < "$S/program"
	read -r socket < "$S/socket"
	read -r pid < "$S/pid"
	printf '%s = {\n\tactive count = 1\n\ttype = LaunchAgent\n' "$target"
	if [ "$pid" -gt 0 ]; then printf '\tstate = running\n'; else printf '\tstate = not running\n'; fi
	printf '\n\tprogram = %s\n\targuments = {\n\t\t%s\n\t\t-registry\n\t\t34.71.57.205:9000\n\t\t-socket\n\t\t%s\n\t\t-encrypt\n\t}\n\n' "$program" "$program" "$socket"
	printf '\tdomain = gui/501 [100023]\n\texit timeout = 5\n\truns = 2\n'
	if [ "$pid" -gt 0 ]; then printf '\tpid = %s\n' "$pid"; fi
	printf '\tlast exit code = 0\n\n\tresource coalition = {\n\t\tID = 2814\n\t\tstate = active\n\t}\n}\n'
	;;
kickstart)
	[ "$2" = "-k" ] || exit 64
	read -r pid < "$S/pid"
	echo $((pid + 1)) > "$S/pid"
	read -r next < "$S/next-version"
	echo "$next" > "$S/version"
	;;
*)
	exit 64
	;;
esac
`

// TestDarwinRestart_ThroughLaunchctlExecutable runs the macOS restart the
// way production does: runCommand execs `launchctl` found on PATH and the
// probe speaks the daemon's IPC protocol over a unix socket. launchctl is a
// shell script and the daemon a fake IPC server. PATH holds only the
// script's directory, so the real /bin/launchctl cannot be reached, and the
// probe refuses any socket but the fake daemon's. Not parallel: it sets
// PATH.
func TestDarwinRestart_ThroughLaunchctlExecutable(t *testing.T) {
	dir := shortTempDir(t)
	installDir := filepath.Join(dir, "bin")
	state := filepath.Join(dir, "launchd")
	pathDir := filepath.Join(dir, "path")
	socket := filepath.Join(dir, "d.sock")
	for _, d := range []string{installDir, state, pathDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, content string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(p, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	daemon := filepath.Join(installDir, "pilot-daemon")
	write(daemon, "new-daemon", 0o755)
	write(filepath.Join(installDir, ".pilot-version"), "v2.0.0\n", 0o644)
	for name, v := range map[string]string{
		"target":       daemonTarget,
		"program":      daemon,
		"socket":       socket,
		"pid":          "700",
		"version":      "v1.0.0",
		"next-version": "v2.0.0",
	} {
		write(filepath.Join(state, name), v+"\n", 0o644)
	}
	write(filepath.Join(pathDir, "launchctl"), strings.ReplaceAll(fakeLaunchctlScript, "@STATE@", state), 0o755)
	serveFakeDaemonIPC(t, socket, func() string {
		data, _ := os.ReadFile(filepath.Join(state, "version"))
		return strings.TrimSpace(string(data))
	}, false)
	t.Setenv("PATH", pathDir)

	u := &Updater{
		config:      Config{InstallDir: installDir},
		goos:        "darwin",
		runCmd:      realRunCommand,
		getuid:      func() int { return fakeUID },
		restartWait: 15 * time.Second,
		restartPoll: 10 * time.Millisecond,
		probeFn: func(s string) (string, bool) {
			if s != socket {
				t.Errorf("probed %s; only the fake daemon's socket may be probed", s)
				return "", false
			}
			return realProbeDaemonVersion(s)
		},
	}

	r := u.signalDaemonRestart()
	if r.err != nil || !r.restarted || r.version != "v2.0.0" || r.by != "launchd "+daemonTarget {
		t.Fatalf("outcome = %+v, want restarted on v2.0.0 by launchd", r)
	}
	// No updater job is loaded: nothing to restart.
	if _, attempted := u.restartUpdaterService(); attempted {
		t.Error("restartUpdaterService attempted without an updater job")
	}

	data, err := os.ReadFile(filepath.Join(state, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	calls := strings.Split(strings.TrimSpace(string(data)), "\n")
	var kicks []string
	for _, c := range calls {
		if strings.HasPrefix(c, "kickstart") {
			kicks = append(kicks, c)
		}
	}
	if calls[0] != "print "+daemonTarget || len(kicks) != 1 || kicks[0] != "kickstart -k "+daemonTarget {
		t.Errorf("launchctl calls = %q", calls)
	}
}
