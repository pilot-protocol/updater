// SPDX-License-Identifier: AGPL-3.0-or-later

package updater

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Cgroup lines as a real /proc/<pid>/cgroup shows them.
const (
	cgroupPilotService = "0::/system.slice/pilot-daemon.service\n"
	cgroupContainer    = "0::/\n"
	cgroupSession      = "0::/user.slice/user-1000.slice/session-3.scope\n"
)

// fakeSystem is a fake /proc plus a systemd unit directory, for driving the
// Linux restart path on any OS. Its kill hook plays systemd: after a
// successful SIGTERM the old pid disappears and, when restarts is set, a new
// daemon process on the installed binary appears in the same cgroup.
type fakeSystem struct {
	t       *testing.T
	proc    string
	unitDir string
	daemon  string // installed daemon binary path

	mu        sync.Mutex
	nextPid   int
	kills     []string
	restarts  bool          // "systemd" starts the daemon again after SIGTERM
	delay     time.Duration // how long it takes to do so
	onKill    func(pid int, sig syscall.Signal) error
	restarted int // pid of the new daemon, once started
}

func newFakeSystem(t *testing.T, installDir string) *fakeSystem {
	t.Helper()
	fs := &fakeSystem{
		t:       t,
		proc:    t.TempDir(),
		unitDir: t.TempDir(),
		daemon:  filepath.Join(installDir, "pilot-daemon"),
		nextPid: 4000000,
	}
	// Non-pid noise that real /proc has.
	if err := os.WriteFile(filepath.Join(fs.proc, "uptime"), []byte("1 1"), 0o644); err != nil {
		t.Fatal(err)
	}
	return fs
}

// addProc adds a process whose exe link points at exe, with the given
// /proc/<pid>/cgroup content ("" = no cgroup file).
func (fs *fakeSystem) addProc(pid int, exe, cgroup string) {
	fs.t.Helper()
	dir := filepath.Join(fs.proc, fmt.Sprint(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fs.t.Fatal(err)
	}
	if err := os.Symlink(exe, filepath.Join(dir, "exe")); err != nil {
		fs.t.Fatal(err)
	}
	if cgroup != "" {
		if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte(cgroup), 0o644); err != nil {
			fs.t.Fatal(err)
		}
	}
}

// addReplacedDaemon adds the daemon as it looks right after an update: its
// binary was renamed over, so /proc/<pid>/exe reads "<path> (deleted)".
func (fs *fakeSystem) addReplacedDaemon(pid int, cgroup string) {
	fs.addProc(pid, fs.daemon+" (deleted)", cgroup)
}

func (fs *fakeSystem) writeFile(rel, content string) {
	fs.t.Helper()
	p := filepath.Join(fs.unitDir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		fs.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		fs.t.Fatal(err)
	}
}

func (fs *fakeSystem) alive(pid int) bool {
	_, err := os.Lstat(filepath.Join(fs.proc, fmt.Sprint(pid), "exe"))
	return err == nil
}

func (fs *fakeSystem) killCount() int {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return len(fs.kills)
}

// attach wires u to the fake system. It does not force the OS: callers that
// want the Linux path on any host set u.goos = "linux".
func (fs *fakeSystem) attach(u *Updater) {
	u.procRoot = fs.proc
	u.systemdDirs = []string{fs.unitDir}
	u.restartWait = 2 * time.Second
	u.restartPoll = time.Millisecond
	u.killFn = func(pid int, sig syscall.Signal) error {
		fs.mu.Lock()
		fs.kills = append(fs.kills, fmt.Sprintf("kill %d %d", pid, sig))
		hook, restarts, delay := fs.onKill, fs.restarts, fs.delay
		fs.mu.Unlock()
		if hook != nil {
			if err := hook(pid, sig); err != nil {
				return err
			}
		}
		dir := filepath.Join(fs.proc, fmt.Sprint(pid))
		cgroup, _ := os.ReadFile(filepath.Join(dir, "cgroup"))
		if err := os.RemoveAll(dir); err != nil { // the old daemon exits
			fs.t.Error(err)
		}
		if restarts {
			start := func() {
				fs.mu.Lock()
				newPid := fs.nextPid
				fs.nextPid++
				fs.restarted = newPid
				fs.mu.Unlock()
				fs.addProc(newPid, fs.daemon, string(cgroup))
			}
			if delay > 0 {
				time.AfterFunc(delay, start)
			} else {
				start()
			}
		}
		return nil
	}
}

