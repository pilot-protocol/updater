// SPDX-License-Identifier: AGPL-3.0-or-later

package updater

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Restarting the daemon on Linux.
//
// The updater cannot run `systemctl restart`: it runs as the unprivileged
// install user, and it runs no external tools (TestNoExternalToolDependency).
// It restarts the daemon by sending it SIGTERM and letting systemd start it
// again on the new binary. That works only when systemd really will start it
// again, which needs two things:
//
//   - The daemon is the process of a system service, not a login session, a
//     container or another service's child. A daemon started with
//     `pilotctl daemon start` (containers, WSL, CI, where install.sh sets up
//     no service) has no supervisor at all.
//   - The service restarts after a clean exit. pilot-daemon exits 0 on
//     SIGTERM. systemd restarts that under Restart=always or on-success, but
//     not under Restart=on-failure, which install.sh wrote up to v1.9.0 (web4
//     1ee4db7d switched to Restart=always). Updates never rewrite the unit.
//
// Stopping a daemon that nothing restarts takes the node offline. So when
// either condition fails, the updater leaves the daemon running the old
// binary and reports why, with the command that restarts it (restart_error in
// the status file). When both hold, it sends SIGTERM and then waits for a new
// daemon process on the new binary; if none appears, that is reported too.
//
// The unit is read from the unit files, not from systemctl: the service is
// found from /proc/<pid>/cgroup, and Restart=, ExecStart= and the exit-status
// lists are read from its unit file and drop-ins.

const (
	// defaultRestartWait bounds how long the updater waits for systemd to
	// start the daemon again after SIGTERM. The daemon's shutdown takes up
	// to ~5 s and install.sh units use RestartSec=5.
	defaultRestartWait = 30 * time.Second
	// defaultRestartPoll is how often /proc is scanned while waiting.
	defaultRestartPoll = 250 * time.Millisecond
)

// systemdSystemUnitDirs is the systemd system unit search path, highest
// priority first (systemd.unit(5)). /lib/systemd/system is included for
// distributions without a merged /usr.
var systemdSystemUnitDirs = []string{
	"/etc/systemd/system.control",
	"/run/systemd/system.control",
	"/run/systemd/transient",
	"/run/systemd/generator.early",
	"/etc/systemd/system",
	"/etc/systemd/system.attached",
	"/run/systemd/system",
	"/run/systemd/system.attached",
	"/run/systemd/generator",
	"/usr/local/lib/systemd/system",
	"/usr/lib/systemd/system",
	"/lib/systemd/system",
	"/run/systemd/generator.late",
}

// targetOS is the OS whose restart mechanism is used. Tests override it to
// drive the Linux path on macOS.
func (u *Updater) targetOS() string {
	if u.goos != "" {
		return u.goos
	}
	return runtime.GOOS
}

// procDir is the /proc root to scan.
func (u *Updater) procDir() string {
	if u.procRoot != "" {
		return u.procRoot
	}
	return "/proc"
}

// unitDirs is the systemd unit search path to read.
func (u *Updater) unitDirs() []string {
	if u.systemdDirs != nil {
		return u.systemdDirs
	}
	return systemdSystemUnitDirs
}

// forEachProcExe calls fn with the pid and /proc/<pid>/exe target of every
// process other than this one whose exe link is readable. fn returns false
// to stop the scan.
func forEachProcExe(procRoot string, fn func(pid int, exe string) bool) error {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return err
	}
	self := os.Getpid()
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 || pid == self {
			continue
		}
		exe, err := os.Readlink(filepath.Join(procRoot, entry.Name(), "exe"))
		if err != nil {
			continue
		}
		if !fn(pid, exe) {
			return nil
		}
	}
	return nil
}

// scanDaemonProcs lists the processes running exePath. current holds those
// running the file now at exePath; replaced holds those still running a
// binary that has since been renamed over ("<exePath> (deleted)").
func scanDaemonProcs(procRoot, exePath string) (current, replaced []int, err error) {
	err = forEachProcExe(procRoot, func(pid int, exe string) bool {
		switch exe {
		case exePath:
			current = append(current, pid)
		case exePath + " (deleted)":
			replaced = append(replaced, pid)
		}
		return true
	})
	return current, replaced, err
}

// daemonOnInstalledBinary reports whether the daemon is known to be running
// the installed binary: a process runs daemonPath itself and none still runs
// a replaced copy. Only Linux can tell (from /proc); elsewhere it returns
// false.
func (u *Updater) daemonOnInstalledBinary() bool {
	if u.targetOS() != "linux" {
		return false
	}
	daemonPath := filepath.Join(u.config.InstallDir, "pilot-daemon")
	current, replaced, err := scanDaemonProcs(u.procDir(), daemonPath)
	return err == nil && len(current) > 0 && len(replaced) == 0
}

