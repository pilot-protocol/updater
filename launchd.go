// SPDX-License-Identifier: AGPL-3.0-or-later

package updater

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pilot-protocol/common/driver"
)

// Restarting the daemon on macOS.
//
// install.sh runs the daemon as the LaunchAgent
// network.pilotprotocol.pilot-daemon (KeepAlive SuccessfulExit=false) and the
// updater as network.pilotprotocol.pilot-updater (KeepAlive true). After an
// update the updater restarts the daemon with
//
//	launchctl kickstart -k gui/<uid>/network.pilotprotocol.pilot-daemon
//
// launchd sends the running daemon SIGTERM (its graceful shutdown stops the
// apps it runs) and starts the job again, which execs the new binary.
// kickstart restarts the job whatever its KeepAlive policy says.
//
// That is only safe, and only useful, when launchd runs this daemon. So the
// updater first reads the job with `launchctl print` and restarts it only when
// the job is running and its program is the binary that was just replaced.
// Afterwards it waits for launchd to run a new process and for that process to
// answer over IPC with the installed version. Otherwise, as on Linux, the
// daemon is left running and restart_error in the status file says why and
// how to restart it:
//
//   - no launchd job is loaded (the daemon was started by hand, or not at
//     all): a daemon that answers on the socket with an older version is
//     reported; one that does not answer is not running, and its next start
//     uses the new binary;
//   - the job starts another pilot-daemon (a second installation): restarting
//     it would not run the new release;
//   - the job did not come back, or the new process did not answer with the
//     installed version in time.
//
// A one-shot RunOnce (`pilotctl update`) that replaced pilot-updater also
// restarts the updater job, so the updater service runs the new binary too.
// The loop never restarts its own job: it exits and launchd starts it again.

// Labels of the LaunchAgents install.sh writes, current first. install.sh
// renamed com.vulturelabs.* to network.pilotprotocol.*; pilotctl and the
// uninstaller still accept the old names, and so does the updater.
var (
	daemonLaunchdLabels  = []string{"network.pilotprotocol.pilot-daemon", "com.vulturelabs.pilot-daemon"}
	updaterLaunchdLabels = []string{"network.pilotprotocol.pilot-updater", "com.vulturelabs.pilot-updater"}
)

const (
	// defaultLaunchdRestartWait bounds how long the updater waits, after
	// `launchctl kickstart -k`, for launchd to start the job again and (for
	// the daemon) for the new daemon to answer over IPC. The daemon's
	// shutdown takes up to ~5 s, and a daemon that starts installed apps
	// first can take well over 10 s to open its socket (`pilotctl daemon
	// start` waits 30 s for it).
	defaultLaunchdRestartWait = 60 * time.Second

	// daemonProbeTimeout bounds one IPC info call, so a wedged daemon cannot
	// hang the updater.
	daemonProbeTimeout = 3 * time.Second
)

// launchdJob is the part of `launchctl print <target>` the updater uses.
type launchdJob struct {
	target  string   // domain/label, e.g. gui/501/network.pilotprotocol.pilot-daemon
	state   string   // "running", "not running", "spawn scheduled", ...
	program string   // "program = ..." (the executable launchd starts)
	args    []string // "arguments = { ... }"
	pid     int      // 0 when no process is running
	// lastExit is "last exit code = ..." (or "last exit reason"), for messages.
	lastExit string
}

// programPath is the executable the job starts.
func (j launchdJob) programPath() string {
	if j.program != "" {
		return j.program
	}
	if len(j.args) > 0 {
		return j.args[0]
	}
	return ""
}

// socket is the daemon's IPC socket: the -socket argument of the job, or the
// daemon's default.
func (j launchdJob) socket() string {
	return socketFromArgs(j.args)
}

// describe summarises the job's state for error messages.
func (j launchdJob) describe() string {
	s := "state = " + orNone(j.state)
	if j.pid > 0 {
		s += fmt.Sprintf(", pid = %d", j.pid)
	}
	if j.lastExit != "" {
		s += ", last exit = " + j.lastExit
	}
	return s
}

