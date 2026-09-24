// SPDX-License-Identifier: AGPL-3.0-or-later

package updater

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// fakeProc builds a /proc-like tree: pid -> exe symlink target.
func fakeProc(t *testing.T, procs map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for pid, exe := range procs {
		dir := filepath.Join(root, pid)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if exe != "" {
			if err := os.Symlink(exe, filepath.Join(dir, "exe")); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Non-pid noise that real /proc has.
	if err := os.WriteFile(filepath.Join(root, "uptime"), []byte("1 1"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestFindProcessByExe_MatchesReplacedBinary: right after an update the
// daemon's /proc/<pid>/exe reads "<path> (deleted)" (its inode was renamed
// over). The old exact-match check never found it, so Linux daemons kept
// running the old version after every auto-update.
func TestFindProcessByExe_MatchesReplacedBinary(t *testing.T) {
	t.Parallel()
	daemon := "/home/u/.pilot/bin/pilot-daemon"
	root := fakeProc(t, map[string]string{
		"100":  "/usr/bin/bash",
		"200":  daemon + " (deleted)",
		"self": daemon, // non-numeric: ignored
		"300":  "",     // no exe link (kernel thread)
	})
	pid, err := findProcessByExe(root, daemon)
	if err != nil || pid != 200 {
		t.Fatalf("findProcessByExe = %d, %v; want 200", pid, err)
	}
}

func TestFindProcessByExe_ExactAndMissing(t *testing.T) {
	t.Parallel()
	daemon := "/opt/pilot/pilot-daemon"
	root := fakeProc(t, map[string]string{"55": daemon, "56": "/opt/pilot/pilot-daemon-old"})
	if pid, err := findProcessByExe(root, daemon); err != nil || pid != 55 {
		t.Errorf("exact match = %d, %v; want 55", pid, err)
	}
	if pid, err := findProcessByExe(root, "/nowhere/pilot-daemon"); err != nil || pid != 0 {
		t.Errorf("no match = %d, %v; want 0, nil", pid, err)
	}
	if _, err := findProcessByExe(filepath.Join(root, "missing"), daemon); err == nil {
		t.Error("unreadable proc root must be an error")
	}
}

// TestSignalDaemonRestartLinux_Paths covers a supervised daemon (SIGTERM,
// systemd starts it again), signal failure, daemon not running (not an
// error) and unreadable /proc (an error).
func TestSignalDaemonRestartLinux_Paths(t *testing.T) {
	t.Parallel()
	u, sys := supervisedLinuxUpdater(t, "always")

	var gotPid int
	var gotSig syscall.Signal
	sys.onKill = func(pid int, sig syscall.Signal) error {
		gotPid, gotSig = pid, sig
		return nil
	}
	if err := u.signalDaemonRestartLinux().err; err != nil {
		t.Fatalf("signalDaemonRestartLinux: %v", err)
	}
	if gotPid != 77 || gotSig != syscall.SIGTERM {
		t.Errorf("signalled pid %d with %v, want 77 SIGTERM", gotPid, gotSig)
	}

	// The restarted daemon (on the installed binary) is found next time.
	sys.onKill = func(int, syscall.Signal) error { return errors.New("operation not permitted") }
	if err := u.signalDaemonRestartLinux().err; err == nil || !strings.Contains(err.Error(), "operation not permitted") {
		t.Errorf("kill failure = %v, want reported", err)
	}

	root := u.procRoot
	u.procRoot = fakeProc(t, nil)
	if err := u.signalDaemonRestartLinux().err; err != nil {
		t.Errorf("daemon not running must not be an error, got %v", err)
	}

	u.procRoot = filepath.Join(root, "does-not-exist")
	if err := u.signalDaemonRestartLinux().err; err == nil {
		t.Error("unreadable /proc must be reported")
	}
}
