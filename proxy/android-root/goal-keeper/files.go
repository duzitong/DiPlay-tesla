package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Every existing component is checked, not just the leaf. This also rejects a
// directory symlink that would escape the /data/local allowlist. StateDir must
// remain root-controlled; no portable API can defend against another root actor
// replacing directories concurrently.
func checkDirectory(dir string, create bool) error {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return errors.New("unsafe_directory")
	}
	volume := filepath.VolumeName(dir)
	current := volume + string(os.PathSeparator)
	for _, part := range strings.Split(strings.TrimPrefix(dir[len(volume):], string(os.PathSeparator)), string(os.PathSeparator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) && create {
			if err = os.Mkdir(current, 0700); err != nil {
				return errors.New("directory_create_failed")
			}
			info, err = os.Lstat(current)
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("unsafe_directory_component")
		}
	}
	return nil
}

func checkPlainFile(name string) error {
	info, err := os.Lstat(name)
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("not_regular_file")
	}
	return checkOwner(info)
}

func privateDir(dir string) error {
	if err := checkDirectory(dir, true); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if err := checkOwner(info); err != nil {
		return err
	}
	return os.Chmod(dir, 0700)
}

func preparePrivateFile(name string) error {
	if err := checkDirectory(filepath.Dir(name), false); err != nil {
		return err
	}
	if _, err := os.Lstat(name); err == nil {
		if err := checkPlainFile(name); err != nil {
			return err
		}
		return os.Chmod(name, 0600)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	return f.Close()
}

func atomicWrite(name string, data []byte) error {
	dir := filepath.Dir(name)
	if err := checkDirectory(dir, false); err != nil {
		return err
	}
	if _, err := os.Lstat(name); err == nil {
		if err := checkPlainFile(name); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.CreateTemp(dir, ".keeper-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(f.Name(), name); err != nil {
		return err
	}
	return syncDir(dir)
}

func writeJSON(name string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return atomicWrite(name, append(data, '\n'))
}

func loadAttempts(name string) (Attempts, error) {
	var a Attempts
	if _, err := os.Lstat(name); errors.Is(err, os.ErrNotExist) {
		return a, nil
	}
	if err := checkPlainFile(name); err != nil {
		return a, err
	}
	f, err := os.Open(name)
	if err != nil {
		return a, err
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 4096))
	d.DisallowUnknownFields()
	if err := d.Decode(&a); err != nil {
		return a, errors.New("invalid_attempt_state")
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return a, errors.New("invalid_attempt_state")
	}
	return a, nil
}

const maxLogSize int64 = 1 << 20

// At most two 1 MiB files, and one entry per state transition (not every tick).
// State/status contain no upstream body or raw command error/output.
func logTransition(dir string, s State) error {
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	name, previous := filepath.Join(dir, "keeper.log"), filepath.Join(dir, "keeper.log.1")
	if err := preparePrivateFile(name); err != nil {
		return err
	}
	info, err := os.Stat(name)
	if err != nil {
		return err
	}
	if info.Size()+int64(len(data)) > maxLogSize {
		if _, err := os.Lstat(previous); err == nil {
			if err := checkPlainFile(previous); err != nil {
				return err
			}
			if err := os.Remove(previous); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := os.Rename(name, previous); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}