// daemonUnit is the pilot-daemon.service install.sh writes, with the given
// Restart= policy ("" = no Restart= line).
func daemonUnit(daemon, restart string) string {
	unit := "[Unit]\nDescription=Pilot Protocol Daemon\nAfter=network-online.target\n\n" +
		"[Service]\nType=simple\nUser=pilot\n" +
		"ExecStart=" + daemon + " \\\n" +
		"  -registry registry.example:9000 \\\n" +
		"  -listen :4000 \\\n" +
		"  -encrypt -hostname=node-1\n"
	if restart != "" {
		unit += "Restart=" + restart + "\n"
	}
	return unit + "RestartSec=5\n\n[Install]\nWantedBy=multi-user.target\n"
}

// supervisedLinuxUpdater returns an Updater on the Linux restart path with a
// fake daemon (pid 77, already replaced on disk) in pilot-daemon.service.
func supervisedLinuxUpdater(t *testing.T, restart string) (*Updater, *fakeSystem) {
	t.Helper()
	installDir := t.TempDir()
	fs := newFakeSystem(t, installDir)
	fs.addReplacedDaemon(77, cgroupPilotService)
	fs.writeFile("pilot-daemon.service", daemonUnit(fs.daemon, restart))
	fs.restarts = true
	u := &Updater{config: Config{InstallDir: installDir}, goos: "linux"}
	fs.attach(u)
	return u, fs
}

