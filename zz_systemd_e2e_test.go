// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build linux

package updater

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// End-to-end checks of the Linux daemon restart against a real systemd,
// the setup the UPD49-1 review reproduced the bug on. They need root and
// systemd as PID 1, so they are skipped unless PILOT_UPDATER_SYSTEMD_E2E=1.
// To run them in a throwaway container:
//
//	GOOS=linux go test -c -o updater.test .
//	docker run -d --name upd-e2e --privileged --cgroupns=private \
//	    -v "$PWD":/e2e:ro <debian image with systemd> /sbin/init
//	docker exec -e PILOT_UPDATER_SYSTEMD_E2E=1 upd-e2e \
//	    /e2e/updater.test -test.run 'TestSystemdE2E' -test.v
//
// The fake daemon is this test binary run as TestFakeDaemonProcess: like
// pilot-daemon it exits 0 on SIGTERM.

const fakeDaemonEnv = "PILOT_UPDATER_FAKE_DAEMON"

// TestFakeDaemonProcess is not a test: it is the fake daemon process.
func TestFakeDaemonProcess(t *testing.T) {
	if os.Getenv(fakeDaemonEnv) == "" {
		t.Skip("fake daemon process for TestSystemdE2E_*")
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM)
	<-sig
	os.Exit(0) // clean exit, as pilot-daemon does on SIGTERM
}

func requireSystemdE2E(t *testing.T) {
	t.Helper()
	if os.Getenv("PILOT_UPDATER_SYSTEMD_E2E") != "1" {
		t.Skip("set PILOT_UPDATER_SYSTEMD_E2E=1 to run against a real systemd (root, systemd as PID 1)")
	}
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	if comm, _ := os.ReadFile("/proc/1/comm"); strings.TrimSpace(string(comm)) != "systemd" {
		t.Skip("PID 1 is not systemd")
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

func systemctl(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("systemctl", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("systemctl %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func mainPID(t *testing.T, unit string) int {
	t.Helper()
	pid, _ := strconv.Atoi(systemctl(t, "show", "-p", "MainPID", "--value", unit))
	return pid
}

func exeOf(pid int) string {
	exe, _ := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	return exe
}

// e2eInstall creates an install dir holding the fake daemon, and returns
// the updater for it and the path of a "new release" daemon binary.
func e2eInstall(t *testing.T) (*Updater, string, string) {
	t.Helper()
	installDir := t.TempDir()
	daemon := filepath.Join(installDir, "pilot-daemon")
	copyFile(t, os.Args[0], daemon)
	newBinary := filepath.Join(t.TempDir(), "pilot-daemon-new")
	copyFile(t, os.Args[0], newBinary)
	u := &Updater{config: Config{InstallDir: installDir}, restartWait: 20 * time.Second}
	return u, daemon, newBinary
}

// startUnit installs and starts a service running the fake daemon.
func startUnit(t *testing.T, name, daemon, restart string) int {
	t.Helper()
	unitPath := "/etc/systemd/system/" + name
	unit := fmt.Sprintf("[Unit]\nDescription=updater e2e %s\n\n[Service]\nType=simple\n"+
		"Environment=%s=1\nExecStart=%s \\\n  -test.run=^TestFakeDaemonProcess$\n"+
		"Restart=%s\nRestartSec=1\n", name, fakeDaemonEnv, daemon, restart)
	if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = exec.Command("systemctl", "stop", name).Run()
		_ = os.Remove(unitPath)
		_ = exec.Command("systemctl", "daemon-reload").Run()
	})
	systemctl(t, "daemon-reload")
	systemctl(t, "start", name)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if pid := mainPID(t, name); pid != 0 && exeOf(pid) == daemon {
			return pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s did not start: %s", name, systemctl(t, "show", "-p", "ActiveState,SubState,MainPID", name))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestSystemdE2E_RestartAlways: the supported unit. The daemon is stopped
// and systemd brings it back on the new binary.
func TestSystemdE2E_RestartAlways(t *testing.T) {
	requireSystemdE2E(t)
	u, daemon, newBinary := e2eInstall(t)
	const unit = "pilot-e2e-always.service"
	oldPid := startUnit(t, unit, daemon, "always")
	if err := replaceBinary(newBinary, daemon); err != nil {
		t.Fatal(err)
	}
	if got := exeOf(oldPid); got != daemon+" (deleted)" {
		t.Fatalf("after replace, exe = %q", got)
	}

	if err := u.signalDaemonRestart(); err != nil {
		t.Fatalf("signalDaemonRestart: %v", err)
	}
	newPid := mainPID(t, unit)
	if newPid == 0 || newPid == oldPid || exeOf(newPid) != daemon {
		t.Fatalf("after restart: MainPID %d (old %d), exe %q; want a new daemon on %s", newPid, oldPid, exeOf(newPid), daemon)
	}
	if state := systemctl(t, "show", "-p", "ActiveState", "--value", unit); state != "active" {
		t.Errorf("ActiveState = %s", state)
	}
}

// TestSystemdE2E_RestartOnFailure: the unit install.sh wrote up to v1.9.0.
// Before the fix the daemon was stopped and systemd left it down (exit 0 is
// not a failure). Now it keeps running and the error says so.
func TestSystemdE2E_RestartOnFailure(t *testing.T) {
	requireSystemdE2E(t)
	u, daemon, newBinary := e2eInstall(t)
	const unit = "pilot-e2e-onfailure.service"
	oldPid := startUnit(t, unit, daemon, "on-failure")
	if err := replaceBinary(newBinary, daemon); err != nil {
		t.Fatal(err)
	}

	err := u.signalDaemonRestart()
	if err == nil || !strings.Contains(err.Error(), "Restart=on-failure") {
		t.Fatalf("signalDaemonRestart = %v, want the Restart=on-failure error", err)
	}
	time.Sleep(2 * time.Second) // longer than RestartSec
	if pid := mainPID(t, unit); pid != oldPid {
		t.Errorf("MainPID = %d, want the untouched daemon %d", pid, oldPid)
	}
	if state := systemctl(t, "show", "-p", "ActiveState", "--value", unit); state != "active" {
		t.Errorf("ActiveState = %s, want active (the node must stay online)", state)
	}
}

// TestSystemdE2E_Unsupervised: a daemon started like `pilotctl daemon
// start` (detached with Setsid, no service) is not stopped.
func TestSystemdE2E_Unsupervised(t *testing.T) {
	requireSystemdE2E(t)
	u, daemon, newBinary := e2eInstall(t)
	cmd := exec.Command(daemon, "-test.run=^TestFakeDaemonProcess$")
	cmd.Env = append(os.Environ(), fakeDaemonEnv+"=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	deadline := time.Now().Add(5 * time.Second)
	for exeOf(cmd.Process.Pid) != daemon {
		if time.Now().After(deadline) {
			t.Fatal("fake daemon did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := replaceBinary(newBinary, daemon); err != nil {
		t.Fatal(err)
	}

	err := u.signalDaemonRestart()
	if err == nil || !strings.Contains(err.Error(), "pilotctl daemon stop && pilotctl daemon start") {
		t.Fatalf("signalDaemonRestart = %v, want the unsupervised error", err)
	}
	select {
	case err := <-exited:
		t.Fatalf("unsupervised daemon was stopped: %v", err)
	case <-time.After(time.Second):
	}
	if got := exeOf(cmd.Process.Pid); got != daemon+" (deleted)" {
		t.Errorf("daemon exe = %q, want it still running the old binary", got)
	}
}