// serviceUnitOf returns the systemd service whose cgroup holds pid, read from
// <procRoot>/<pid>/cgroup. unit is "" when the process is not in a service:
// it is in a login session or container scope, or systemd is not running.
// userManager reports that the service belongs to a per-user systemd
// instance (under user@<uid>.service) rather than the system manager.
func serviceUnitOf(procRoot string, pid int) (unit string, userManager bool, err error) {
	data, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cgroup"))
	if err != nil {
		return "", false, err
	}
	// Lines are "hierarchy-ID:controller-list:cgroup-path". systemd's own
	// hierarchy is "name=systemd" under cgroup v1 and the unified "0::"
	// line under v2 (both are present, and agree, in hybrid mode).
	var named, unified string
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), ":", 3)
		if len(parts) != 3 {
			continue
		}
		switch {
		case parts[1] == "name=systemd":
			named = parts[2]
		case parts[0] == "0" && parts[1] == "":
			unified = parts[2]
		}
	}
	path := named
	if path == "" {
		path = unified
	}
	// The process belongs to the deepest unit in the path. Slices only group
	// units; sub-cgroups a service delegates have no unit suffix.
	for _, comp := range strings.Split(path, "/") {
		comp = strings.TrimPrefix(comp, "_") // systemd escapes some names with a leading "_"
		if !strings.HasSuffix(comp, ".service") && !strings.HasSuffix(comp, ".scope") {
			continue
		}
		if strings.HasPrefix(unit, "user@") && strings.HasSuffix(unit, ".service") {
			userManager = true
		}
		unit = comp
	}
	if !strings.HasSuffix(unit, ".service") {
		return "", false, nil
	}
	return unit, userManager, nil
}

// serviceUnit is the part of a systemd service's configuration that decides
// whether stopping its process with SIGTERM makes systemd start it again.
type serviceUnit struct {
	name     string
	fragment string // the unit file that was read

	execStart   string // program of the first ExecStart= command ("" = none)
	execSet     bool
	restart     string   // Restart=, lower-cased ("" = the default, "no")
	forceExit   []string // RestartForceExitStatus=
	preventExit []string // RestartPreventExitStatus=
}

// templateOf returns the template unit name of an instance unit
// ("pilot-daemon@x.service" -> "pilot-daemon@.service"), or "".
func templateOf(name string) string {
	at := strings.IndexByte(name, '@')
	dot := strings.LastIndexByte(name, '.')
	if at < 0 || dot < at || at+1 == dot {
		return ""
	}
	return name[:at+1] + name[dot:]
}

// loadServiceUnit reads the unit file of service name (or its template)
// from dirs, highest priority first, and applies its drop-ins
// (<name>.d/*.conf and the top-level service.d/*.conf), sorted by file name,
// the first of each name winning, as systemd does.
func loadServiceUnit(dirs []string, name string) (serviceUnit, error) {
	su := serviceUnit{name: name}
	names := []string{name}
	if tmpl := templateOf(name); tmpl != "" {
		names = append(names, tmpl)
	}
	for _, n := range names {
		for _, dir := range dirs {
			p := filepath.Join(dir, n)
			data, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			su.fragment = p
			su.parse(data)
			break
		}
		if su.fragment != "" {
			break
		}
	}
	if su.fragment == "" {
		return su, fmt.Errorf("no unit file for %s", name)
	}

	dropins := map[string]string{} // file name -> path
	for _, n := range append(names, "service") {
		for _, dir := range dirs {
			matches, _ := filepath.Glob(filepath.Join(dir, n+".d", "*.conf"))
			for _, m := range matches {
				if _, seen := dropins[filepath.Base(m)]; !seen {
					dropins[filepath.Base(m)] = m
				}
			}
		}
	}
	order := make([]string, 0, len(dropins))
	for base := range dropins {
		order = append(order, base)
	}
	sort.Strings(order)
	for _, base := range order {
		data, err := os.ReadFile(dropins[base])
		if err != nil {
			continue
		}
		su.parse(data)
	}
	return su, nil
}

