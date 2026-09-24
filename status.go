// SPDX-License-Identifier: AGPL-3.0-or-later

package updater

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// StatusFileName is the default name of the file the updater writes after
// every check. When Config.StatusPath is empty and Config.StatePath is set,
// the status file lives next to the control file (for the standard install:
// ~/.pilot/auto-update.json -> ~/.pilot/update-state.json).
const StatusFileName = "update-state.json"

// Values of Status.LastResult.
const (
	// ResultUpToDate: the check succeeded and nothing needed installing
	// (the installed version is the latest release, or the pinned one).
	ResultUpToDate = "up_to_date"
	// ResultUpdated: the check succeeded and a release was installed.
	ResultUpdated = "updated"
	// ResultFailed: the check failed; Status.LastError says why.
	ResultFailed = "failed"
)

// Values of Status.LastCheckTrigger.
const (
	// TriggerAuto marks a check run by the periodic loop (Start).
	TriggerAuto = "auto"
	// TriggerManual marks a one-shot check (RunOnce, e.g. `pilotctl update`).
	TriggerManual = "manual"
)

// Status is the machine-readable record of what the updater last did. It is
// persisted as JSON at Config.StatusPath after every check so that other
// tools (`pilotctl update status`, `daemon status`, support scripts) can tell
// whether updates are actually working, instead of only whether they are
// switched on.
//
// The file can be shared by the long-running updater service and one-shot
// RunOnce callers configured with the same StatusPath (or StatePath). Each
// writer takes an advisory lock (StatusFileName + ".lock") and merges its
// fields into what is already on disk, so a manual run keeps the loop's
// LoopPID/NextCheckAt and the loop keeps the failure streak a manual run
// started. A RunOnce caller with neither path set writes nothing.
type Status struct {
	// UpdaterVersion is the version of the updater that wrote the record.
	UpdaterVersion string `json:"updater_version,omitempty"`
	// Repo is the GitHub owner/repo releases are read from.
	Repo string `json:"repo,omitempty"`
	// PinnedVersion is the pin in force for the last check ("" = latest).
	PinnedVersion string `json:"pinned_version,omitempty"`

	// LastCheckAt is when the last check finished.
	LastCheckAt time.Time `json:"last_check_at,omitzero"`
	// LastCheckTrigger is TriggerAuto or TriggerManual.
	LastCheckTrigger string `json:"last_check_trigger,omitempty"`
	// LastResult is ResultUpToDate, ResultUpdated or ResultFailed.
	LastResult string `json:"last_result,omitempty"`
	// LastError is the error of the last check; empty when it succeeded.
	LastError string `json:"last_error,omitempty"`
	// ConsecutiveFailures counts failed checks since the last success.
	ConsecutiveFailures int `json:"consecutive_failures"`
	// LastSuccessAt is when a check last completed without error.
	LastSuccessAt time.Time `json:"last_success_at,omitzero"`

	// CurrentVersion is the installed release after the last check.
	CurrentVersion string `json:"current_version,omitempty"`
	// LatestVersion is the release the last check compared against: the
	// latest published release, or the pinned tag when pinned.
	LatestVersion string `json:"latest_version,omitempty"`

	// LastUpdateAt / LastUpdateVersion record the last release installed.
	LastUpdateAt      time.Time `json:"last_update_at,omitzero"`
	LastUpdateVersion string    `json:"last_update_version,omitempty"`
	// RestartError is set when new binaries were installed but the daemon
	// is not running them: it was left on the old version, or it was
	// stopped and did not come back. It says how to restart it. The updater
	// stops the daemon only when a service manager will start it again: a
	// systemd service on Linux, the launchd job on macOS. A daemon without
	// one (e.g. `pilotctl daemon start` in a container) is left running and
	// reported here. Cleared by the next successful restart and by the next
	// check that finds the daemon running the installed release.
	RestartError string `json:"restart_error,omitempty"`
	// DaemonRestartedAt is when the updater last restarted the daemon onto
	// installed binaries and saw it come back: a new process on the
	// installed binary under systemd (Linux), or a new launchd process that
	// answered over IPC with the installed version (macOS).
	DaemonRestartedAt time.Time `json:"daemon_restarted_at,omitzero"`
	// DaemonRestartedBy names the service that restarted it, e.g.
	// "launchd gui/501/network.pilotprotocol.pilot-daemon" or
	// "systemd pilot-daemon.service".
	DaemonRestartedBy string `json:"daemon_restarted_by,omitempty"`
	// DaemonRestartedVersion is the version the daemon reported over IPC
	// after that restart (macOS; empty on Linux).
	DaemonRestartedVersion string `json:"daemon_restarted_version,omitempty"`

	// UpdaterRestartError is set when a one-shot update (RunOnce, e.g.
	// `pilotctl update`) replaced pilot-updater but could not move the
	// running updater service (the macOS launchd job) onto it. Cleared by the
	// next successful restart.
	UpdaterRestartError string `json:"updater_restart_error,omitempty"`
	// UpdaterRestartedAt is when a one-shot update last restarted the
	// updater service onto a new pilot-updater binary.
	UpdaterRestartedAt time.Time `json:"updater_restarted_at,omitzero"`

	// LoopPID and LoopStartedAt identify the long-running updater process
	// (the service started via Start). Absent when no loop ever ran.
	LoopPID       int       `json:"loop_pid,omitempty"`
	LoopStartedAt time.Time `json:"loop_started_at,omitzero"`
	// NextCheckAt is when the loop will next wake up (plus up to 30s of
	// jitter). A NextCheckAt well in the past means the loop is not running.
	NextCheckAt time.Time `json:"next_check_at,omitzero"`
}

