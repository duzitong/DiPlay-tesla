//go:build windows

package main

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

// Windows supports -check and unit tests, not the Android daemon.
func requireRoot() error { return errors.New("android_linux_root_required") }
func checkOwner(os.FileInfo) error { return nil }
func syncDir(string) error { return nil }

type instanceLock struct {
	file *os.File
	overlap syscall.Overlapped
}

var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	lockFileEx = kernel32.NewProc("LockFileEx")
	unlockFileEx = kernel32.NewProc("UnlockFileEx")
)

func acquireLock(name string) (*instanceLock, error) {
	if err := preparePrivateFile(name); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(name, os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	l := &instanceLock{file: f}
	// LOCKFILE_EXCLUSIVE_LOCK | LOCKFILE_FAIL_IMMEDIATELY, one byte at offset 0.
	r, _, _ := lockFileEx.Call(f.Fd(), 3, 0, 1, 0, uintptr(unsafe.Pointer(&l.overlap)))
	if r == 0 {
		f.Close()
		return nil, errors.New("already_running_or_lock_failed")
	}
	return l, nil
}

func (l *instanceLock) Close() error {
	unlockFileEx.Call(l.file.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(&l.overlap)))
	return l.file.Close()
}