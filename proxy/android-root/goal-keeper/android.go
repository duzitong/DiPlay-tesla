package main

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	getpropBin  = "/system/bin/getprop"
	ipBin       = "/system/bin/ip"
	pmBin       = "/system/bin/pm"
	dumpsysBin  = "/system/bin/dumpsys"
	amBin       = "/system/bin/am"
	iptablesBin = "/system/bin/iptables"
)

type Runner interface {
	Run(context.Context, time.Duration, []string, string, ...string) ([]byte, error)
}

type execRunner struct{}

// boundedOutput retains only a small prefix of stdout. stderr is discarded and
// command output is never included in status/errors. A noisy process cannot OOM us.
type boundedOutput struct{ data []byte }

func (b *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := 64*1024 - len(b.data)
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		b.data = append(b.data, p...)
	}
	return n, nil
}

func (execRunner) Run(ctx context.Context, timeout time.Duration, env []string, name string, args ...string) ([]byte, error) {
	if !filepath.IsAbs(name) {
		return nil, errors.New("nonabsolute executable")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	// Do not inherit LD_PRELOAD, SSLKEYLOGFILE, proxy settings or arbitrary
	// environment secrets. Every executed binary is an absolute path.
	cmd.Env = append([]string{"PATH=/system/bin:/system/xbin", "HOME=/", "LANG=C", "ANDROID_ROOT=/system", "ANDROID_DATA=/data"}, env...)
	var output boundedOutput
	cmd.Stdout = &output
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	return output.data, err
}

type androidHost struct {
	cfg     Config
	runner  Runner
	procDir string
	probes  *prober
}

func (h *androidHost) command(ctx context.Context, name string, args ...string) ([]byte, error) {
	return h.runner.Run(ctx, 3*time.Second, nil, name, args...)
}

func (h *androidHost) Boot(ctx context.Context) (bool, error) {
	out, err := h.command(ctx, getpropBin, "sys.boot_completed")
	return strings.TrimSpace(string(out)) == "1" && err == nil, err
}

func (h *androidHost) Hotspot(ctx context.Context) (bool, error) {
	out, err := h.command(ctx, ipBin, "-4", "addr", "show", "dev", h.cfg.Interface)
	if err != nil {
		return false, err
	}
	return exactIPv4(out, h.cfg.GatewayIP), nil
}

func exactIPv4(output []byte, wanted string) bool {
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "inet" {
			prefix, err := netip.ParsePrefix(fields[1])
			if err == nil && prefix.Addr().Is4() && prefix.Addr().String() == wanted {
				return true
			}
		}
	}
	return false
}

func (h *androidHost) App(ctx context.Context) (AppInfo, error) {
	running, err := scanProcesses(h.procDir, func(pid string, args []string) (bool, error) {
		return args[0] == h.cfg.PackageName || strings.HasPrefix(args[0], h.cfg.PackageName+":"), nil
	})
	info := AppInfo{Running: running}
	if err != nil {
		return info, err
	}
	out, err := h.command(ctx, pmBin, "path", "--user", "0", h.cfg.PackageName)
	if err != nil {
		// pm may exit 1 for an absent package. dumpsys below distinguishes
		// absence from service/permission failures without trusting stderr.
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return info, errors.New("package_query_failed")
		}
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "package:/") {
			info.Installed = true
		}
	}
	dump, err := h.command(ctx, dumpsysBin, "package", h.cfg.PackageName)
	if err != nil {
		return info, errors.New("package_state_failed")
	}
	if !info.Installed {
		// A successful dumpsys query plus no pm path is a definite absence.
		return info, nil
	}
	for _, line := range strings.Split(string(dump), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "User 0:") {
			continue
		}
		var stoppedKnown, installedKnown bool
		for _, field := range strings.Fields(line) {
			switch field {
			case "stopped=true":
				info.Stopped, stoppedKnown = true, true
			case "stopped=false":
				stoppedKnown = true
			case "installed=true":
				installedKnown = true
			case "installed=false":
				info.Installed, installedKnown = false, true
			}
		}
		if stoppedKnown && installedKnown {
			return info, nil
		}
	}
	return info, errors.New("package_user_state_unknown")
}

// scanProcesses does not trust PID files. A non-transient /proc read failure is
// returned conservatively: the caller must not launch a possibly running process.
func scanProcesses(proc string, match func(string, []string) (bool, error)) (bool, error) {
	entries, err := os.ReadDir(proc)
	if err != nil {
		return false, err
	}
	var observationErr error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		data, err := os.ReadFile(filepath.Join(proc, entry.Name(), "cmdline"))
		if errors.Is(err, os.ErrNotExist) {
			continue // normal process exit race
		}
		if err != nil {
			observationErr = errors.New("proc_unreadable")
			continue
		}
		if len(data) == 0 {
			continue // kernel thread
		}
		args := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
		found, err := match(entry.Name(), args)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			observationErr = errors.New("proc_identity_unreadable")
		}
		if found && err == nil {
			return true, nil
		}
	}
	return false, observationErr
}