func orNone(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// socketFromArgs returns the value of the daemon's -socket flag in args (Go
// flag syntax: -socket X, --socket X, -socket=X), or the daemon's default
// socket when the flag is absent.
func socketFromArgs(args []string) string {
	for i := 1; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-socket" || a == "--socket":
			if i+1 < len(args) {
				return args[i+1]
			}
		case strings.HasPrefix(a, "-socket="):
			return strings.TrimPrefix(a, "-socket=")
		case strings.HasPrefix(a, "--socket="):
			return strings.TrimPrefix(a, "--socket=")
		}
	}
	return driver.DefaultSocketPath()
}

// parseLaunchdPrint parses the output of `launchctl print <domain>/<label>`:
//
//	gui/501/network.pilotprotocol.pilot-daemon = {
//		active count = 1
//		path = /Users/u/Library/LaunchAgents/network.pilotprotocol.pilot-daemon.plist
//		state = running
//		program = /Users/u/.pilot/bin/pilot-daemon
//		arguments = {
//			/Users/u/.pilot/bin/pilot-daemon
//			-socket
//			/tmp/pilot.sock
//		}
//		pid = 812
//		last exit code = 0
//		resource coalition = {
//			state = active
//		}
//	}
//
// Only keys of the job itself (depth 1) are read: nested blocks repeat names
// such as "state". ok is false when out is not a job description.
func parseLaunchdPrint(out string) (job launchdJob, ok bool) {
	depth := 0
	inArgs := false
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if depth == 0 {
			// The header: "<target> = {".
			if !strings.HasSuffix(line, "= {") {
				return launchdJob{}, false
			}
			depth = 1
			continue
		}
		if line == "}" {
			depth--
			inArgs = false
			if depth == 0 {
				break
			}
			continue
		}
		if inArgs {
			job.args = append(job.args, line)
			continue
		}
		key, value, hasEq := strings.Cut(line, " = ")
		opens := strings.HasSuffix(line, "{")
		if depth == 1 && hasEq {
			switch key {
			case "state":
				job.state = value
			case "program":
				job.program = value
			case "pid":
				job.pid, _ = strconv.Atoi(value)
			case "last exit code", "last exit reason":
				if job.lastExit == "" {
					job.lastExit = value
				}
			case "arguments":
				if opens {
					inArgs = true
					depth++
					continue
				}
			}
		}
		if opens {
			depth++
		}
	}
	if job.programPath() == "" {
		return launchdJob{}, false
	}
	return job, true
}

// launchctl runs launchctl through the injectable command hook. It is the
// only external command the updater runs.
func (u *Updater) launchctl(args ...string) ([]byte, error) {
	return u.command("launchctl", args...)
}

// uid is the user whose launchd domains hold the Pilot jobs.
func (u *Updater) uid() int {
	if u.getuid != nil {
		return u.getuid()
	}
	return os.Getuid()
}

// launchdDomains lists the launchd domains to look for the Pilot jobs in, in
// order: the login session (gui/<uid>), where install.sh and `pilotctl daemon
// start` load them, then the background session (user/<uid>). Under sudo
// (uid 0) the invoking user's domains come first.
func (u *Updater) launchdDomains() []string {
	uids := []int{u.uid()}
	if uids[0] == 0 {
		if s := os.Getenv("SUDO_UID"); s != "" {
			if n, err := strconv.Atoi(s); err == nil && n > 0 {
				uids = []int{n, 0}
			}
		}
	}
	var domains []string
	for _, id := range uids {
		domains = append(domains, fmt.Sprintf("gui/%d", id), fmt.Sprintf("user/%d", id))
	}
	return domains
}

// printLaunchdJob reads target with `launchctl print`. ok is false when the
// job is not loaded there (or launchctl is unavailable).
func (u *Updater) printLaunchdJob(target string) (launchdJob, bool) {
	out, err := u.launchctl("print", target)
	if err != nil {
		return launchdJob{}, false
	}
	job, ok := parseLaunchdPrint(string(out))
	if !ok {
		return launchdJob{}, false
	}
	job.target = target
	return job, true
}

