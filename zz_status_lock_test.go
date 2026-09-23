// SPDX-License-Identifier: AGPL-3.0-or-later

package updater

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"
)

func incrementFailures(u *Updater, n int) {
	for i := 0; i < n; i++ {
		u.updateStatus(func(s *Status) { s.ConsecutiveFailures++ })
	}
}

// lockHeld reports whether another open file description holds the status
// lock for path.
func lockHeld(t *testing.T, path string) bool {
	t.Helper()
	f, err := os.Open(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return false
	}
	if !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatalf("flock: %v", err)
	}
	return true
}

// TestUpdateStatus_ConcurrentWritersDoNotLoseUpdates reproduces UPD49-2:
// two Updaters with separate mutexes (as two processes have) share one
// status file. Without a file lock their read-merge-write cycles interleaved
// and increments were lost (197 of 400 in the review's repro).
func TestUpdateStatus_ConcurrentWritersDoNotLoseUpdates(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), StatusFileName)
	const n = 200
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		u := &Updater{config: Config{StatusPath: path}}
		wg.Add(1)
		go func() {
			defer wg.Done()
			incrementFailures(u, n)
		}()
	}
	wg.Wait()
	if got := mustReadStatus(t, path).ConsecutiveFailures; got != 2*n {
		t.Fatalf("consecutive_failures = %d, want %d: concurrent writers lost updates", got, 2*n)
	}
}

// TestHelperStatusWriter is not a test: TestUpdateStatus_CrossProcessWriters
// runs it in child processes.
func TestHelperStatusWriter(t *testing.T) {
	path := os.Getenv("UPDATER_TEST_STATUS_WRITER_PATH")
	if path == "" {
		t.Skip("helper process for TestUpdateStatus_CrossProcessWriters")
	}
	n, err := strconv.Atoi(os.Getenv("UPDATER_TEST_STATUS_WRITER_N"))
	if err != nil {
		t.Fatal(err)
	}
	incrementFailures(&Updater{config: Config{StatusPath: path}}, n)
}

// TestUpdateStatus_CrossProcessWriters runs real separate processes (the
// updater service and `pilotctl update` are separate processes) against one
// status file, alongside this one.
func TestUpdateStatus_CrossProcessWriters(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), StatusFileName)
	const n, procs = 100, 3
	cmds := make([]*exec.Cmd, procs)
	outs := make([][]byte, procs)
	var wg sync.WaitGroup
	for i := range cmds {
		cmd := exec.Command(os.Args[0], "-test.run=^TestHelperStatusWriter$", "-test.count=1")
		cmd.Env = append(os.Environ(),
			"UPDATER_TEST_STATUS_WRITER_PATH="+path,
			"UPDATER_TEST_STATUS_WRITER_N="+strconv.Itoa(n),
		)
		cmds[i] = cmd
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			outs[i], _ = cmd.CombinedOutput()
		}(i)
	}
	incrementFailures(&Updater{config: Config{StatusPath: path}}, n)
	wg.Wait()
	for i, cmd := range cmds {
		if !cmd.ProcessState.Success() {
			t.Fatalf("writer %d failed: %s", i, outs[i])
		}
	}
	if got := mustReadStatus(t, path).ConsecutiveFailures; got != (procs+1)*n {
		t.Fatalf("consecutive_failures = %d, want %d: writers in other processes lost updates", got, (procs+1)*n)
	}
}

// TestUpdateStatus_HoldsLockDuringReadModifyWrite: the lock is held while
// mutate runs and released afterwards.
func TestUpdateStatus_HoldsLockDuringReadModifyWrite(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), StatusFileName)
	u := &Updater{config: Config{StatusPath: path}}
	var heldInside bool
	u.updateStatus(func(s *Status) {
		heldInside = lockHeld(t, path)
		s.LastResult = ResultUpToDate
	})
	if !heldInside {
		t.Error("status lock not held during read-merge-write")
	}
	if lockHeld(t, path) {
		t.Error("status lock still held after the write")
	}
	if st := mustReadStatus(t, path); st.LastResult != ResultUpToDate {
		t.Errorf("status = %+v", st)
	}
}

// TestUpdateStatus_LockTimeoutWritesUnlocked: a writer that cannot get the
// lock (another process wedged holding it) waits a bounded time, then
// records the outcome anyway. Recording must never block an update.
func TestUpdateStatus_LockTimeoutWritesUnlocked(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), StatusFileName)
	release := lockStatusFile(path, time.Second)
	defer release()
	if !lockHeld(t, path) {
		t.Fatal("setup: lock not taken")
	}

	const wait = 50 * time.Millisecond
	u := &Updater{config: Config{StatusPath: path}, statusLockWait: wait}
	start := time.Now()
	u.updateStatus(func(s *Status) { s.LastResult = ResultFailed })
	if d := time.Since(start); d < wait {
		t.Errorf("returned after %v, before the %v lock wait", d, wait)
	}
	if st := mustReadStatus(t, path); st.LastResult != ResultFailed {
		t.Errorf("status not written after lock timeout: %+v", st)
	}
}

// TestLockStatusFile_ReadOnlyLockFile: a lock file created by another user
// (e.g. `sudo pilotctl update`) is only readable; flock still works on it.
func TestLockStatusFile_ReadOnlyLockFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), StatusFileName)
	if err := os.WriteFile(path+".lock", nil, 0o444); err != nil {
		t.Fatal(err)
	}
	release := lockStatusFile(path, time.Second)
	defer release()
	if !lockHeld(t, path) {
		t.Error("lock not taken on a read-only lock file")
	}
}

// TestLockStatusFile_Unusable: when the lock file cannot be opened or its
// directory created, the write goes ahead unlocked.
func TestLockStatusFile_Unusable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, StatusFileName)
	if err := os.Mkdir(path+".lock", 0o755); err != nil { // a directory, not a file
		t.Fatal(err)
	}
	u := &Updater{config: Config{StatusPath: path}}
	u.updateStatus(func(s *Status) { s.LastResult = ResultUpToDate })
	if st := mustReadStatus(t, path); st.LastResult != ResultUpToDate {
		t.Errorf("status not written with an unusable lock file: %+v", st)
	}

	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	release := lockStatusFile(filepath.Join(blocker, "sub", StatusFileName), time.Second)
	release() // a no-op, must not panic
}
