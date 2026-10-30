//go:build linux

package main

import (
	"errors"
	"os"
	"syscall"
)

func requireRoot() error {
	if os.Geteuid() != 0 {
		return errors.New("root_required")
	}
	return nil
}

func checkOwner(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || (!info.IsDir() && stat.Nlink != 1) {
		return errors.New("unsafe_owner_or_hardlink")
	}
	return nil
}

type instanceLock struct{ file *os.File }

func acquireLock(name string) (*instanceLock, error) {
	if err := preparePrivateFile(name); err != nil {
		return nil, err
	}
	fd, err := syscall.Open(name, syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), name)
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("already_running_or_lock_failed")
	}
	return &instanceLock{file: f}, nil
}

func (l *instanceLock) Close() error {
	// Close releases flock; never unlink a lock file (which would permit a second inode).
	return l.file.Close()
}

func syncDir(name string) error {
	f, err := os.Open(name)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}