// ReadStatus loads the status file written by the updater. A missing file
// returns an error satisfying errors.Is(err, fs.ErrNotExist), which callers
// should read as "the updater has never recorded a check here".
func ReadStatus(path string) (Status, error) {
	var s Status
	data, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return Status{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return s, nil
}

// statusPath returns where this updater records its status, or "" when it
// records nothing (no StatusPath and no StatePath configured).
func (u *Updater) statusPath() string {
	if u.config.StatusPath != "" {
		return u.config.StatusPath
	}
	if u.config.StatePath != "" {
		return filepath.Join(filepath.Dir(u.config.StatePath), StatusFileName)
	}
	return ""
}

// LastStatus returns the status recorded by this Updater's most recent check
// (or loop event). It is available even when no status file is configured,
// so a caller of RunOnce can report the outcome without reading the file.
func (u *Updater) LastStatus() Status {
	u.statusMu.Lock()
	defer u.statusMu.Unlock()
	return u.status
}

// updateStatus applies mutate to the current status and persists it. The
// base is the file on disk (so fields written by another process survive);
// when the file is missing or unreadable the in-memory copy is used. The
// read-merge-write runs under statusMu (this process) and an advisory file
// lock (other processes), so concurrent writers do not drop each other's
// changes. Write failures are logged, never returned: recording the outcome
// must not change the outcome.
func (u *Updater) updateStatus(mutate func(*Status)) {
	u.statusMu.Lock()
	defer u.statusMu.Unlock()

	path := u.statusPath()
	base := u.status
	if path != "" {
		wait := u.statusLockWait
		if wait <= 0 {
			wait = defaultStatusLockWait
		}
		unlock := lockStatusFile(path, wait)
		defer unlock()
		if onDisk, err := ReadStatus(path); err == nil {
			base = onDisk
		}
	}
	mutate(&base)
	base.UpdaterVersion = u.config.Version
	base.Repo = u.config.Repo
	u.status = base

	if path == "" {
		return
	}
	if err := writeStatusFile(path, base); err != nil {
		slog.Warn("failed to write update status file", "path", path, "error", err)
	}
}

// defaultStatusLockWait bounds how long a writer waits for another
// process's read-merge-write of the status file (each holds the lock for a
// few milliseconds). After it, the write goes ahead unlocked: recording the
// outcome must never block an update.
const defaultStatusLockWait = 5 * time.Second

// lockStatusFile takes an exclusive advisory lock (flock) on path + ".lock",
// a sidecar file: path itself is replaced by rename on every write, so it
// cannot carry the lock. It returns the function that releases the lock. If
// the lock cannot be taken within wait it logs why and returns a no-op.
func lockStatusFile(path string, wait time.Duration) (unlock func()) {
	noop := func() {}
	lockPath := path + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		slog.Warn("cannot lock update status file; writing unlocked", "path", lockPath, "error", err)
		return noop
	}
	// Read-only is enough for flock and works when another user (e.g. a
	// `sudo pilotctl update`) created the lock file.
	f, err := os.OpenFile(lockPath, os.O_RDONLY|os.O_CREATE, 0o644)
	if err != nil {
		slog.Warn("cannot lock update status file; writing unlocked", "path", lockPath, "error", err)
		return noop
	}
	fd := int(f.Fd()) // #nosec G115 -- a file descriptor always fits in an int
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(fd, syscall.LOCK_UN)
				_ = f.Close()
			}
		}
		retry := errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EINTR)
		if !retry || !time.Now().Before(deadline) {
			slog.Warn("cannot lock update status file; writing unlocked", "path", lockPath, "error", err)
			_ = f.Close()
			return noop
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// writeStatusFile writes s to path atomically (temp file + rename in the same
// directory) so a reader never sees a torn file.
func writeStatusFile(path string, s Status) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".update-state-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// checkOutcome is what one update check found and did.
type checkOutcome struct {
	current   string // installed version before the check
	latest    string // release compared against (latest or pinned)
	installed string // release installed by this check ("" = none)
	// updaterReplaced reports that the pilot-updater binary itself was
	// swapped, so the running updater process is now stale.
	updaterReplaced bool
	// restart is the daemon restart this check made (nil = none: nothing
	// was installed, or the loop left it to its successor process).
	restart *restartOutcome
	// updaterRestart is the updater service restart this check made (nil =
	// none).
	updaterRestart *restartOutcome
	// daemonCurrent reports that, with nothing installed by this check, the
	// daemon was found running the installed release, so an earlier
	// restart_error no longer applies.
	daemonCurrent bool
}

// recordCheck persists the outcome of one check.
func (u *Updater) recordCheck(trigger string, out checkOutcome, checkErr error) {
	now := time.Now().UTC()
	u.updateStatus(func(s *Status) {
		s.PinnedVersion = u.config.PinnedVersion
		s.LastCheckAt = now
		s.LastCheckTrigger = trigger
		if out.installed == "" && out.daemonCurrent {
			s.RestartError = ""
		}
		if out.latest != "" {
			s.LatestVersion = out.latest
		}
		switch {
		case out.installed != "":
			s.CurrentVersion = out.installed
		case out.current != "":
			s.CurrentVersion = out.current
		}
		if checkErr != nil {
			s.LastResult = ResultFailed
			s.LastError = checkErr.Error()
			s.ConsecutiveFailures++
			return
		}
		s.LastError = ""
		s.ConsecutiveFailures = 0
		s.LastSuccessAt = now
		if out.installed == "" {
			s.LastResult = ResultUpToDate
			return
		}
		s.LastResult = ResultUpdated
		s.LastUpdateAt = now
		s.LastUpdateVersion = out.installed
		// When the loop replaced its own binary it exits without restarting
		// the daemon: the new process does that (recoverPendingRestart) and
		// records it.
		if out.restart != nil {
			applyDaemonRestart(s, *out.restart, now)
		}
		if out.updaterRestart != nil {
			applyUpdaterRestart(s, *out.updaterRestart, now)
		}
	})
}

// applyDaemonRestart records a daemon restart attempt in s.
func applyDaemonRestart(s *Status, r restartOutcome, now time.Time) {
	if r.err != nil {
		s.RestartError = r.err.Error()
		return
	}
	s.RestartError = ""
	if r.restarted {
		s.DaemonRestartedAt = now
		s.DaemonRestartedBy = r.by
		s.DaemonRestartedVersion = r.version
	}
}

// applyUpdaterRestart records an updater service restart attempt in s.
func applyUpdaterRestart(s *Status, r restartOutcome, now time.Time) {
	if r.err != nil {
		s.UpdaterRestartError = r.err.Error()
		return
	}
	s.UpdaterRestartError = ""
	if r.restarted {
		s.UpdaterRestartedAt = now
	}
}

// recordRestart records the outcome of a daemon restart made outside a
// check (recoverPendingRestart).
func (u *Updater) recordRestart(r restartOutcome) {
	now := time.Now().UTC()
	u.updateStatus(func(s *Status) { applyDaemonRestart(s, r, now) })
}

// recordedRestartError is the restart_error currently on record: in the
// status file when there is one, else in this Updater's last status.
func (u *Updater) recordedRestartError() string {
	if path := u.statusPath(); path != "" {
		if s, err := ReadStatus(path); err == nil {
			return s.RestartError
		}
	}
	return u.LastStatus().RestartError
}