func (h *androidHost) Caddy(context.Context) (bool, error) {
	wanted := filepath.Join(h.cfg.GatewayDir, "caddy")
	return scanProcesses(h.procDir, func(pid string, args []string) (bool, error) {
		// Validate both the running executable and its invocation. A process
		// merely called caddy (or a reused PID) is not our gateway.
		if len(args) < 2 || (args[1] != "run" && args[1] != "start") {
			return false, nil
		}
		if args[0] != wanted && args[0] != "caddy" {
			return false, nil
		}
		exe, err := os.Readlink(filepath.Join(h.procDir, pid, "exe"))
		return exe == wanted && err == nil, err
	})
}

func (h *androidHost) Upstream(ctx context.Context) error {
	port, err := h.browserPort()
	if err != nil {
		return err
	}
	if port != h.cfg.UpstreamPort {
		h.cfg.UpstreamPort = port
		h.probes = newProber(h.cfg, nil)
	}
	if err := h.probes.upstream(ctx); err != nil {
		return err
	}
	return h.syncGatewayPort()
}
func (h *androidHost) HTTPS(ctx context.Context) error { return h.probes.https(ctx) }

func (h *androidHost) StartApp(ctx context.Context) error {
	if h.cfg.PackageName != "com.diplay.tesla" {
		return errors.New("debug_autostart_disabled")
	}
	// Recheck immediately before acting to close the common observation/action race.
	info, err := h.App(ctx)
	if err != nil || !info.Installed || info.Stopped {
		return errors.New("app_start_precondition_changed")
	}
	if info.Running {
		return nil
	}
	_, err = h.runner.Run(ctx, 10*time.Second, nil, amBin, "start", "--user", "0", "-n", h.cfg.PackageName+"/com.shilapi.xcertplay.DiPlayActivity")
	return err
}

func (h *androidHost) StartCaddy(ctx context.Context) error {
	running, err := h.Caddy(ctx)
	if err != nil || running {
		return err
	}
	if err := checkDirectory(h.cfg.GatewayDir, false); err != nil {
		return errors.New("unsafe_gateway_directory")
	}
	for _, name := range []string{"caddy", "Caddyfile"} {
		if err := checkPlainFile(filepath.Join(h.cfg.GatewayDir, name)); err != nil {
			return errors.New("unsafe_gateway_file")
		}
	}
	for _, dir := range []string{"state", "config"} {
		if err := checkDirectory(filepath.Join(h.cfg.GatewayDir, dir), true); err != nil {
			return errors.New("unsafe_caddy_directory")
		}
	}
	// One shared deadline bounds validation plus start, not 10 seconds each.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	bin := filepath.Join(h.cfg.GatewayDir, "caddy")
	config := filepath.Join(h.cfg.GatewayDir, "Caddyfile")
	env := []string{"XDG_DATA_HOME=" + filepath.Join(h.cfg.GatewayDir, "state"), "XDG_CONFIG_HOME=" + filepath.Join(h.cfg.GatewayDir, "config"), "SSL_CERT_DIR=" + androidCertDirs}
	if _, err := h.runner.Run(ctx, 10*time.Second, env, bin, "validate", "--config", config, "--adapter", "caddyfile"); err != nil {
		return errors.New("caddy_validation_failed")
	}
	// PID file is informational; process identity is always established via /proc.
	pidfile := filepath.Join(h.cfg.StateDir, "caddy.pid")
	if err := preparePrivateFile(pidfile); err != nil {
		return err
	}
	_, err = h.runner.Run(ctx, 10*time.Second, env, bin, "start", "--config", config, "--adapter", "caddyfile", "--pidfile", pidfile)
	return err
}

func natRules(c Config) [][]string {
	ports := []struct{ from, to int }{{c.HTTPSPort, c.HTTPSPort}, {443, c.HTTPSPort}, {80, c.UpstreamPort}}
	rules := make([][]string, 0, len(ports))
	for _, p := range ports {
		rules = append(rules, []string{"-i", c.Interface, "-d", c.PublicIP + "/32", "-p", "tcp", "--dport", strconv.Itoa(p.from), "-m", "comment", "--comment", "diplay-goal-keeper", "-j", "DNAT", "--to-destination", fmt.Sprintf("%s:%d", c.GatewayIP, p.to)})
	}
	return rules
}

func (h *androidHost) EnsureNAT(ctx context.Context) error {
	rules := natRules(h.cfg)
	failed := false
	check := func(rule []string) error {
		args := append([]string{"-w", "2", "-t", "nat", "-C", "PREROUTING"}, rule...)
		_, err := h.command(ctx, iptablesBin, args...)
		return err
	}
	for _, rule := range rules {
		if err := check(rule); err != nil {
			// Only iptables exit 1 means missing. Unsupported commands, permission
			// errors and xtables lock failures must not blindly insert duplicates.
			var exit interface{ ExitCode() int }
			if !errors.As(err, &exit) || exit.ExitCode() != 1 {
				failed = true
				continue
			}
			args := append([]string{"-w", "2", "-t", "nat", "-I", "PREROUTING", "1"}, rule...)
			if _, err := h.command(ctx, iptablesBin, args...); err != nil {
				failed = true
			}
		}
	}
	// Verify every rule again, including rules whose insertion failed. A single
	// failed command never becomes a success just because another rule succeeded.
	for _, rule := range rules {
		if check(rule) != nil {
			failed = true
		}
	}
	if failed {
		return errors.New("nat_rule_verification_failed")
	}
	return nil
}
