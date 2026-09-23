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

// TestSignalDaemonRestartLinux_Paths covers signal success, signal failure,
// daemon not running (not an error) and unreadable /proc (an error).
func TestSignalDaemonRestartLinux_Paths(t *testing.T) {
	t.Parallel()
	installDir := t.TempDir()
	daemon := filepath.Join(installDir, "pilot-daemon")
	root := fakeProc(t, map[string]string{"77": daemon + " (deleted)"})

	var gotPid int
	var gotSig syscall.Signal
	u := &Updater{
		config:   Config{InstallDir: installDir},
		procRoot: root,
		killFn: func(pid int, sig syscall.Signal) error {
			gotPid, gotSig = pid, sig
			return nil
		},
	}
	if err := u.signalDaemonRestartLinux(); err != nil {
		t.Fatalf("signalDaemonRestartLinux: %v", err)
	}
	if gotPid != 77 || gotSig != syscall.SIGTERM {
		t.Errorf("signalled pid %d with %v, want 77 SIGTERM", gotPid, gotSig)
	}

	u.killFn = func(int, syscall.Signal) error { return errors.New("operation not permitted") }
	if err := u.signalDaemonRestartLinux(); err == nil || !strings.Contains(err.Error(), "operation not permitted") {
		t.Errorf("kill failure = %v, want reported", err)
	}

	u.procRoot = fakeProc(t, nil)
	if err := u.signalDaemonRestartLinux(); err != nil {
		t.Errorf("daemon not running must not be an error, got %v", err)
	}

	u.procRoot = filepath.Join(root, "does-not-exist")
	if err := u.signalDaemonRestartLinux(); err == nil {
		t.Error("unreadable /proc must be reported")
	}
}

// TestSignalDaemonRestartDarwin_Paths: the launchctl invocation and its
// failure are reported (a daemon started by hand, not by launchd, keeps
// running the old binary).
func TestSignalDaemonRestartDarwin_Paths(t *testing.T) {
	t.Parallel()
	var got []string
	u := &Updater{
		config: Config{InstallDir: t.TempDir()},
		runCmd: func(name string, args ...string) ([]byte, error) {
			got = append([]string{name}, args...)
			return nil, nil
		},
	}
	if err := u.signalDaemonRestartDarwin(); err != nil {
		t.Fatalf("signalDaemonRestartDarwin: %v", err)
	}
	if len(got) != 4 || got[0] != "launchctl" || got[1] != "kickstart" || got[2] != "-k" ||
		!strings.HasSuffix(got[3], "/network.pilotprotocol.pilot-daemon") {
		t.Errorf("command = %q", got)
	}

	u.runCmd = func(string, ...string) ([]byte, error) {
		return []byte("Could not find service"), errors.New("exit status 113")
	}
	err := u.signalDaemonRestartDarwin()
	if err == nil || !strings.Contains(err.Error(), "Could not find service") || !strings.Contains(err.Error(), "exit status 113") {
		t.Errorf("launchctl failure = %v, want reported with output", err)
	}
}