// TestSignalDaemonRestartLinux_UnsupervisedDaemonIsLeftRunning is the core of
// UPD49-1: SIGTERM is only sent when systemd will start the daemon again. A
// daemon started by `pilotctl daemon start` in a container, WSL or CI has no
// supervisor; stopping it took the node offline while the status file said
// "updated". Now it keeps running the old binary and the error says how to
// restart it.
func TestSignalDaemonRestartLinux_UnsupervisedDaemonIsLeftRunning(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		cgroup string
		unit   string // pilot file name -> content written when non-empty
		want   []string
	}{
		{
			name:   "container, cgroup v2 namespace root",
			cgroup: cgroupContainer,
			want:   []string{"not run by a systemd service", "pilotctl daemon stop && pilotctl daemon start"},
		},
		{
			name:   "login session scope",
			cgroup: cgroupSession,
			want:   []string{"not run by a systemd service", "pilotctl daemon stop && pilotctl daemon start"},
		},
		{
			name: "cgroup v1 login session",
			cgroup: "12:pids:/user.slice/user-1000.slice/session-2.scope\n" +
				"4:cpu,cpuacct:/user.slice\n" +
				"1:name=systemd:/user.slice/user-1000.slice/session-2.scope\n",
			want: []string{"not run by a systemd service"},
		},
		{
			name:   "no cgroup file",
			cgroup: "",
			want:   []string{"cannot tell whether a service manager would start it again", "pilotctl daemon stop"},
		},
		{
			name:   "child of another service (CI runner with Restart=always)",
			cgroup: "0::/system.slice/actions.runner.org.host.service\n",
			unit:   "[Service]\nExecStart=/home/runner/actions-runner/runsvc.sh\nRestart=always\nKillMode=process\n",
			want:   []string{"actions.runner.org.host.service", `starts "/home/runner/actions-runner/runsvc.sh"`, "pilotctl daemon stop"},
		},
		{
			name:   "systemd user service",
			cgroup: "0::/user.slice/user-1000.slice/user@1000.service/app.slice/pilot-daemon.service\n",
			want:   []string{"systemd user service pilot-daemon.service", "systemctl --user restart pilot-daemon"},
		},
		{
			name:   "service whose unit file cannot be found",
			cgroup: "0::/system.slice/pilot-daemon.service\n",
			want:   []string{"could not be read", "sudo systemctl restart pilot-daemon"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			installDir := t.TempDir()
			fs := newFakeSystem(t, installDir)
			fs.addReplacedDaemon(77, tc.cgroup)
			if tc.unit != "" {
				unitName := tc.cgroup[strings.LastIndex(tc.cgroup, "/")+1 : len(tc.cgroup)-1]
				fs.writeFile(unitName, tc.unit)
			}
			fs.restarts = true
			u := &Updater{config: Config{InstallDir: installDir}, goos: "linux"}
			fs.attach(u)

			err := u.signalDaemonRestart().err
			if err == nil {
				t.Fatal("signalDaemonRestart = nil; an unsupervised daemon must be reported, not stopped")
			}
			if fs.killCount() != 0 {
				t.Errorf("daemon was signalled (%v); nothing would start it again", fs.kills)
			}
			if !fs.alive(77) {
				t.Error("daemon pid 77 is gone")
			}
			for _, want := range append(tc.want, "left running the old version") {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

// TestSignalDaemonRestartLinux_RestartPolicy: pilot-daemon exits 0 on
// SIGTERM, which systemd restarts only under Restart=always / on-success (or
// when RestartForceExitStatus lists it). Units written by install.sh up to
// v1.9.0 say Restart=on-failure and are never rewritten by updates.
func TestSignalDaemonRestartLinux_RestartPolicy(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		restart  string            // Restart= in the unit ("" = none)
		files    map[string]string // extra unit-dir files (drop-ins)
		cgroup   string            // default cgroupPilotService
		fragment string            // unit file name, default pilot-daemon.service
		signal   bool
		want     string // expected error text when not signalled
	}{
		{name: "always (install.sh since web4 1ee4db7d)", restart: "always", signal: true},
		{name: "on-success", restart: "on-success", signal: true},
		{name: "on-failure (install.sh up to v1.9.0)", restart: "on-failure", want: "has Restart=on-failure"},
		{name: "unset", restart: "", want: "has Restart=no"},
		{name: "on-abnormal", restart: "on-abnormal", want: "has Restart=on-abnormal"},
		{name: "upper-case value", restart: "Always", signal: true},
		{
			name: "drop-in overrides always with on-failure", restart: "always",
			files: map[string]string{"pilot-daemon.service.d/override.conf": "[Service]\nRestart=on-failure\n"},
			want:  "has Restart=on-failure",
		},
		{
			name: "drop-in overrides on-failure with always", restart: "on-failure",
			files:  map[string]string{"pilot-daemon.service.d/override.conf": "[Service]\nRestart=always\n"},
			signal: true,
		},
		{
			name: "drop-ins apply in file-name order", restart: "on-failure",
			files: map[string]string{
				"pilot-daemon.service.d/20-late.conf":  "[Service]\nRestart=always\n",
				"pilot-daemon.service.d/10-early.conf": "[Service]\nRestart=no\n",
			},
			signal: true,
		},
		{
			name: "top-level service.d drop-in", restart: "always",
			files: map[string]string{"service.d/10-all.conf": "[Service]\nRestart=on-failure\n"},
			want:  "has Restart=on-failure",
		},
		{
			name: "RestartForceExitStatus=0 restarts an on-failure unit", restart: "on-failure",
			files:  map[string]string{"pilot-daemon.service.d/force.conf": "[Service]\nRestartForceExitStatus=0\n"},
			signal: true,
		},
		{
			name: "RestartPreventExitStatus=SIGTERM stops always", restart: "always",
			files: map[string]string{"pilot-daemon.service.d/prevent.conf": "[Service]\nRestartPreventExitStatus=1 SIGTERM\n"},
			want:  "has Restart=always",
		},
		{
			name: "drop-in replaces ExecStart with a wrapper", restart: "always",
			files: map[string]string{"pilot-daemon.service.d/wrap.conf": "[Service]\nExecStart=\nExecStart=/opt/wrap.sh --run\n"},
			want:  `starts "/opt/wrap.sh"`,
		},
		{
			name: "template instance", restart: "always",
			cgroup:   "0::/system.slice/system-pilot\\x2ddaemon.slice/pilot-daemon@main.service\n",
			fragment: "pilot-daemon@.service", signal: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			installDir := t.TempDir()
			fs := newFakeSystem(t, installDir)
			cgroup := tc.cgroup
			if cgroup == "" {
				cgroup = cgroupPilotService
			}
			fragment := tc.fragment
			if fragment == "" {
				fragment = "pilot-daemon.service"
			}
			fs.addReplacedDaemon(77, cgroup)
			fs.writeFile(fragment, daemonUnit(fs.daemon, tc.restart))
			for rel, content := range tc.files {
				fs.writeFile(rel, content)
			}
			fs.restarts = true
			u := &Updater{config: Config{InstallDir: installDir}, goos: "linux"}
			fs.attach(u)

			err := u.signalDaemonRestart().err
			if tc.signal {
				if err != nil {
					t.Fatalf("signalDaemonRestart: %v", err)
				}
				if fs.killCount() != 1 || fs.kills[0] != fmt.Sprintf("kill 77 %d", syscall.SIGTERM) {
					t.Errorf("kills = %v, want one SIGTERM to 77", fs.kills)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("signalDaemonRestart = %v, want error mentioning %q", err, tc.want)
			}
			if fs.killCount() != 0 || !fs.alive(77) {
				t.Errorf("daemon was stopped (%v) although systemd would not start it again", fs.kills)
			}
		})
	}
}

// TestSignalDaemonRestartLinux_OnFailureMessage pins the guidance given for
// the pre-v1.9.0 unit, the most common unsupervised case on systemd hosts.
func TestSignalDaemonRestartLinux_OnFailureMessage(t *testing.T) {
	t.Parallel()
	u, fs := supervisedLinuxUpdater(t, "on-failure")
	err := u.signalDaemonRestart().err
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{
		"restart daemon:", "pid 77", "pilot-daemon.service has Restart=on-failure",
		"sudo systemctl restart pilot-daemon", "re-running install.sh",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if fs.killCount() != 0 {
		t.Errorf("kills = %v", fs.kills)
	}
}

// TestSignalDaemonRestartLinux_ReportsDaemonThatDoesNotComeBack: even with
// Restart=always, if no new daemon appears after SIGTERM, the restart failed
// and must be reported rather than recorded as success.
func TestSignalDaemonRestartLinux_ReportsDaemonThatDoesNotComeBack(t *testing.T) {
	t.Parallel()
	u, fs := supervisedLinuxUpdater(t, "always")
	fs.restarts = false
	u.restartWait = 50 * time.Millisecond

	err := u.signalDaemonRestart().err
	if err == nil {
		t.Fatal("signalDaemonRestart = nil although the daemon never came back")
	}
	for _, want := range []string{"sent SIGTERM to the daemon (pid 77)", "did not start it again", "systemctl status pilot-daemon", "sudo systemctl start pilot-daemon"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if fs.killCount() != 1 {
		t.Errorf("kills = %v, want 1", fs.kills)
	}
}

// TestSignalDaemonRestartLinux_WaitsForNewDaemon: systemd starts the daemon
// again after RestartSec; the restart succeeds once the new process runs the
// installed binary, and the old "(deleted)" process never counts.
func TestSignalDaemonRestartLinux_WaitsForNewDaemon(t *testing.T) {
	t.Parallel()
	u, fs := supervisedLinuxUpdater(t, "always")
	fs.delay = 30 * time.Millisecond
	// Another process still on a replaced binary must not satisfy the wait.
	fs.addReplacedDaemon(78, cgroupPilotService)
	fs.onKill = func(pid int, _ syscall.Signal) error {
		if pid != 77 {
			return fmt.Errorf("unexpected pid %d", pid)
		}
		return nil
	}

	start := time.Now()
	if err := u.signalDaemonRestart().err; err != nil {
		t.Fatalf("signalDaemonRestart: %v", err)
	}
	if time.Since(start) < fs.delay {
		t.Errorf("returned after %v, before the new daemon started", time.Since(start))
	}
	fs.mu.Lock()
	restarted := fs.restarted
	fs.mu.Unlock()
	if restarted == 0 || !fs.alive(restarted) {
		t.Errorf("new daemon pid %d not running", restarted)
	}
}

// TestSignalDaemonRestartLinux_StopAbortsWait: Stop() must not wait out the
// restart window.
func TestSignalDaemonRestartLinux_StopAbortsWait(t *testing.T) {
	t.Parallel()
	u, fs := supervisedLinuxUpdater(t, "always")
	fs.restarts = false
	u.restartWait = time.Minute
	u.stopCh = make(chan struct{})
	close(u.stopCh)

	done := make(chan error, 1)
	go func() { done <- u.signalDaemonRestart().err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "updater stopped") {
			t.Errorf("signalDaemonRestart = %v, want stopped error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("signalDaemonRestart did not return after Stop")
	}
}

// TestRunOnce_LinuxUnsupervisedDaemon reproduces UPD49-1 end to end: a
// container node whose daemon was started by `pilotctl daemon start` runs
// `pilotctl update`. The update installs, the daemon keeps running (old
// binary) and the status file says so, instead of "updated" with the node
// silently offline.
func TestRunOnce_LinuxUnsupervisedDaemon(t *testing.T) {
	t.Parallel()
	srv := newReleaseServer(t, "v2.0.0", map[string]string{"daemon": "new-daemon"})
	u, statusPath := newTestUpdater(t, srv.Server, "v1.0.0")
	fs := newFakeSystem(t, u.config.InstallDir)
	fs.addReplacedDaemon(3999999, cgroupContainer)
	fs.restarts = true
	u.goos = "linux"
	fs.attach(u)

	if err := u.RunOnce(); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if fs.killCount() != 0 || !fs.alive(3999999) {
		t.Fatalf("unsupervised daemon was stopped: %v", fs.kills)
	}
	st := mustReadStatus(t, statusPath)
	if st.LastResult != ResultUpdated || st.CurrentVersion != "v2.0.0" {
		t.Errorf("status = %+v", st)
	}
	if !strings.Contains(st.RestartError, "pilotctl daemon stop && pilotctl daemon start") {
		t.Errorf("restart_error = %q, want the manual restart command", st.RestartError)
	}
	if got := u.LastStatus().RestartError; got != st.RestartError {
		t.Errorf("LastStatus().RestartError = %q, want %q", got, st.RestartError)
	}
}

// TestRunOnce_LinuxOnFailureUnit: the systemd half of UPD49-1. A unit from
// an old installer (Restart=on-failure) is not stopped; restart_error says
// how to restart it.
func TestRunOnce_LinuxOnFailureUnit(t *testing.T) {
	t.Parallel()
	srv := newReleaseServer(t, "v2.0.0", map[string]string{"daemon": "new-daemon"})
	u, statusPath := newTestUpdater(t, srv.Server, "v1.0.0")
	fs := newFakeSystem(t, u.config.InstallDir)
	fs.addReplacedDaemon(3999999, cgroupPilotService)
	fs.writeFile("pilot-daemon.service", daemonUnit(fs.daemon, "on-failure"))
	fs.restarts = true
	u.goos = "linux"
	fs.attach(u)

	if err := u.RunOnce(); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if fs.killCount() != 0 || !fs.alive(3999999) {
		t.Fatalf("daemon under Restart=on-failure was stopped: %v", fs.kills)
	}
	st := mustReadStatus(t, statusPath)
	if st.LastResult != ResultUpdated || !strings.Contains(st.RestartError, "Restart=on-failure") {
		t.Errorf("status = %s / restart_error %q", st.LastResult, st.RestartError)
	}
}

// TestRunOnce_LinuxAlwaysUnitRestartsDaemon: the supported install. The
// daemon is stopped, comes back on the new binary, and restart_error is
// empty.
func TestRunOnce_LinuxAlwaysUnitRestartsDaemon(t *testing.T) {
	t.Parallel()
	srv := newReleaseServer(t, "v2.0.0", map[string]string{"daemon": "new-daemon"})
	u, statusPath := newTestUpdater(t, srv.Server, "v1.0.0")
	fs := newFakeSystem(t, u.config.InstallDir)
	fs.addReplacedDaemon(3999999, cgroupPilotService)
	fs.writeFile("pilot-daemon.service", daemonUnit(fs.daemon, "always"))
	fs.restarts = true
	u.goos = "linux"
	fs.attach(u)
	if err := writeStatusFile(statusPath, Status{RestartError: "an old failure"}); err != nil {
		t.Fatal(err)
	}

	if err := u.RunOnce(); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if fs.killCount() != 1 {
		t.Errorf("kills = %v, want 1", fs.kills)
	}
	if st := mustReadStatus(t, statusPath); st.LastResult != ResultUpdated || st.RestartError != "" {
		t.Errorf("status = %s / restart_error %q, want updated with no restart error", st.LastResult, st.RestartError)
	}
}

// TestRecoverPendingRestart_LinuxUnsupervised: the auto path (the updater
// replaced itself; the new process restarts the daemon) applies the same
// rule and records the result.
func TestRecoverPendingRestart_LinuxUnsupervised(t *testing.T) {
	t.Parallel()
	installDir := t.TempDir()
	statusPath := filepath.Join(t.TempDir(), StatusFileName)
	if err := os.WriteFile(filepath.Join(installDir, ".pilot-version"), []byte("v2.0.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(installDir, "pilot-daemon"), []byte("d"), 0o755); err != nil {
		t.Fatal(err)
	}
	fs := newFakeSystem(t, installDir)
	fs.addReplacedDaemon(3999999, cgroupSession)
	u := &Updater{config: Config{InstallDir: installDir, StatusPath: statusPath}, goos: "linux"}
	fs.attach(u)

	u.recoverPendingRestart()
	if fs.killCount() != 0 {
		t.Errorf("kills = %v", fs.kills)
	}
	if st := mustReadStatus(t, statusPath); !strings.Contains(st.RestartError, "not run by a systemd service") {
		t.Errorf("restart_error = %q", st.RestartError)
	}
}

// TestRunCheck_ClearsRestartErrorOnceDaemonIsCurrent: restart_error would
// otherwise stay until the next release, long after the operator restarted
// the daemon by hand. A check that finds the daemon on the installed binary
// clears it; one that finds it still on a replaced binary keeps it.
func TestRunCheck_ClearsRestartErrorOnceDaemonIsCurrent(t *testing.T) {
	t.Parallel()
	srv := newReleaseServer(t, "v1.0.0", map[string]string{"daemon": "d"})
	u, statusPath := newTestUpdater(t, srv.Server, "v1.0.0")
	fs := newFakeSystem(t, u.config.InstallDir)
	fs.addReplacedDaemon(500, cgroupContainer)
	u.goos = "linux"
	fs.attach(u)
	const restartErr = "restart daemon: daemon (pid 500) left running the old version"
	if err := writeStatusFile(statusPath, Status{RestartError: restartErr}); err != nil {
		t.Fatal(err)
	}

	// Still on the replaced binary: keep the error.
	if err := u.RunOnce(); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if st := mustReadStatus(t, statusPath); st.RestartError != restartErr || st.LastResult != ResultUpToDate {
		t.Fatalf("restart_error = %q (%s), want kept while the daemon is on the old binary", st.RestartError, st.LastResult)
	}

	// Operator ran `pilotctl daemon stop && pilotctl daemon start`.
	if err := os.RemoveAll(filepath.Join(fs.proc, "500")); err != nil {
		t.Fatal(err)
	}
	fs.addProc(501, fs.daemon, cgroupContainer)
	if err := u.RunOnce(); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if st := mustReadStatus(t, statusPath); st.RestartError != "" {
		t.Errorf("restart_error = %q, want cleared once the daemon runs the installed binary", st.RestartError)
	}

	// macOS has no /proc: it asks the daemon over IPC instead (see
	// TestRunCheck_DarwinClearsRestartErrorOnceDaemonIsCurrent). With no
	// daemon answering, the error is kept.
	if err := writeStatusFile(statusPath, Status{RestartError: restartErr}); err != nil {
		t.Fatal(err)
	}
	u.goos = "darwin"
	if err := u.RunOnce(); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if st := mustReadStatus(t, statusPath); st.RestartError != restartErr {
		t.Errorf("darwin: restart_error = %q, want kept while no daemon answers", st.RestartError)
	}
}

func TestServiceUnitOf(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, cgroup string
		unit         string
		user         bool
	}{
		{"v2 system service", "0::/system.slice/pilot-daemon.service\n", "pilot-daemon.service", false},
		{"v1 name=systemd", "11:memory:/system.slice/pilot-daemon.service\n1:name=systemd:/system.slice/pilot-daemon.service\n", "pilot-daemon.service", false},
		{"hybrid", "1:name=systemd:/system.slice/pilot-daemon.service\n0::/system.slice/pilot-daemon.service\n", "pilot-daemon.service", false},
		{"delegated sub-cgroup", "0::/system.slice/pilot-daemon.service/payload\n", "pilot-daemon.service", false},
		{"escaped name", "0::/system.slice/_cgroup.service\n", "cgroup.service", false},
		{"user service", "0::/user.slice/user-1000.slice/user@1000.service/app.slice/pilot-daemon.service\n", "pilot-daemon.service", true},
		{"user manager scope", "0::/user.slice/user-1000.slice/user@1000.service/init.scope\n", "", false},
		{"session", cgroupSession, "", false},
		{"container root", cgroupContainer, "", false},
		{"docker from host", "0::/system.slice/docker-0123abcd.scope\n", "", false},
		{"v1 without systemd", "5:cpu,cpuacct:/docker/0123\n", "", false},
		{"garbage", "not a cgroup file", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fs := newFakeSystem(t, t.TempDir())
			fs.addProc(10, "/bin/x", tc.cgroup)
			unit, user, err := serviceUnitOf(fs.proc, 10)
			if err != nil || unit != tc.unit || user != tc.user {
				t.Errorf("serviceUnitOf = %q, %v, %v; want %q, %v", unit, user, err, tc.unit, tc.user)
			}
		})
	}
	if _, _, err := serviceUnitOf(t.TempDir(), 10); err == nil {
		t.Error("missing cgroup file must be an error")
	}
}

func TestServiceUnitParse(t *testing.T) {
	t.Parallel()
	var su serviceUnit
	su.parse([]byte(`# comment
[Unit]
Restart=always
ExecStart=/not/the/service

[Service]
; another comment
ExecStart=-"/opt/pilot bin/pilot-daemon" \
# a comment inside a continued line is skipped
  -listen :4000 \
  -encrypt
Restart = On-Failure
RestartForceExitStatus=1 2
RestartForceExitStatus=
RestartPreventExitStatus=SIGKILL
ExecStart=/second/command

[Install]
Restart=always
`))
	if su.execStart != "/opt/pilot bin/pilot-daemon" {
		t.Errorf("execStart = %q", su.execStart)
	}
	if su.restart != "on-failure" || su.restartSetting() != "on-failure" {
		t.Errorf("restart = %q", su.restart)
	}
	if len(su.forceExit) != 0 {
		t.Errorf("forceExit = %v, want reset by the empty assignment", su.forceExit)
	}
	if len(su.preventExit) != 1 || su.preventExit[0] != "SIGKILL" {
		t.Errorf("preventExit = %v", su.preventExit)
	}
	if su.restartsOnCleanExit() {
		t.Error("on-failure must not restart after a clean exit")
	}

	// An empty ExecStart= resets it; the next one wins.
	su.parse([]byte("[Service]\nExecStart=\nExecStart=@/usr/bin/pilot-daemon pilot -x\nRestart=\n"))
	if su.execStart != "/usr/bin/pilot-daemon" || su.restartSetting() != "no" {
		t.Errorf("after reset: execStart %q restart %q", su.execStart, su.restartSetting())
	}
}

func TestExecProgramAndTemplateOf(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"/a/pilot-daemon -x":      "/a/pilot-daemon",
		"-/a/pilot-daemon":        "/a/pilot-daemon",
		"!!/a/pilot-daemon\t-x":   "/a/pilot-daemon",
		"+:'/a b/pilot-daemon' x": "/a b/pilot-daemon",
		`"/unterminated`:          "/unterminated",
		"   ":                     "",
	} {
		if got := execProgram(in); got != want {
			t.Errorf("execProgram(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"pilot-daemon@main.service": "pilot-daemon@.service",
		"pilot-daemon@.service":     "",
		"pilot-daemon.service":      "",
		"a.b@c":                     "",
	} {
		if got := templateOf(in); got != want {
			t.Errorf("templateOf(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSameFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "pilot-daemon"), []byte("d"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if !sameFile(filepath.Join(link, "pilot-daemon"), filepath.Join(real, "pilot-daemon")) {
		t.Error("symlinked install dir must match")
	}
	if !sameFile(real+"/./pilot-daemon", filepath.Join(real, "pilot-daemon")) {
		t.Error("uncleaned path must match")
	}
	if sameFile("", filepath.Join(real, "pilot-daemon")) || sameFile("/nope/pilot-daemon", filepath.Join(real, "pilot-daemon")) {
		t.Error("different or empty program must not match")
	}
}

func TestLoadServiceUnit_Errors(t *testing.T) {
	t.Parallel()
	if _, err := loadServiceUnit([]string{t.TempDir()}, "pilot-daemon.service"); err == nil {
		t.Error("missing unit must be an error")
	}
	// Higher-priority directory wins for the fragment and for a drop-in of
	// the same name.
	hi, lo := t.TempDir(), t.TempDir()
	for dir, restart := range map[string]string{hi: "always", lo: "no"} {
		if err := os.WriteFile(filepath.Join(dir, "p.service"), []byte("[Service]\nExecStart=/x\nRestart="+restart+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	su, err := loadServiceUnit([]string{hi, lo}, "p.service")
	if err != nil || su.restart != "always" || su.fragment != filepath.Join(hi, "p.service") {
		t.Fatalf("fragment precedence: %+v, %v", su, err)
	}
	for dir, restart := range map[string]string{hi: "on-success", lo: "no"} {
		if err := os.MkdirAll(filepath.Join(dir, "p.service.d"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "p.service.d", "x.conf"), []byte("[Service]\nRestart="+restart+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if su, err := loadServiceUnit([]string{hi, lo}, "p.service"); err != nil || su.restart != "on-success" {
		t.Errorf("drop-in precedence: restart %q, %v", su.restart, err)
	}
}

// TestDaemonOnInstalledBinary covers the /proc reading behind clearing
// restart_error.
func TestDaemonOnInstalledBinary(t *testing.T) {
	t.Parallel()
	installDir := t.TempDir()
	fs := newFakeSystem(t, installDir)
	u := &Updater{config: Config{InstallDir: installDir}, goos: "linux", procRoot: fs.proc}
	if u.daemonOnInstalledBinary() {
		t.Error("no daemon running: want false")
	}
	fs.addProc(20, fs.daemon, "")
	if !u.daemonOnInstalledBinary() {
		t.Error("daemon on the installed binary: want true")
	}
	fs.addReplacedDaemon(21, "")
	if u.daemonOnInstalledBinary() {
		t.Error("a daemon still on a replaced binary: want false")
	}
	u.procRoot = filepath.Join(fs.proc, "missing")
	if u.daemonOnInstalledBinary() {
		t.Error("unreadable /proc: want false")
	}
	if (&Updater{goos: "darwin", procRoot: fs.proc}).daemonOnInstalledBinary() {
		t.Error("darwin: want false")
	}
}
