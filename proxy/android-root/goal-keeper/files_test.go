package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLockExcludesSecondInstanceAndReleases(t *testing.T) {
	name := filepath.Join(t.TempDir(), "keeper.lock")
	first, err := acquireLock(name)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if second, err := acquireLock(name); err == nil {
		second.Close()
		t.Fatal("second instance acquired lock")
	}
	// Verify interprocess exclusion too, not only two handles in one process.
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockChild$")
	cmd.Env = append(os.Environ(), "GOAL_LOCK_CHILD="+name)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child lock test failed: %v %s", err, output)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := acquireLock(name)
	if err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	third.Close()
	if _, err := os.Stat(name); err != nil {
		t.Fatal("lock file must not be unlinked")
	}
}

func TestLockChild(t *testing.T) {
	name := os.Getenv("GOAL_LOCK_CHILD")
	if name == "" {
		return
	}
	if lock, err := acquireLock(name); err == nil {
		lock.Close()
		t.Fatal("child acquired parent's lock")
	}
}

func TestAtomicPrivateStatusAndAttempts(t *testing.T) {
	dir := t.TempDir()
	if err := privateDir(dir); err != nil {
		t.Fatal(err)
	}
	status := filepath.Join(dir, "status.json")
	for i := 0; i < 10; i++ {
		s := State{BootReady: true, Timestamp: time.Unix(int64(i), 0).UTC()}
		if err := writeJSON(status, s); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(status)
		var got State
		if json.Unmarshal(data, &got) != nil || got != s {
			t.Fatal("atomic status not complete")
		}
	}
	pid := filepath.Join(dir, "keeper.pid")
	if err := atomicWrite(pid, []byte("123\n")); err != nil {
		t.Fatal(err)
	}
	attemptFile := filepath.Join(dir, "attempts.json")
	if got, err := loadAttempts(attemptFile); err != nil || got != (Attempts{}) {
		t.Fatal("missing attempt file is not initial state")
	}
	want := Attempts{App: time.Unix(100, 0).UTC(), Caddy: time.Unix(200, 0).UTC()}
	if err := writeJSON(attemptFile, want); err != nil {
		t.Fatal(err)
	}
	if got, err := loadAttempts(attemptFile); err != nil || got != want {
		t.Fatalf("backoff not persisted: %+v %v", got, err)
	}
	for _, name := range []string{status, pid, attemptFile} {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
			t.Fatalf("not root-private: %s %o", name, info.Mode().Perm())
		}
	}
	entries, _ := os.ReadDir(dir)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".keeper-") {
			t.Fatal("temporary file leaked")
		}
	}
	if err := os.WriteFile(attemptFile, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadAttempts(attemptFile); err == nil {
		t.Fatal("corrupt attempt file must not reset launch backoff")
	}
}

func TestSymlinkEscapesRejected(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	link := filepath.Join(dir, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skip("directory symlinks unavailable on this Windows host")
	}
	if checkDirectory(filepath.Join(link, "child"), true) == nil {
		t.Fatal("directory symlink escaped path policy")
	}
	if _, err := os.Stat(filepath.Join(outside, "child")); !os.IsNotExist(err) {
		t.Fatal("created a directory outside path policy")
	}
	target := filepath.Join(outside, "target")
	os.WriteFile(target, []byte("do not modify"), 0600)
	fileLink := filepath.Join(dir, "status.json")
	if err := os.Symlink(target, fileLink); err != nil {
		t.Fatal(err)
	}
	if atomicWrite(fileLink, []byte("bad")) == nil || preparePrivateFile(fileLink) == nil {
		t.Fatal("private file symlink accepted")
	}
	if lock, err := acquireLock(fileLink); err == nil {
		lock.Close()
		t.Fatal("lock symlink accepted")
	}
	if data, _ := os.ReadFile(target); string(data) != "do not modify" {
		t.Fatal("modified symlink target")
	}
}

func TestLogBoundedRotationAndStateChangeKey(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "keeper.log")
	if err := os.WriteFile(name, []byte(strings.Repeat("x", int(maxLogSize)-10)), 0600); err != nil {
		t.Fatal(err)
	}
	s := State{Timestamp: time.Unix(1000, 0).UTC(), LastError: "boot_pending"}
	if err := logTransition(dir, s); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"keeper.log", "keeper.log.1"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Size() > maxLogSize {
			t.Fatal("log rotation bound exceeded")
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
			t.Fatal("log not private")
		}
	}
	key := stateKey(s)
	s.Timestamp = s.Timestamp.Add(time.Second)
	if stateKey(s) != key {
		t.Fatal("timestamps must not cause a log entry every tick")
	}
	s.BootReady = true
	if stateKey(s) == key {
		t.Fatal("state change not detected")
	}
}