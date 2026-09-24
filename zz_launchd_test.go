// SPDX-License-Identifier: AGPL-3.0-or-later

package updater

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeUID is the user the fake launchd serves (gui/501, user/501).
const fakeUID = 501

const (
	daemonTarget  = "gui/501/network.pilotprotocol.pilot-daemon"
	updaterTarget = "gui/501/network.pilotprotocol.pilot-updater"
	fakeSocket    = "/tmp/pilot-fake-test.sock"
)

// fakeLaunchd plays launchd and launchctl for the macOS restart path, and the
// daemon's IPC socket. It answers `launchctl print <target>` like the real
// launchctl (including nested blocks that repeat keys such as "state") and
// `launchctl kickstart -k <target>` by stopping the job's process and, unless
// told otherwise, starting a new one. A daemon job's process answers IPC on
// its -socket with its version.
type fakeLaunchd struct {
	t *testing.T

	mu      sync.Mutex
	jobs    map[string]*fakeJob // by target
	manual  map[string]string   // socket -> version of a daemon launchd does not run
	calls   []string
	nextPid int
	// installDir, when set, is where a restarted daemon without a newVersion
	// reads its version from (.pilot-version), like a real new process.
	installDir string
	// onKickstart runs before a kickstart is carried out; an error fails it.
	onKickstart func(target string) ([]byte, error)
}

type fakeJob struct {
	program  string
	args     []string
	pid      int // 0 = no process
	socket   string
	version  string // what the running process reports over IPC
	answers  bool   // the running process answers IPC
	lastExit string

	// What kickstart -k does.
	respawn    bool   // launchd starts a new process
	newVersion string // version of the new process ("" = installed version)
	newAnswers bool   // the new process answers IPC
	// respawnDelay is how long launchd still shows the old process after
	// kickstart returns.
	respawnDelay time.Duration
	kickErr      error
	kickOut      string
}

func newFakeLaunchd(t *testing.T) *fakeLaunchd {
	return &fakeLaunchd{
		t:       t,
		jobs:    map[string]*fakeJob{},
		manual:  map[string]string{},
		nextPid: 9000,
	}
}

// addDaemonJob loads a running daemon job at target that runs program with
// -socket socket and reports version over IPC.
func (fl *fakeLaunchd) addDaemonJob(target, program string, pid int, socket, version string) *fakeJob {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	j := &fakeJob{
		program: program,
		args: []string{program, "-registry", "34.71.57.205:9000", "-beacon", "34.71.57.205:9001",
			"-listen", ":4000", "-socket", socket, "-identity", "/Users/test/.pilot/identity.json", "-encrypt"},
		pid: pid, socket: socket, version: version, answers: pid > 0,
		respawn: true, newAnswers: true, lastExit: "0",
	}
	fl.jobs[target] = j
	return j
}

// addUpdaterJob loads a running updater job at target that runs program.
func (fl *fakeLaunchd) addUpdaterJob(target, program string, pid int) *fakeJob {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	j := &fakeJob{
		program: program,
		args:    []string{program, "-install-dir", filepath.Dir(program)},
		pid:     pid, respawn: true, lastExit: "0",
	}
	fl.jobs[target] = j
	return j
}

// addManualDaemon runs a daemon launchd does not manage on socket.
func (fl *fakeLaunchd) addManualDaemon(socket, version string) {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	fl.manual[socket] = version
}

// wire routes u's launchctl and IPC calls to the fake without changing the
// OS u restarts for.
func (fl *fakeLaunchd) wire(u *Updater) {
	u.runCmd = fl.run
	u.probeFn = fl.probe
	u.getuid = func() int { return fakeUID }
	u.restartWait = 2 * time.Second
	u.restartPoll = time.Millisecond
}

// attach wires u to the fake and puts it on the macOS restart path.
func (fl *fakeLaunchd) attach(u *Updater) {
	fl.wire(u)
	u.goos = "darwin"
}

func (fl *fakeLaunchd) kickstarts() []string {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	var out []string
	for _, c := range fl.calls {
		if strings.HasPrefix(c, "kickstart") {
			out = append(out, c)
		}
	}
	return out
}

func (fl *fakeLaunchd) job(target string) fakeJob {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	return *fl.jobs[target]
}

func notFound(target string) ([]byte, error) {
	label := target[strings.LastIndexByte(target, '/')+1:]
	return []byte(fmt.Sprintf("Could not find service %q in domain for user gui: %d\n", label, fakeUID)),
		errors.New("exit status 113")
}

