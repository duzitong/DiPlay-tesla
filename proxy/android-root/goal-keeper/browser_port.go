package main

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

// Match listening socket inodes against DiPlay's own descriptors. Never guess
// from an open 8080: that port may instead belong to the ZTE admin portal.
func (h *androidHost) browserPort() (int, error) {
	sockets := map[string]bool{}
	entries, err := os.ReadDir(h.procDir)
	if err != nil {
		return 0, err
	}
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		dir := filepath.Join(h.procDir, entry.Name())
		cmd, err := os.ReadFile(filepath.Join(dir, "cmdline"))
		if err != nil {
			continue
		}
		name := strings.Split(string(cmd), "\x00")[0]
		if name != h.cfg.PackageName && !strings.HasPrefix(name, h.cfg.PackageName+":") {
			continue
		}
		fds, err := os.ReadDir(filepath.Join(dir, "fd"))
		if err != nil {
			return 0, errors.New("app_sockets_unreadable")
		}
		for _, fd := range fds {
			target, err := os.Readlink(filepath.Join(dir, "fd", fd.Name()))
			if err == nil && strings.HasPrefix(target, "socket:[") {
				sockets[strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")] = true
			}
		}
	}
	found := map[int]bool{}
	for _, table := range []string{"tcp", "tcp6"} {
		data, err := os.ReadFile(filepath.Join(h.procDir, "net", table))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 10 || fields[3] != "0A" || !sockets[fields[9]] {
				continue
			}
			address := strings.Split(fields[1], ":")
			if len(address) != 2 {
				continue
			}
			value, err := strconv.ParseInt(address[1], 16, 32)
			if err == nil && value >= 8080 && value <= 8085 {
				found[int(value)] = true
			}
		}
	}
	if found[h.cfg.UpstreamPort] {
		return h.cfg.UpstreamPort, nil
	}
	for port := 8080; port <= 8085; port++ {
		if found[port] {
			return port, nil
		}
	}
	return 0, errors.New("app_browser_socket_missing")
}

var loopbackBrowser = regexp.MustCompile(`127[.]0[.]0[.]1:808[0-5]\b`)

func (h *androidHost) syncGatewayPort() error {
	filename := filepath.Join(h.cfg.GatewayDir, "Caddyfile")
	if err := checkPlainFile(filename); err != nil {
		return err
	}
	original, err := os.ReadFile(filename)
	if err != nil {
		return err
	}
	if !loopbackBrowser.Match(original) {
		return errors.New("gateway_upstream_not_recognized")
	}
	updated := loopbackBrowser.ReplaceAll(original, []byte("127.0.0.1:"+strconv.Itoa(h.cfg.UpstreamPort)))
	for port := 8080; port <= 8085; port++ {
		updated = []byte(strings.ReplaceAll(string(updated), "127[.]0[.]0[.]1:"+strconv.Itoa(port), "127[.]0[.]0[.]1:"+strconv.Itoa(h.cfg.UpstreamPort)))
	}
	if string(original) == string(updated) {
		return nil
	}
	// Preserve a rollback copy, without exposing TLS keys or configuration in logs.
	backup := filepath.Join(h.cfg.StateDir, "Caddyfile.before-port-sync")
	if _, err := os.Lstat(backup); os.IsNotExist(err) {
		if err := atomicWrite(backup, original); err != nil {
			return err
		}
	}
	if err := atomicWrite(filename, updated); err != nil {
		return err
	}
	// admin is deliberately disabled in this deployment. Only stop a process
	// verified by both executable path and invocation, allowing Reconcile to start
	// it with the updated configuration on the next tick. Never touch DiPlay.
	_, err = scanProcesses(h.procDir, func(pid string, args []string) (bool, error) {
		bin := filepath.Join(h.cfg.GatewayDir, "caddy")
		if len(args) < 2 || (args[1] != "run" && args[1] != "start") || (args[0] != bin && args[0] != "caddy") {
			return false, nil
		}
		exe, e := os.Readlink(filepath.Join(h.procDir, pid, "exe"))
		if e != nil || exe != bin {
			return false, e
		}
		number, e := strconv.Atoi(pid)
		if e != nil {
			return false, e
		}
		process, e := os.FindProcess(number)
		if e != nil {
			return false, e
		}
		return false, process.Signal(syscall.SIGTERM)
	})
	return err
}
