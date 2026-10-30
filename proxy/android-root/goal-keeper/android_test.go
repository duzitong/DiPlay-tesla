package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

type commandCall struct {
	name    string
	args    []string
	env     []string
	timeout time.Duration
}

type fakeRunner struct {
	calls []commandCall
	fn    func(commandCall) ([]byte, error)
}

func (r *fakeRunner) Run(_ context.Context, timeout time.Duration, env []string, name string, args ...string) ([]byte, error) {
	c := commandCall{name: name, args: append([]string(nil), args...), env: env, timeout: timeout}
	r.calls = append(r.calls, c)
	if r.fn != nil {
		return r.fn(c)
	}
	return nil, nil
}

type exitCode int

func (e exitCode) Error() string { return "command failed" }
func (e exitCode) ExitCode() int { return int(e) }

func TestExactGatewayIPv4(t *testing.T) {
	for _, tc := range []struct {
		output string
		want   bool
	}{
		{"4: br0: <UP>\n    inet 192.168.0.1/24 brd 192.168.0.255 scope global br0", true},
		{"    inet 192.168.0.10/24 scope global br0", false},
		{"    inet 192.168.1.1/24 scope global br0", false},
		{"    inet6 ::ffff:192.168.0.1/128", false},
		{"comment 192.168.0.1/24", false},
		{"    inet 192.168.0.1/invalid", false},
		{"", false},
	} {
		if got := exactIPv4([]byte(tc.output), "192.168.0.1"); got != tc.want {
			t.Errorf("exact IP parse = %v, want %v", got, tc.want)
		}
	}
	r := &fakeRunner{fn: func(c commandCall) ([]byte, error) {
		if c.name == getpropBin {
			return []byte("1\n"), nil
		}
		if !reflect.DeepEqual(c.args, []string{"-4", "addr", "show", "dev", "br0"}) {
			t.Fatalf("unexpected ip args: %q", c.args)
		}
		return []byte("inet 192.168.0.1/24"), nil
	}}
	h := &androidHost{cfg: sampleConfig(), runner: r}
	if ready, err := h.Boot(context.Background()); !ready || err != nil {
		t.Fatal("boot not ready")
	}
	if ready, err := h.Hotspot(context.Background()); !ready || err != nil {
		t.Fatal("hotspot not ready")
	}
	assertSafeCommands(t, r.calls)
}

func assertSafeCommands(t *testing.T, calls []commandCall) {
	t.Helper()
	for _, c := range calls {
		if !filepath.IsAbs(c.name) && !strings.HasPrefix(c.name, "/system/bin/") {
			t.Fatalf("relative executable: %q", c.name)
		}
		for _, forbidden := range []string{"busybox", "curl", "wget", "start.sh", "force-stop", "kill", "flush"} {
			if strings.Contains(c.name+" "+strings.Join(c.args, " "), forbidden) {
				t.Fatalf("forbidden command: %s %q", c.name, c.args)
			}
		}
		if filepath.Base(c.name) == "sh" || filepath.Base(c.name) == "su" {
			t.Fatal("shell execution forbidden")
		}
	}
}