func (fl *fakeLaunchd) run(name string, args ...string) ([]byte, error) {
	if name != "launchctl" {
		fl.t.Errorf("ran %s %q; the updater may only run launchctl", name, args)
		return nil, errors.New("not launchctl")
	}
	fl.mu.Lock()
	fl.calls = append(fl.calls, strings.Join(args, " "))
	fl.mu.Unlock()

	switch {
	case len(args) == 2 && args[0] == "print":
		fl.mu.Lock()
		defer fl.mu.Unlock()
		j, ok := fl.jobs[args[1]]
		if !ok {
			return notFound(args[1])
		}
		return []byte(fl.render(args[1], j)), nil

	case len(args) == 3 && args[0] == "kickstart" && args[1] == "-k":
		target := args[2]
		if hook := fl.onKickstart; hook != nil {
			if out, err := hook(target); err != nil {
				return out, err
			}
		}
		fl.mu.Lock()
		defer fl.mu.Unlock()
		j, ok := fl.jobs[target]
		if !ok {
			return notFound(target)
		}
		if j.kickErr != nil {
			return []byte(j.kickOut), j.kickErr
		}
		// launchd sends SIGTERM; the daemon exits 0.
		j.answers, j.lastExit = false, "0"
		if !j.respawn {
			j.pid = 0
			return nil, nil
		}
		oldPid := j.pid
		j.pid = fl.nextPid
		fl.nextPid++
		j.answers = j.newAnswers
		j.version = j.newVersion
		if j.version == "" && fl.installDir != "" {
			data, _ := os.ReadFile(filepath.Join(fl.installDir, ".pilot-version"))
			j.version = strings.TrimSpace(string(data))
		}
		if j.respawnDelay > 0 {
			newPid := j.pid
			j.pid = oldPid
			time.AfterFunc(j.respawnDelay, func() {
				fl.mu.Lock()
				defer fl.mu.Unlock()
				j.pid = newPid
			})
		}
		return nil, nil
	}
	fl.t.Errorf("unexpected launchctl %q", args)
	return []byte("Unrecognized subcommand"), errors.New("exit status 64")
}