// parse applies the [Service] settings in one unit file or drop-in.
func (su *serviceUnit) parse(data []byte) {
	lines := strings.Split(string(data), "\n")
	section := ""
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		// A trailing backslash continues the line. Comment lines inside a
		// continued line are skipped.
		for strings.HasSuffix(line, `\`) && i+1 < len(lines) {
			i++
			next := strings.TrimSpace(lines[i])
			if next != "" && (next[0] == '#' || next[0] == ';') {
				continue
			}
			line = strings.TrimSuffix(line, `\`) + " " + next
		}
		line = strings.TrimSuffix(line, `\`)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line[1 : len(line)-1]
			continue
		}
		if section != "Service" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch strings.TrimSpace(key) {
		case "Restart":
			su.restart = strings.ToLower(value)
		case "ExecStart":
			// An empty assignment resets the list (how drop-ins replace it).
			if value == "" {
				su.execStart, su.execSet = "", false
			} else if !su.execSet {
				su.execStart, su.execSet = execProgram(value), true
			}
		case "RestartForceExitStatus":
			su.forceExit = appendStatusList(su.forceExit, value)
		case "RestartPreventExitStatus":
			su.preventExit = appendStatusList(su.preventExit, value)
		}
	}
}

func appendStatusList(list []string, value string) []string {
	if value == "" {
		return nil
	}
	return append(list, strings.Fields(value)...)
}

// execProgram returns the program of an ExecStart= command line, without
// systemd's special prefixes (@ - : + ! |) and quotes.
func execProgram(cmd string) string {
	cmd = strings.TrimSpace(strings.TrimLeft(cmd, "@-:+!|"))
	if cmd == "" {
		return ""
	}
	if q := cmd[0]; q == '"' || q == '\'' {
		if end := strings.IndexByte(cmd[1:], q); end >= 0 {
			return cmd[1 : 1+end]
		}
		return cmd[1:]
	}
	if i := strings.IndexAny(cmd, " \t"); i >= 0 {
		return cmd[:i]
	}
	return cmd
}

// restartSetting is Restart= as systemd reads it.
func (su serviceUnit) restartSetting() string {
	if su.restart == "" {
		return "no"
	}
	return su.restart
}

// restartsOnCleanExit reports whether systemd starts the service again after
// its main process exits cleanly, which is how pilot-daemon exits on SIGTERM
// (exit status 0; SIGTERM itself also counts as clean).
func (su serviceUnit) restartsOnCleanExit() bool {
	clean := func(list []string) bool {
		for _, s := range list {
			switch strings.ToUpper(s) {
			case "0", "SUCCESS", "SIGTERM", "TERM":
				return true
			}
		}
		return false
	}
	if clean(su.preventExit) {
		return false
	}
	switch su.restart {
	case "always", "on-success":
		return true
	}
	return clean(su.forceExit)
}

// sameFile reports whether the program path a names the file at b.
func sameFile(a, b string) bool {
	if a == "" {
		return false
	}
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

// unitShortName drops the ".service" suffix for systemctl commands.
func unitShortName(unit string) string {
	return strings.TrimSuffix(unit, ".service")
}

// daemonSupervisor decides whether the daemon (pid, running daemonPath) may
// be stopped with SIGTERM, i.e. whether systemd will start it again. It
// returns the service that will, or an error that says why not and how to
// restart the daemon by hand. The daemon is left running in that case.
func (u *Updater) daemonSupervisor(procRoot string, pid int, daemonPath string) (string, error) {
	const manual = "pilotctl daemon stop && pilotctl daemon start"
	unit, userManager, err := serviceUnitOf(procRoot, pid)
	if err != nil {
		return "", fmt.Errorf("daemon (pid %d) left running the old version: cannot tell whether a service manager would start it again (%v); restart it with: %s", pid, err, manual)
	}
	if unit == "" {
		return "", fmt.Errorf("daemon (pid %d) left running the old version: it is not run by a systemd service, so nothing would start it again after a stop; restart it with: %s", pid, manual)
	}
	if userManager {
		return "", fmt.Errorf("daemon (pid %d) left running the old version: it runs in the systemd user service %s, which the updater does not restart; restart it with: systemctl --user restart %s", pid, unit, unitShortName(unit))
	}
	su, err := loadServiceUnit(u.unitDirs(), unit)
	if err != nil {
		return "", fmt.Errorf("daemon (pid %d) left running the old version: it runs in %s, but that unit could not be read (%v); restart it with: sudo systemctl restart %s", pid, unit, err, unitShortName(unit))
	}
	if !sameFile(su.execStart, daemonPath) {
		return "", fmt.Errorf("daemon (pid %d) left running the old version: it runs inside %s, which starts %q rather than %s, so systemd would not start the daemon again; restart it with: %s", pid, unit, su.execStart, daemonPath, manual)
	}
	if !su.restartsOnCleanExit() {
		return "", fmt.Errorf("daemon (pid %d) left running the old version: %s has Restart=%s, so systemd would not start the daemon again after it stops cleanly; restart it with: sudo systemctl restart %s (re-running install.sh rewrites the unit with Restart=always)", pid, unit, su.restartSetting(), unitShortName(unit))
	}
	return unit, nil
}

// waitForDaemonRestart waits for systemd to start the daemon again after
// oldPid was sent SIGTERM: a new process running daemonPath itself, not a
// replaced binary. It returns the new pid.
func (u *Updater) waitForDaemonRestart(procRoot, daemonPath string, oldPid int, unit string) (int, error) {
	wait, poll := u.restartWait, u.restartPoll
	if wait <= 0 {
		wait = defaultRestartWait
	}
	if poll <= 0 {
		poll = defaultRestartPoll
	}
	deadline := time.Now().Add(wait)
	for {
		if current, _, err := scanDaemonProcs(procRoot, daemonPath); err == nil {
			for _, pid := range current {
				if pid != oldPid {
					return pid, nil
				}
			}
		}
		if !time.Now().Before(deadline) {
			return 0, fmt.Errorf("sent SIGTERM to the daemon (pid %d), but %s did not start it again on the new binary within %s; check `systemctl status %s` and start it with: sudo systemctl start %s",
				oldPid, unit, wait, unitShortName(unit), unitShortName(unit))
		}
		timer := time.NewTimer(poll)
		select {
		case <-timer.C:
		case <-u.stopCh:
			timer.Stop()
			return 0, fmt.Errorf("updater stopped while waiting for %s to start the daemon again (pid %d was sent SIGTERM)", unit, oldPid)
		}
	}
}