// findLaunchdJob returns the first loaded job with one of labels, looking in
// every domain of launchdDomains.
func (u *Updater) findLaunchdJob(labels []string) (launchdJob, bool) {
	for _, label := range labels {
		for _, domain := range u.launchdDomains() {
			if job, ok := u.printLaunchdJob(domain + "/" + label); ok {
				return job, true
			}
		}
	}
	return launchdJob{}, false
}

// launchdWait is how long to wait for a job restarted with kickstart -k.
func (u *Updater) launchdWait() time.Duration {
	if u.restartWait > 0 {
		return u.restartWait
	}
	return defaultLaunchdRestartWait
}

// pollInterval is the pause between checks while waiting for a restart.
func (u *Updater) pollInterval() time.Duration {
	if u.restartPoll > 0 {
		return u.restartPoll
	}
	return defaultRestartPoll
}

// pause sleeps for d. It returns false when the updater was stopped.
func (u *Updater) pause(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-u.stopCh:
		return false
	}
}

// waitForLaunchdRespawn waits until launchd runs a process for target other
// than oldPid, and returns its pid.
func (u *Updater) waitForLaunchdRespawn(target string, oldPid int, deadline time.Time) (int, error) {
	last := "not loaded"
	for {
		if job, ok := u.printLaunchdJob(target); ok {
			if job.pid > 0 && job.pid != oldPid {
				return job.pid, nil
			}
			last = job.describe()
		} else {
			last = "not loaded"
		}
		if !time.Now().Before(deadline) {
			return 0, fmt.Errorf("launchd shows %s", last)
		}
		if !u.pause(u.pollInterval()) {
			return 0, fmt.Errorf("updater stopped while waiting (launchd shows %s)", last)
		}
	}
}

// probeDaemonVersion asks the daemon listening on socket for its version (IPC
// info). running reports whether a daemon answered on the socket; version is
// "" when the info call failed or timed out. Tests replace it so they never
// reach a real daemon.
var probeDaemonVersion = func(socket string) (version string, running bool) {
	d, err := driver.Connect(socket)
	if err != nil {
		return "", false
	}
	ch := make(chan string, 1)
	go func() {
		info, err := d.Info()
		v := ""
		if err == nil {
			v, _ = info["version"].(string)
		}
		ch <- v
	}()
	timer := time.NewTimer(daemonProbeTimeout)
	defer timer.Stop()
	select {
	case v := <-ch:
		_ = d.Close()
		return v, true
	case <-timer.C:
		_ = d.Close() // unblocks the Info call
		return "", true
	}
}

// probe asks the daemon on socket for its version via the injectable hook.
func (u *Updater) probe(socket string) (string, bool) {
	if u.probeFn != nil {
		return u.probeFn(socket)
	}
	return probeDaemonVersion(socket)
}