// render prints j like `launchctl print` does.
func (fl *fakeLaunchd) render(target string, j *fakeJob) string {
	state := "not running"
	if j.pid > 0 {
		state = "running"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s = {\n", target)
	fmt.Fprintf(&b, "\tactive count = 1\n\tpath = /Users/test/Library/LaunchAgents/%s.plist\n", target[strings.LastIndexByte(target, '/')+1:])
	fmt.Fprintf(&b, "\ttype = LaunchAgent\n\tstate = %s\n\n", state)
	fmt.Fprintf(&b, "\tprogram = %s\n\targuments = {\n", j.program)
	for _, a := range j.args {
		fmt.Fprintf(&b, "\t\t%s\n", a)
	}
	b.WriteString("\t}\n\n")
	b.WriteString("\tinherited environment = {\n\t\tSSH_AUTH_SOCK => /private/tmp/com.apple.launchd.x/Listeners\n\t}\n\n")
	b.WriteString("\tdefault environment = {\n\t\tPATH => /usr/bin:/bin:/usr/sbin:/sbin\n\t}\n\n")
	b.WriteString("\tdomain = gui/501 [100023]\n\tasid = 100023\n\tminimum runtime = 10\n\texit timeout = 5\n\truns = 4\n")
	if j.pid > 0 {
		fmt.Fprintf(&b, "\tpid = %d\n", j.pid)
	}
	b.WriteString("\timmediate reason = inefficient\n\tforks = 0\n\texecs = 1\n")
	fmt.Fprintf(&b, "\tlast exit code = %s\n\n", j.lastExit)
	// Nested blocks repeat top-level key names.
	b.WriteString("\tresource coalition = {\n\t\tID = 2814\n\t\ttype = resource\n\t\tstate = active\n\t\tactive count = 1\n\t\tname = x\n\t}\n\n")
	b.WriteString("\tevent triggers = {\n\t\tx => {\n\t\t\tkeepalive = 0\n\t\t\tdescriptor = {\n\t\t\t\t\"pid\" => 1\n\t\t\t}\n\t\t}\n\t}\n\n")
	b.WriteString("\tproperties = keepalive | runatload | inferred program\n}\n")
	return b.String()
}

// probe answers an IPC info call on socket.
func (fl *fakeLaunchd) probe(socket string) (string, bool) {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	for _, j := range fl.jobs {
		if j.socket == socket && j.pid > 0 && j.answers {
			return j.version, true
		}
	}
	if v, ok := fl.manual[socket]; ok {
		return v, true
	}
	return "", false
}

// darwinUpdater returns an Updater on the macOS restart path whose install
// dir holds release installed, with the fake launchd attached.
func darwinUpdater(t *testing.T, installed string) (*Updater, *fakeLaunchd) {
	t.Helper()
	installDir := t.TempDir()
	for _, name := range []string{"pilot-daemon", "pilot-updater", "pilotctl"} {
		if err := os.WriteFile(filepath.Join(installDir, name), []byte(name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if installed != "" {
		if err := os.WriteFile(filepath.Join(installDir, ".pilot-version"), []byte(installed+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	fl := newFakeLaunchd(t)
	u := &Updater{config: Config{InstallDir: installDir}}
	fl.attach(u)
	return u, fl
}

// --- launchctl print parsing ---------------------------------------------

// A real `launchctl print` of a running LaunchAgent (macOS 26), trimmed.
const realPrintRunning = `gui/501/com.apple.bird = {
	active count = 8
	path = /System/Library/LaunchAgents/com.apple.bird.plist
	type = LaunchAgent
	state = running

	program = /System/Library/PrivateFrameworks/iCloudDriveCore.framework/Versions/A/Support/bird
	arguments = {
		/System/Library/PrivateFrameworks/iCloudDriveCore.framework/Versions/A/Support/bird
	}

	inherited environment = {
		SSH_AUTH_SOCK => /private/tmp/com.apple.launchd.PJSgmVbv5P/Listeners
	}

	domain = gui/501 [100023]
	asid = 100023
	minimum runtime = 10
	exit timeout = 5
	runs = 7
	pid = 46144
	immediate reason = ipc (mach)
	last exit code = 0

	event triggers = {
		com.apple.bird.finish-salting-partially-salted-directories => {
			keepalive = 0
			descriptor = {
				"RelatedApplications" => [
					0 = "com.apple.bird"
				]
				"NonRepeatingTask" => {
					"TrySchedulingBefore" => 86400
				}
			}
		}
	}

	resource coalition = {
		ID = 2814
		type = resource
		state = active
		active count = 1
	}
}
`

// A real `launchctl print` of a loaded LaunchAgent with no process.
const realPrintNotRunning = `gui/501/com.google.GoogleUpdater.wake = {
	active count = 0
	path = /Users/u/Library/LaunchAgents/com.google.GoogleUpdater.wake.plist
	type = LaunchAgent
	state = not running

	program = /Users/u/Library/Application Support/Google/GoogleUpdater/Current/GoogleUpdater.app/Contents/MacOS/GoogleUpdater
	arguments = {
		/Users/u/Library/Application Support/Google/GoogleUpdater/Current/GoogleUpdater.app/Contents/MacOS/GoogleUpdater
		--wake-all
		--enable-logging
	}

	domain = gui/501 [100023]
	runs = 168
	last exit code = 0
}
`

func TestParseLaunchdPrint(t *testing.T) {
	t.Parallel()
	j, ok := parseLaunchdPrint(realPrintRunning)
	if !ok {
		t.Fatal("running job not parsed")
	}
	if j.state != "running" || j.pid != 46144 || j.lastExit != "0" ||
		j.program != "/System/Library/PrivateFrameworks/iCloudDriveCore.framework/Versions/A/Support/bird" ||
		len(j.args) != 1 || j.args[0] != j.program {
		t.Errorf("running job = %+v", j)
	}

	j, ok = parseLaunchdPrint(realPrintNotRunning)
	if !ok {
		t.Fatal("not-running job not parsed")
	}
	wantProg := "/Users/u/Library/Application Support/Google/GoogleUpdater/Current/GoogleUpdater.app/Contents/MacOS/GoogleUpdater"
	if j.state != "not running" || j.pid != 0 || j.program != wantProg ||
		strings.Join(j.args[1:], " ") != "--wake-all --enable-logging" {
		t.Errorf("not-running job = %+v", j)
	}

	// The fake renders nested blocks that repeat "state" and "pid".
	fl := newFakeLaunchd(t)
	fj := fl.addDaemonJob(daemonTarget, "/Users/u/.pilot/bin/pilot-daemon", 812, "/tmp/pilot.sock", "v1.0.0")
	j, ok = parseLaunchdPrint(fl.render(daemonTarget, fj))
	if !ok || j.state != "running" || j.pid != 812 || j.program != "/Users/u/.pilot/bin/pilot-daemon" || j.socket() != "/tmp/pilot.sock" {
		t.Errorf("fake job = %+v (%v)", j, ok)
	}

	// Without "program = ", the first argument is the program.
	j, ok = parseLaunchdPrint("gui/1/x = {\n\targuments = {\n\t\t/bin/x\n\t\t-y\n\t}\n}\n")
	if !ok || j.programPath() != "/bin/x" {
		t.Errorf("arguments-only job = %+v (%v)", j, ok)
	}

	for _, bad := range []string{
		"",
		"Could not find service \"network.pilotprotocol.pilot-daemon\" in domain for user gui: 501\n",
		"Bad request.\nCould not find domain for port identifier.\n",
		"gui/1/x = {\n\tstate = running\n}\n", // no program
	} {
		if j, ok := parseLaunchdPrint(bad); ok {
			t.Errorf("parseLaunchdPrint(%q) = %+v, want not a job", bad, j)
		}
	}
}

func TestSocketFromArgs(t *testing.T) {
	t.Parallel()
	def := socketFromArgs(nil)
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"/bin/pilot-daemon", "-listen", ":4000", "-socket", "/tmp/a.sock", "-encrypt"}, "/tmp/a.sock"},
		{[]string{"/bin/pilot-daemon", "--socket", "/tmp/b.sock"}, "/tmp/b.sock"},
		{[]string{"/bin/pilot-daemon", "-socket=/tmp/c.sock"}, "/tmp/c.sock"},
		{[]string{"/bin/pilot-daemon", "--socket=/tmp/d.sock"}, "/tmp/d.sock"},
		{[]string{"/bin/pilot-daemon", "-encrypt"}, def},
		{[]string{"/bin/pilot-daemon", "-socket"}, def},
		{[]string{"-socket"}, def}, // args[0] is the program, never a flag
	}
	for _, tc := range cases {
		if got := socketFromArgs(tc.args); got != tc.want {
			t.Errorf("socketFromArgs(%q) = %q, want %q", tc.args, got, tc.want)
		}
	}
}

// --- daemon restart on macOS ----------------------------------------------

// TestDarwinRestart_KickstartsJobAndConfirmsVersion is the fix for the
// reported gap: the daemon runs under its launchd job from the replaced
// binary, so the updater kickstarts it and waits for the new process to
// answer over IPC with the installed version.
func TestDarwinRestart_KickstartsJobAndConfirmsVersion(t *testing.T) {
	t.Parallel()
	u, fl := darwinUpdater(t, "v1.13.10")
	daemon := filepath.Join(u.config.InstallDir, "pilot-daemon")
	j := fl.addDaemonJob(daemonTarget, daemon, 700, fakeSocket, "v1.13.9")
	j.newVersion = "v1.13.10"
	j.respawnDelay = 20 * time.Millisecond // launchd still shows the old pid for a while

	r := u.signalDaemonRestart()
	if r.err != nil || !r.restarted {
		t.Fatalf("outcome = %+v, want restarted", r)
	}
	if r.by != "launchd "+daemonTarget || r.version != "v1.13.10" {
		t.Errorf("by/version = %q/%q", r.by, r.version)
	}
	if ks := fl.kickstarts(); len(ks) != 1 || ks[0] != "kickstart -k "+daemonTarget {
		t.Errorf("kickstarts = %q, want one of %s", ks, daemonTarget)
	}
	if got := fl.job(daemonTarget); got.pid == 700 {
		t.Error("daemon still on the old pid")
	}
}

// TestDarwinRestart_AcceptsSymlinkedProgram: the job may name the binary
// through a symlink (e.g. a Homebrew-style bin link).
func TestDarwinRestart_AcceptsSymlinkedProgram(t *testing.T) {
	t.Parallel()
	u, fl := darwinUpdater(t, "v2.0.0")
	link := filepath.Join(t.TempDir(), "pilot-daemon")
	if err := os.Symlink(filepath.Join(u.config.InstallDir, "pilot-daemon"), link); err != nil {
		t.Fatal(err)
	}
	fl.addDaemonJob(daemonTarget, link, 700, fakeSocket, "v1.0.0").newVersion = "v2.0.0"
	if r := u.signalDaemonRestart(); r.err != nil || !r.restarted {
		t.Fatalf("outcome = %+v, want restarted", r)
	}
}

// TestDarwinRestart_LegacyLabelInUserDomain: the job is found under the old
// com.vulturelabs label and in the background (user/<uid>) domain.
func TestDarwinRestart_LegacyLabelInUserDomain(t *testing.T) {
	t.Parallel()
	u, fl := darwinUpdater(t, "v2.0.0")
	const target = "user/501/com.vulturelabs.pilot-daemon"
	fl.addDaemonJob(target, filepath.Join(u.config.InstallDir, "pilot-daemon"), 700, fakeSocket, "v1.0.0").newVersion = "v2.0.0"
	r := u.signalDaemonRestart()
	if r.err != nil || r.by != "launchd "+target {
		t.Fatalf("outcome = %+v", r)
	}
	if ks := fl.kickstarts(); len(ks) != 1 || ks[0] != "kickstart -k "+target {
		t.Errorf("kickstarts = %q", ks)
	}
}

// TestDarwinRestart_SudoUsesInvokingUsersDomain: `sudo pilotctl update` runs
// as uid 0; the jobs live in the invoking user's domain. Not parallel:
// it sets SUDO_UID.
func TestDarwinRestart_SudoUsesInvokingUsersDomain(t *testing.T) {
	t.Setenv("SUDO_UID", "501")
	u, fl := darwinUpdater(t, "v2.0.0")
	u.getuid = func() int { return 0 }
	fl.addDaemonJob(daemonTarget, filepath.Join(u.config.InstallDir, "pilot-daemon"), 700, fakeSocket, "v1.0.0").newVersion = "v2.0.0"
	if r := u.signalDaemonRestart(); r.err != nil || r.by != "launchd "+daemonTarget {
		t.Fatalf("outcome = %+v", r)
	}
	if got := u.launchdDomains(); strings.Join(got, " ") != "gui/501 user/501 gui/0 user/0" {
		t.Errorf("domains = %q", got)
	}
}

// TestDarwinRestart_NoJob covers a daemon launchd does not run: never
// kickstarted; reported only when it answers with an older version.
func TestDarwinRestart_NoJob(t *testing.T) {
	t.Parallel()
	def := socketFromArgs(nil)
	cases := []struct {
		name    string
		manual  string // version of a hand-started daemon on the default socket ("" = none)
		wantErr string
	}{
		{"daemon not running", "", ""},
		{"hand-started daemon on the old version", "v1.0.0", "no launchd job network.pilotprotocol.pilot-daemon is loaded"},
		{"hand-started daemon that does not report a version", "-", "unknown version"},
		{"hand-started daemon already on the installed version", "v2.0.0", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			u, fl := darwinUpdater(t, "v2.0.0")
			if tc.manual != "" {
				fl.addManualDaemon(def, strings.TrimPrefix(tc.manual, "-"))
			}
			r := u.signalDaemonRestart()
			if r.restarted {
				t.Error("restarted = true, want false")
			}
			switch {
			case tc.wantErr == "" && r.err != nil:
				t.Errorf("err = %v, want nil", r.err)
			case tc.wantErr != "" && (r.err == nil || !strings.Contains(r.err.Error(), tc.wantErr) ||
				!strings.Contains(r.err.Error(), "restart it with: pilotctl daemon stop && pilotctl daemon start")):
				t.Errorf("err = %v, want mention of %q and the restart command", r.err, tc.wantErr)
			}
			if ks := fl.kickstarts(); len(ks) != 0 {
				t.Errorf("kickstarts = %q; a daemon launchd does not run must never be restarted", ks)
			}
		})
	}
}

// TestDarwinRestart_JobLoadedButNotRunning: the job exited (or was never
// started), so launchd starts the new binary next time; a daemon answering
// on the job's socket was started by hand and is reported instead.
func TestDarwinRestart_JobLoadedButNotRunning(t *testing.T) {
	t.Parallel()
	u, fl := darwinUpdater(t, "v2.0.0")
	fl.addDaemonJob(daemonTarget, filepath.Join(u.config.InstallDir, "pilot-daemon"), 0, fakeSocket, "")
	if r := u.signalDaemonRestart(); r.err != nil || r.restarted {
		t.Errorf("idle job: outcome = %+v, want nothing to do", r)
	}

	fl.addManualDaemon(fakeSocket, "v1.0.0")
	r := u.signalDaemonRestart()
	if r.err == nil || !strings.Contains(r.err.Error(), "is loaded but runs no process") || !strings.Contains(r.err.Error(), fakeSocket) {
		t.Errorf("hand-started daemon: err = %v", r.err)
	}
	if ks := fl.kickstarts(); len(ks) != 0 {
		t.Errorf("kickstarts = %q, want none", ks)
	}
}

// TestDarwinRestart_JobRunsAnotherBinary: the job starts some other
// pilot-daemon, so restarting it would not run the new release.
func TestDarwinRestart_JobRunsAnotherBinary(t *testing.T) {
	t.Parallel()
	u, fl := darwinUpdater(t, "v2.0.0")
	fl.addDaemonJob(daemonTarget, "/usr/local/bin/pilot-daemon", 700, fakeSocket, "v1.0.0")
	r := u.signalDaemonRestart()
	if r.err == nil || !strings.Contains(r.err.Error(), `runs "/usr/local/bin/pilot-daemon"`) {
		t.Errorf("err = %v, want the other program named", r.err)
	}
	if ks := fl.kickstarts(); len(ks) != 0 {
		t.Errorf("kickstarts = %q, want none", ks)
	}
}

// TestDarwinRestart_Failures: kickstart fails, launchd does not start the
// job again, or the new daemon never answers with the installed version.
// Each is recorded with how to restart the daemon.
func TestDarwinRestart_Failures(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(j *fakeJob)
		want  []string
	}{
		{"kickstart refused", func(j *fakeJob) {
			j.kickErr, j.kickOut = errors.New("exit status 150"), "Operation not permitted while System Integrity Protection is engaged"
		}, []string{"launchctl kickstart -k " + daemonTarget, "exit status 150", "Operation not permitted"}},
		{"launchd does not start it again", func(j *fakeJob) { j.respawn = false },
			[]string{"did not start the daemon again", "state = not running", "start it with: launchctl kickstart " + daemonTarget}},
		{"new daemon never answers", func(j *fakeJob) { j.newAnswers = false },
			[]string{"did not answer on " + fakeSocket + " with v2.0.0", "no daemon answered", "restart it with: launchctl kickstart -k"}},
		{"new daemon reports another version", func(j *fakeJob) { j.newVersion = "v1.0.0" },
			[]string{"the daemon answered with v1.0.0"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			u, fl := darwinUpdater(t, "v2.0.0")
			u.restartWait = 50 * time.Millisecond
			j := fl.addDaemonJob(daemonTarget, filepath.Join(u.config.InstallDir, "pilot-daemon"), 700, fakeSocket, "v1.0.0")
			j.newVersion = "v2.0.0"
			tc.setup(j)
			r := u.signalDaemonRestart()
			if r.err == nil || r.restarted {
				t.Fatalf("outcome = %+v, want a failure", r)
			}
			for _, w := range tc.want {
				if !strings.Contains(r.err.Error(), w) {
					t.Errorf("err = %v\nwant it to contain %q", r.err, w)
				}
			}
		})
	}
}

// TestDarwinRestart_StopAbortsWait: Stop() must not wait out the restart
// window.
func TestDarwinRestart_StopAbortsWait(t *testing.T) {
	t.Parallel()
	u, fl := darwinUpdater(t, "v2.0.0")
	u.restartWait = time.Minute
	fl.addDaemonJob(daemonTarget, filepath.Join(u.config.InstallDir, "pilot-daemon"), 700, fakeSocket, "v1.0.0").respawn = false
	u.stopCh = make(chan struct{})
	close(u.stopCh)
	done := make(chan error, 1)
	go func() { done <- u.signalDaemonRestart().err }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "updater stopped") {
			t.Errorf("err = %v, want stopped", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("did not return after Stop")
	}
}

// TestDarwinRestart_NeverRestartsItsOwnProcess: if the daemon job's process
// is this process, kickstart would kill the caller.
func TestDarwinRestart_NeverRestartsItsOwnProcess(t *testing.T) {
	t.Parallel()
	u, fl := darwinUpdater(t, "v2.0.0")
	fl.addDaemonJob(daemonTarget, filepath.Join(u.config.InstallDir, "pilot-daemon"), os.Getpid(), fakeSocket, "v1.0.0")
	if r := u.signalDaemonRestart(); r.err == nil || !strings.Contains(r.err.Error(), "the updater runs inside it") {
		t.Errorf("outcome = %+v", r)
	}
	if ks := fl.kickstarts(); len(ks) != 0 {
		t.Errorf("kickstarts = %q, want none", ks)
	}
}

// --- updater service restart on macOS -------------------------------------

func TestRestartUpdaterService(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		setup     func(u *Updater, fl *fakeLaunchd)
		attempted bool
		restarted bool
		wantErr   string
	}{
		{"restarts the launchd job", func(u *Updater, fl *fakeLaunchd) {
			fl.addUpdaterJob(updaterTarget, filepath.Join(u.config.InstallDir, "pilot-updater"), 600)
		}, true, true, ""},
		{"no job loaded", func(*Updater, *fakeLaunchd) {}, false, false, ""},
		{"job loaded, no process", func(u *Updater, fl *fakeLaunchd) {
			fl.addUpdaterJob(updaterTarget, filepath.Join(u.config.InstallDir, "pilot-updater"), 0)
		}, false, false, ""},
		{"this process is the job", func(u *Updater, fl *fakeLaunchd) {
			fl.addUpdaterJob(updaterTarget, filepath.Join(u.config.InstallDir, "pilot-updater"), os.Getpid())
		}, false, false, ""},
		{"job runs another binary", func(u *Updater, fl *fakeLaunchd) {
			fl.addUpdaterJob(updaterTarget, "/opt/other/pilot-updater", 600)
		}, true, false, `runs "/opt/other/pilot-updater"`},
		{"kickstart fails", func(u *Updater, fl *fakeLaunchd) {
			j := fl.addUpdaterJob(updaterTarget, filepath.Join(u.config.InstallDir, "pilot-updater"), 600)
			j.kickErr, j.kickOut = errors.New("exit status 1"), "boom"
		}, true, false, "restart updater service (launchctl kickstart -k " + updaterTarget + "): exit status 1 boom"},
		{"launchd does not start it again", func(u *Updater, fl *fakeLaunchd) {
			fl.addUpdaterJob(updaterTarget, filepath.Join(u.config.InstallDir, "pilot-updater"), 600).respawn = false
		}, true, false, "did not start the updater again"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			u, fl := darwinUpdater(t, "v2.0.0")
			u.restartWait = 50 * time.Millisecond
			tc.setup(u, fl)
			r, attempted := u.restartUpdaterService()
			if attempted != tc.attempted || r.restarted != tc.restarted {
				t.Errorf("attempted/restarted = %v/%v, want %v/%v (%+v)", attempted, r.restarted, tc.attempted, tc.restarted, r)
			}
			switch {
			case tc.wantErr == "" && r.err != nil:
				t.Errorf("err = %v", r.err)
			case tc.wantErr != "" && (r.err == nil || !strings.Contains(r.err.Error(), tc.wantErr)):
				t.Errorf("err = %v, want %q", r.err, tc.wantErr)
			}
			if !tc.attempted || tc.wantErr == `runs "/opt/other/pilot-updater"` {
				if ks := fl.kickstarts(); len(ks) != 0 {
					t.Errorf("kickstarts = %q, want none", ks)
				}
			}
		})
	}

	// Not macOS: nothing to do.
	u := &Updater{config: Config{InstallDir: t.TempDir()}, goos: "linux"}
	if _, attempted := u.restartUpdaterService(); attempted {
		t.Error("linux: attempted = true")
	}
}

// --- end to end through RunOnce / the loop --------------------------------

// darwinReleaseUpdater is a RunOnce-ready Updater on the macOS restart path
// whose install is at v1.0.0 and whose release server offers v2.0.0 with
// new daemon and updater binaries. The daemon and updater launchd jobs run
// the installed binaries.
func darwinReleaseUpdater(t *testing.T) (*Updater, *fakeLaunchd, string) {
	t.Helper()
	srv := newReleaseServer(t, "v2.0.0", map[string]string{
		"daemon":   "new-daemon",
		"updater":  "new-updater",
		"pilotctl": "new-pilotctl",
	})
	u, statusPath := newTestUpdater(t, srv.Server, "v1.0.0")
	fl := newFakeLaunchd(t)
	fl.installDir = u.config.InstallDir
	fl.attach(u)
	fl.addDaemonJob(daemonTarget, filepath.Join(u.config.InstallDir, "pilot-daemon"), 700, fakeSocket, "v1.0.0")
	fl.addUpdaterJob(updaterTarget, filepath.Join(u.config.InstallDir, "pilot-updater"), 600)
	return u, fl, statusPath
}

// TestRunOnce_DarwinRestartsDaemonAndUpdaterService reproduces the report
// end to end: `pilotctl update` installs a release that replaces
// pilot-daemon, pilotctl and pilot-updater. The daemon's launchd job is
// kickstarted and confirmed on the new version over IPC, then the updater
// job is kickstarted so the updater service runs the new binary too. The
// status file records both, and a restart_error left by an earlier run is
// cleared although the release replaced pilot-updater (updater v0.2.5 kept
// it).
func TestRunOnce_DarwinRestartsDaemonAndUpdaterService(t *testing.T) {
	t.Parallel()
	u, fl, statusPath := darwinReleaseUpdater(t)
	if err := writeStatusFile(statusPath, Status{RestartError: "restart daemon (launchctl kickstart -k x): exit status 113"}); err != nil {
		t.Fatal(err)
	}

	if err := u.RunOnce(); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if ks := fl.kickstarts(); len(ks) != 2 || ks[0] != "kickstart -k "+daemonTarget || ks[1] != "kickstart -k "+updaterTarget {
		t.Errorf("kickstarts = %q, want the daemon then the updater", ks)
	}
	st := mustReadStatus(t, statusPath)
	if st.LastResult != ResultUpdated || st.CurrentVersion != "v2.0.0" {
		t.Errorf("result = %s %s", st.LastResult, st.CurrentVersion)
	}
	if st.RestartError != "" || st.UpdaterRestartError != "" {
		t.Errorf("restart errors = %q / %q, want none", st.RestartError, st.UpdaterRestartError)
	}
	if st.DaemonRestartedAt.IsZero() || st.DaemonRestartedBy != "launchd "+daemonTarget || st.DaemonRestartedVersion != "v2.0.0" {
		t.Errorf("daemon restart = %v %q %q", st.DaemonRestartedAt, st.DaemonRestartedBy, st.DaemonRestartedVersion)
	}
	if st.UpdaterRestartedAt.IsZero() {
		t.Error("updater_restarted_at not recorded")
	}
	// The restart record is newer than the daemon binary, so the restarted
	// updater service does not restart the daemon a second time.
	before := len(fl.kickstarts())
	u.recoverPendingRestart()
	if after := len(fl.kickstarts()); after != before {
		t.Errorf("recoverPendingRestart kickstarted again (%d -> %d)", before, after)
	}
}

// TestRunOnce_DarwinDaemonNotUnderLaunchd: a hand-started daemon is left
// running and reported; the updater service is still restarted.
func TestRunOnce_DarwinDaemonNotUnderLaunchd(t *testing.T) {
	t.Parallel()
	u, fl, statusPath := darwinReleaseUpdater(t)
	fl.mu.Lock()
	delete(fl.jobs, daemonTarget)
	fl.mu.Unlock()
	fl.addManualDaemon(socketFromArgs(nil), "v1.0.0")

	if err := u.RunOnce(); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if ks := fl.kickstarts(); len(ks) != 1 || ks[0] != "kickstart -k "+updaterTarget {
		t.Errorf("kickstarts = %q, want only the updater job", ks)
	}
	st := mustReadStatus(t, statusPath)
	if st.LastResult != ResultUpdated || !strings.Contains(st.RestartError, "left running the old version (v1.0.0)") {
		t.Errorf("status = %s / %q", st.LastResult, st.RestartError)
	}
	if !st.DaemonRestartedAt.IsZero() {
		t.Error("daemon_restarted_at recorded without a restart")
	}
}

// TestRunOnce_DarwinUpdaterRestartFailureRecorded: the daemon restart works
// but the updater job cannot be restarted.
func TestRunOnce_DarwinUpdaterRestartFailureRecorded(t *testing.T) {
	t.Parallel()
	u, fl, statusPath := darwinReleaseUpdater(t)
	fl.onKickstart = func(target string) ([]byte, error) {
		if target == updaterTarget {
			return []byte("Could not kickstart service"), errors.New("exit status 5")
		}
		return nil, nil
	}
	if err := u.RunOnce(); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	st := mustReadStatus(t, statusPath)
	if st.RestartError != "" || !strings.Contains(st.UpdaterRestartError, "exit status 5 Could not kickstart service") {
		t.Errorf("restart errors = %q / %q", st.RestartError, st.UpdaterRestartError)
	}
	if !st.UpdaterRestartedAt.IsZero() {
		t.Error("updater_restarted_at recorded for a failed restart")
	}
}

// TestLoop_DarwinSelfUpdateLeavesRestartsToSuccessor: the loop (the updater
// service itself) replaced its own binary. It exits for launchd to start it
// again and restarts nothing: the new process does the daemon.
func TestLoop_DarwinSelfUpdateLeavesRestartsToSuccessor(t *testing.T) {
	t.Parallel()
	u, fl, statusPath := darwinReleaseUpdater(t)
	var exits atomic.Int32
	u.exitFn = func(int) { exits.Add(1) }
	if err := u.checkOnce(); err != nil {
		t.Fatalf("checkOnce: %v", err)
	}
	if exits.Load() != 1 {
		t.Errorf("exits = %d, want 1", exits.Load())
	}
	if ks := fl.kickstarts(); len(ks) != 0 {
		t.Errorf("kickstarts = %q, want none from the stale updater", ks)
	}

	// Its successor restarts the daemon on startup and records it. (The
	// test's restart record is dated in the future; date it before the
	// update, as it would be.)
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(u.config.InstallDir, ".daemon-last-restart"), past, past); err != nil {
		t.Fatal(err)
	}
	u2 := &Updater{config: u.config}
	fl.attach(u2)
	u2.recoverPendingRestart()
	if ks := fl.kickstarts(); len(ks) != 1 || ks[0] != "kickstart -k "+daemonTarget {
		t.Errorf("kickstarts = %q, want the daemon", ks)
	}
	if st := mustReadStatus(t, statusPath); st.RestartError != "" || st.DaemonRestartedVersion != "v2.0.0" {
		t.Errorf("status = %q / %q", st.RestartError, st.DaemonRestartedVersion)
	}
}