func TestNATOnlyRepairsMissingAndVerifiesAll(t *testing.T) {
	cfg := sampleConfig()
	rules := natRules(cfg)
	want := [][]string{
		{"-i", "br0", "-d", "203.0.113.10/32", "-p", "tcp", "--dport", "9443", "-m", "comment", "--comment", "diplay-goal-keeper", "-j", "DNAT", "--to-destination", "192.168.0.1:9443"},
		{"-i", "br0", "-d", "203.0.113.10/32", "-p", "tcp", "--dport", "443", "-m", "comment", "--comment", "diplay-goal-keeper", "-j", "DNAT", "--to-destination", "192.168.0.1:9443"},
		{"-i", "br0", "-d", "203.0.113.10/32", "-p", "tcp", "--dport", "80", "-m", "comment", "--comment", "diplay-goal-keeper", "-j", "DNAT", "--to-destination", "192.168.0.1:8081"},
	}
	if !reflect.DeepEqual(rules, want) {
		t.Fatalf("incorrect exact rules: %#v", rules)
	}
	present := map[string]bool{"9443": true, "443": true, "80": true}
	inserted := []string{}
	r := &fakeRunner{fn: func(c commandCall) ([]byte, error) {
		if c.name != iptablesBin || !reflect.DeepEqual(c.args[:4], []string{"-w", "2", "-t", "nat"}) || c.args[5] != "PREROUTING" {
			t.Fatalf("unsafe iptables command: %+v", c)
		}
		args := c.args
		var port string
		for i, arg := range args {
			if arg == "--dport" {
				port = args[i+1]
			}
		}
		switch args[4] {
		case "-C":
			if !present[port] {
				return nil, exitCode(1)
			}
		case "-I":
			if args[6] != "1" {
				t.Fatal("must insert at PREROUTING 1")
			}
			present[port] = true
			inserted = append(inserted, port)
		default:
			t.Fatal("destructive or unexpected NAT operation")
		}
		return nil, nil
	}}
	h := &androidHost{cfg: cfg, runner: r}
	for i := 0; i < 3; i++ {
		if err := h.EnsureNAT(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(inserted) != 0 || len(r.calls) != 18 {
		t.Fatal("healthy NAT is not idempotent or final verification missing")
	}
	delete(present, "443")
	if err := h.EnsureNAT(context.Background()); err != nil || !reflect.DeepEqual(inserted, []string{"443"}) {
		t.Fatalf("must repair only lost rule: %v %v", inserted, err)
	}
	assertSafeCommands(t, r.calls)
}

func TestNATFailuresDoNotMasqueradeAsSuccess(t *testing.T) {
	for _, failure := range []string{"insert", "final-check", "permission"} {
		t.Run(failure, func(t *testing.T) {
			checks, inserts := 0, 0
			r := &fakeRunner{fn: func(c commandCall) ([]byte, error) {
				if c.args[4] == "-I" {
					inserts++
					return nil, errors.New("insert failed")
				}
				checks++
				if failure == "insert" && checks == 1 {
					return nil, exitCode(1)
				}
				if failure == "final-check" && checks == 5 {
					return nil, exitCode(1)
				}
				if failure == "permission" && checks == 1 {
					return nil, exitCode(2)
				}
				return nil, nil
			}}
			h := &androidHost{cfg: sampleConfig(), runner: r}
			if h.EnsureNAT(context.Background()) == nil || checks != 6 {
				t.Fatal("must report any failure and final-check all rules")
			}
			if failure != "insert" && inserts != 0 {
				t.Fatal("unexpected insert")
			}
		})
	}
}

func writeProcess(t *testing.T, dir string, pid int, args ...string) string {
	t.Helper()
	dir = filepath.Join(dir, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(strings.Join(args, "\x00")+"\x00"), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestAppProcessAndPackageDiagnostics(t *testing.T) {
	proc := t.TempDir()
	writeProcess(t, proc, 100, "com.diplay.tesla.debug")
	writeProcess(t, proc, 101, "com.diplay.tesla.fake")
	stopped, installed := false, true
	r := &fakeRunner{fn: func(c commandCall) ([]byte, error) {
		switch c.name {
		case pmBin:
			if installed {
				return []byte("package:/data/app/base.apk\n"), nil
			}
			return nil, nil
		case dumpsysBin:
			return []byte("    User 0: installed=true hidden=false stopped=" + strconv.FormatBool(stopped) + " notLaunched=false\n"), nil
		case amBin:
			if !reflect.DeepEqual(c.args, []string{"start", "--user", "0", "-n", "com.diplay.tesla/com.shilapi.xcertplay.DiPlayActivity"}) || c.timeout != 10*time.Second {
				t.Fatalf("unsafe app start: %+v", c)
			}
		}
		return nil, nil
	}}
	h := &androidHost{cfg: sampleConfig(), runner: r, procDir: proc}
	info, err := h.App(context.Background())
	if err != nil || info.Running || !info.Installed || info.Stopped {
		t.Fatalf("misidentified package/process: %+v %v", info, err)
	}
	if err := h.StartApp(context.Background()); err != nil {
		t.Fatal(err)
	}
	writeProcess(t, proc, 102, "com.diplay.tesla:worker")
	before := len(r.calls)
	if err := h.StartApp(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, call := range r.calls[before:] {
		if call.name == amBin {
			t.Fatal("running app must not be foregrounded repeatedly")
		}
	}
	if err := os.RemoveAll(filepath.Join(proc, "102")); err != nil {
		t.Fatal(err)
	}
	stopped = true
	info, err = h.App(context.Background())
	if err != nil || !info.Stopped || h.StartApp(context.Background()) == nil {
		t.Fatal("force-stopped package should be diagnostic, not started")
	}
	installed = false
	info, err = h.App(context.Background())
	if err != nil || info.Installed {
		t.Fatal("uninstalled package not recognized")
	}
	h.cfg.PackageName += ".debug"
	if h.StartApp(context.Background()) == nil {
		t.Fatal("debug auto-start not blocked")
	}
	assertSafeCommands(t, r.calls)
}

func TestCaddyIdentityIgnoresStalePIDAndOtherExecutables(t *testing.T) {
	proc := t.TempDir()
	cfg := sampleConfig()
	wanted := filepath.Join(cfg.GatewayDir, "caddy")
	p := writeProcess(t, proc, 22, wanted, "run", "--config", filepath.Join(cfg.GatewayDir, "Caddyfile"))
	if err := os.Symlink("/other/caddy", filepath.Join(p, "exe")); err != nil {
		t.Skip("file symlinks unavailable on this Windows host")
	}
	h := &androidHost{cfg: cfg, procDir: proc}
	if running, err := h.Caddy(context.Background()); err != nil || running {
		t.Fatalf("other executable must not count: %v %v", running, err)
	}
	os.Remove(filepath.Join(p, "exe"))
	if err := os.Symlink(wanted, filepath.Join(p, "exe")); err != nil {
		t.Fatal(err)
	}
	if running, err := h.Caddy(context.Background()); err != nil || !running {
		t.Fatalf("real executable/cmdline not recognized: %v %v", running, err)
	}
	os.Remove(filepath.Join(p, "cmdline"))
	if running, err := h.Caddy(context.Background()); err != nil || running {
		t.Fatal("exited PID must not count")
	}
}

func TestCaddyValidateBeforeStartAndNoLegacyScript(t *testing.T) {
	cfg := sampleConfig()
	cfg.GatewayDir, cfg.StateDir = t.TempDir(), t.TempDir()
	for _, name := range []string{"caddy", "Caddyfile"} {
		if err := os.WriteFile(filepath.Join(cfg.GatewayDir, name), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	r := &fakeRunner{}
	h := &androidHost{cfg: cfg, runner: r, procDir: t.TempDir()}
	if err := h.StartCaddy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 2 || r.calls[0].args[0] != "validate" || r.calls[1].args[0] != "start" {
		t.Fatal("caddy must validate then start")
	}
	for _, call := range r.calls {
		if call.name != filepath.Join(cfg.GatewayDir, "caddy") || call.timeout != 10*time.Second {
			t.Fatalf("wrong caddy invocation: %+v", call)
		}
		for _, env := range []string{"XDG_DATA_HOME=" + filepath.Join(cfg.GatewayDir, "state"), "XDG_CONFIG_HOME=" + filepath.Join(cfg.GatewayDir, "config")} {
			if !strings.Contains(strings.Join(call.env, "\n"), env) {
				t.Fatal("missing private caddy environment")
			}
		}
	}
	assertSafeCommands(t, r.calls)
	r.calls = nil
	r.fn = func(c commandCall) ([]byte, error) { return nil, errors.New("validation failure with secret output") }
	if h.StartCaddy(context.Background()) == nil || len(r.calls) != 1 {
		t.Fatal("must not start after failed validation")
	}
}

func TestCommandOutputBoundedAndRelativeExecRejected(t *testing.T) {
	var output boundedOutput
	data := make([]byte, 100000)
	if n, err := output.Write(data); err != nil || n != len(data) || len(output.data) != 64*1024 {
		t.Fatal("stdout not bounded")
	}
	output.Write(data)
	if len(output.data) != 64*1024 {
		t.Fatal("stdout grew past cap")
	}
	if _, err := (execRunner{}).Run(context.Background(), time.Second, nil, "busybox", "true"); err == nil {
		t.Fatal("relative command must never execute")
	}
}