// installedVersion is the release recorded in InstallDir/.pilot-version, or
// "" when there is none.
func (u *Updater) installedVersion() string {
	data, err := os.ReadFile(filepath.Join(u.config.InstallDir, ".pilot-version"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// sameRelease reports whether two version strings name the same release
// ("v1.13.10" and "1.13.10" do). A version that does not parse, such as a
// "dev" build, matches nothing.
func sameRelease(a, b string) bool {
	va, errA := ParseSemver(a)
	vb, errB := ParseSemver(b)
	return errA == nil && errB == nil && va.Compare(vb) == 0
}

// runsInstalled reports whether a daemon that answered with version runs the
// installed release. With no installed version recorded, any answer counts.
func runsInstalled(version, installed string) bool {
	return installed == "" || sameRelease(version, installed)
}

func versionOrUnknown(v string) string {
	if v == "" {
		return "unknown version"
	}
	return v
}

// waitForDaemonVersion waits until the daemon on socket answers with the
// installed version, and returns the version it reported.
func (u *Updater) waitForDaemonVersion(socket, installed string, deadline time.Time) (string, error) {
	last := "no daemon answered"
	for {
		ver, running := u.probe(socket)
		if running && runsInstalled(ver, installed) {
			return ver, nil
		}
		if running {
			last = "the daemon answered with " + versionOrUnknown(ver)
		}
		if !time.Now().Before(deadline) {
			return "", fmt.Errorf("%s", last)
		}
		if !u.pause(u.pollInterval()) {
			return "", fmt.Errorf("updater stopped while waiting (%s)", last)
		}
	}
}

// daemonNotUnderLaunchd handles a daemon that launchd does not run: why says
// how the launchd job was found. A daemon answering on socket with another
// version is left running and reported; one that does not answer is not
// running, and its next start uses the new binary.
func (u *Updater) daemonNotUnderLaunchd(socket, installed, why string) restartOutcome {
	ver, running := u.probe(socket)
	switch {
	case !running:
		slog.Info("daemon not running; its next start uses the new binary", "socket", socket, "launchd", why)
		return restartOutcome{}
	case sameRelease(ver, installed):
		slog.Info("daemon already runs the installed version", "version", ver, "socket", socket)
		return restartOutcome{}
	}
	slog.Warn("daemon not restarted onto the new binary: launchd does not run it", "socket", socket, "version", ver, "launchd", why)
	return restartOutcome{err: fmt.Errorf(
		"daemon on %s left running the old version (%s): %s, so nothing would start it again after a stop; restart it with: pilotctl daemon stop && pilotctl daemon start",
		socket, versionOrUnknown(ver), why)}
}

// signalDaemonRestartDarwin restarts the daemon through its launchd job and
// confirms the new daemon answers with the installed version. See the comment
// at the top of this file for when it leaves the daemon running instead.
func (u *Updater) signalDaemonRestartDarwin() restartOutcome {
	daemonPath := filepath.Join(u.config.InstallDir, "pilot-daemon")
	installed := u.installedVersion()

	job, found := u.findLaunchdJob(daemonLaunchdLabels)
	if !found {
		return u.daemonNotUnderLaunchd(driver.DefaultSocketPath(), installed,
			fmt.Sprintf("no launchd job %s is loaded in %s", daemonLaunchdLabels[0], strings.Join(u.launchdDomains(), " or ")))
	}
	if job.pid == 0 {
		return u.daemonNotUnderLaunchd(job.socket(), installed,
			fmt.Sprintf("the launchd job %s is loaded but runs no process (%s)", job.target, job.describe()))
	}
	if !sameFile(job.programPath(), daemonPath) {
		slog.Warn("daemon not restarted: its launchd job runs another binary", "target", job.target, "program", job.programPath(), "updated", daemonPath)
		return restartOutcome{err: fmt.Errorf(
			"daemon (pid %d) not restarted: the launchd job %s runs %q, not the updated %s, so a restart would not run the new release; re-run install.sh, which points the job at %s, then restart it with: launchctl kickstart -k %s",
			job.pid, job.target, job.programPath(), daemonPath, daemonPath, job.target)}
	}
	if job.pid == os.Getpid() {
		return restartOutcome{err: fmt.Errorf("daemon (pid %d) not restarted: the updater runs inside it; restart it with: launchctl kickstart -k %s", job.pid, job.target)}
	}

	oldPid, socket := job.pid, job.socket()
	wait := u.launchdWait()
	deadline := time.Now().Add(wait)
	slog.Info("restarting daemon via launchd", "target", job.target, "pid", oldPid)
	if out, err := u.launchctl("kickstart", "-k", job.target); err != nil {
		output := strings.TrimSpace(string(out))
		slog.Warn("launchctl kickstart failed — restart daemon manually", "target", job.target, "err", err, "output", output)
		return restartOutcome{err: fmt.Errorf("restart daemon (launchctl kickstart -k %s): %v %s", job.target, err, output)}
	}
	newPid, err := u.waitForLaunchdRespawn(job.target, oldPid, deadline)
	if err != nil {
		slog.Error("launchd did not start the daemon again", "target", job.target, "error", err)
		return restartOutcome{err: fmt.Errorf(
			"sent launchctl kickstart -k %s (daemon pid %d), but launchd did not start the daemon again within %s (%v); check `launchctl print %s` and the daemon log, and start it with: launchctl kickstart %s",
			job.target, oldPid, wait, err, job.target, job.target)}
	}
	ver, err := u.waitForDaemonVersion(socket, installed, deadline)
	if err != nil {
		slog.Error("restarted daemon did not answer with the installed version", "target", job.target, "pid", newPid, "socket", socket, "error", err)
		want := installed
		if want == "" {
			want = "its version"
		}
		return restartOutcome{err: fmt.Errorf(
			"launchd restarted the daemon (pid %d -> %d) but it did not answer on %s with %s within %s (%v); check the daemon log and restart it with: launchctl kickstart -k %s",
			oldPid, newPid, socket, want, wait, err, job.target)}
	}
	slog.Info("daemon restarted on the new binary", "target", job.target, "pid", newPid, "version", ver)
	return restartOutcome{restarted: true, by: "launchd " + job.target, version: ver}
}

// restartUpdaterService restarts the updater service onto a pilot-updater
// binary that this process replaced. It is used by a one-shot RunOnce: the
// loop exits instead, and its service manager starts it again. Only the macOS
// launchd job is restarted. attempted is false when there was nothing to
// restart: no updater job runs the replaced binary, or this process is the
// job.
func (u *Updater) restartUpdaterService() (restartOutcome, bool) {
	if u.targetOS() != "darwin" {
		slog.Info("pilot-updater binary replaced; a running updater service uses it after its next restart")
		return restartOutcome{}, false
	}
	updaterPath := filepath.Join(u.config.InstallDir, "pilot-updater")
	job, found := u.findLaunchdJob(updaterLaunchdLabels)
	switch {
	case !found:
		slog.Info("pilot-updater binary replaced; no launchd updater job is loaded")
		return restartOutcome{}, false
	case job.pid == 0:
		slog.Info("pilot-updater binary replaced; the launchd updater job runs no process and starts the new binary next", "target", job.target)
		return restartOutcome{}, false
	case job.pid == os.Getpid():
		return restartOutcome{}, false
	case !sameFile(job.programPath(), updaterPath):
		slog.Warn("updater service not restarted: its launchd job runs another binary", "target", job.target, "program", job.programPath(), "updated", updaterPath)
		return restartOutcome{err: fmt.Errorf(
			"updater service %s (pid %d) not restarted: it runs %q, not the updated %s; re-run install.sh, which points the job at %s",
			job.target, job.pid, job.programPath(), updaterPath, updaterPath)}, true
	}

	wait := u.launchdWait()
	deadline := time.Now().Add(wait)
	slog.Info("restarting updater service via launchd", "target", job.target, "pid", job.pid)
	if out, err := u.launchctl("kickstart", "-k", job.target); err != nil {
		output := strings.TrimSpace(string(out))
		return restartOutcome{err: fmt.Errorf("restart updater service (launchctl kickstart -k %s): %v %s", job.target, err, output)}, true
	}
	newPid, err := u.waitForLaunchdRespawn(job.target, job.pid, deadline)
	if err != nil {
		return restartOutcome{err: fmt.Errorf(
			"sent launchctl kickstart -k %s (updater pid %d), but launchd did not start the updater again within %s (%v); start it with: launchctl kickstart %s",
			job.target, job.pid, wait, err, job.target)}, true
	}
	slog.Info("updater service restarted on the new binary", "target", job.target, "pid", newPid)
	return restartOutcome{restarted: true, by: "launchd " + job.target}, true
}

// daemonOnInstalledVersionDarwin reports whether the daemon answers over IPC
// with the installed version.
func (u *Updater) daemonOnInstalledVersionDarwin() bool {
	installed := u.installedVersion()
	if installed == "" {
		return false
	}
	socket := driver.DefaultSocketPath()
	if job, ok := u.findLaunchdJob(daemonLaunchdLabels); ok {
		socket = job.socket()
	}
	ver, running := u.probe(socket)
	return running && sameRelease(ver, installed)
}
