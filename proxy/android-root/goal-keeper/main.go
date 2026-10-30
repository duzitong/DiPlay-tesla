package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

// Exit codes: 0 = local + NAT ready (NOT proof of external reachability),
// 1 = not ready, 2 = configuration/usage, 3 = root/lock/storage/runtime setup.
func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, out, errOut io.Writer) int {
	flags := flag.NewFlagSet("goal-keeper", flag.ContinueOnError)
	flags.SetOutput(errOut)
	configFile := flags.String("config", "/data/local/diplay-goal-keeper/config.json", "JSON configuration file")
	check := flags.Bool("check", false, "validate configuration only; no root or mutations")
	once := flags.Bool("once", false, "one reconciliation; 0=local+NAT ready, 1=not ready")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 || (*check && *once) {
		fmt.Fprintln(errOut, "invalid arguments")
		return 2
	}
	cfg, err := LoadConfig(*configFile)
	if err != nil {
		fmt.Fprintln(errOut, "configuration invalid:", err)
		return 2
	}
	if *check {
		fmt.Fprintln(out, "configuration valid (schema only; runtime paths and health not checked)")
		return 0
	}
	if requireRoot() != nil {
		fmt.Fprintln(errOut, "Android/Linux root required")
		return 3
	}
	if privateDir(cfg.StateDir) != nil {
		fmt.Fprintln(errOut, "unsafe or inaccessible state directory")
		return 3
	}
	lock, err := acquireLock(filepath.Join(cfg.StateDir, "keeper.lock"))
	if err != nil {
		fmt.Fprintln(errOut, "already running or cannot acquire private lock")
		return 3
	}
	defer lock.Close()
	pidfile := filepath.Join(cfg.StateDir, "keeper.pid")
	if atomicWrite(pidfile, []byte(strconv.Itoa(os.Getpid())+"\n")) != nil {
		fmt.Fprintln(errOut, "cannot write private PID")
		return 3
	}
	defer os.Remove(pidfile)
	// Set BEFORE x509 initializes its cached system roots. These include the
	// Android 13 legacy store and the Conscrypt APEX store on newer devices.
	if os.Setenv("SSL_CERT_DIR", androidCertDirs) != nil || os.Unsetenv("SSL_CERT_FILE") != nil {
		fmt.Fprintln(errOut, "cannot configure system certificate directories")
		return 3
	}
	if checkDirectory(cfg.GatewayDir, false) != nil {
		fmt.Fprintln(errOut, "unsafe or inaccessible gateway directory")
		return 3
	}
	attemptFile := filepath.Join(cfg.StateDir, "attempts.json")
	attempts, err := loadAttempts(attemptFile)
	if err != nil {
		fmt.Fprintln(errOut, "invalid or unreadable attempt state; refusing to bypass backoff")
		return 3
	}
	host := &androidHost{cfg: cfg, runner: execRunner{}, procDir: "/proc", probes: newProber(cfg, nil)}
	k := &Keeper{Config: cfg, Host: host, Attempts: attempts, SaveAttempts: func(a Attempts) error { return writeJSON(attemptFile, a) }}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ticker := time.NewTicker(loopInterval)
	defer ticker.Stop()
	last := ""
	for {
		s := k.Reconcile(ctx)
		if writeJSON(filepath.Join(cfg.StateDir, "status.json"), s) != nil {
			fmt.Fprintln(errOut, "cannot write private status")
			return 3
		}
		key := stateKey(s)
		if key != last {
			if logTransition(cfg.StateDir, s) != nil {
				fmt.Fprintln(errOut, "cannot write bounded private transition log")
				return 3
			}
			last = key
		}
		if *once {
			if s.LocalReady && s.NATReady {
				return 0
			}
			return 1
		}
		select {
		case <-ctx.Done():
			return 0
		case <-ticker.C:
		}
	}
}