// TestRunCheck_DarwinClearsRestartErrorOnceDaemonIsCurrent: after a
// restart failure, a later check that finds the daemon answering with the
// installed version clears restart_error (before, macOS kept it until the
// next release).
func TestRunCheck_DarwinClearsRestartErrorOnceDaemonIsCurrent(t *testing.T) {
	t.Parallel()
	srv := newReleaseServer(t, "v1.0.0", map[string]string{"daemon": "d"})
	u, statusPath := newTestUpdater(t, srv.Server, "v1.0.0")
	fl := newFakeLaunchd(t)
	fl.attach(u)
	j := fl.addDaemonJob(daemonTarget, filepath.Join(u.config.InstallDir, "pilot-daemon"), 700, fakeSocket, "v0.9.0")
	const restartErr = "daemon left running the old version"
	if err := writeStatusFile(statusPath, Status{RestartError: restartErr}); err != nil {
		t.Fatal(err)
	}

	if err := u.RunOnce(); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if st := mustReadStatus(t, statusPath); st.RestartError != restartErr {
		t.Fatalf("restart_error = %q, want kept while the daemon reports v0.9.0", st.RestartError)
	}

	fl.mu.Lock()
	j.version = "v1.0.0" // restarted by hand
	fl.mu.Unlock()
	if err := u.RunOnce(); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if st := mustReadStatus(t, statusPath); st.RestartError != "" {
		t.Errorf("restart_error = %q, want cleared", st.RestartError)
	}
	if ks := fl.kickstarts(); len(ks) != 0 {
		t.Errorf("kickstarts = %q; an up-to-date check restarts nothing", ks)
	}

	// With no restart_error on record, an up-to-date check does not even
	// ask launchd or the daemon.
	fl.mu.Lock()
	fl.calls = nil
	fl.mu.Unlock()
	if err := u.RunOnce(); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	fl.mu.Lock()
	calls := fl.calls
	fl.mu.Unlock()
	if len(calls) != 0 {
		t.Errorf("launchctl calls = %q, want none", calls)
	}
}
