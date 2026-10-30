package main

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const (
	loopInterval = 10 * time.Second
	appBackoff   = 120 * time.Second
	caddyBackoff = 30 * time.Second
)

type State struct {
	BootReady     bool      `json:"boot_ready"`
	HotspotReady  bool      `json:"hotspot_ready"`
	UpstreamReady bool      `json:"upstream_ready"`
	HTTPSReady    bool      `json:"https_ready"`
	NATReady      bool      `json:"nat_ready"`
	LocalReady    bool      `json:"local_ready"`
	AppRunning    bool      `json:"app_running"`
	CaddyRunning  bool      `json:"caddy_running"`
	LastError     string    `json:"last_error"`
	Timestamp     time.Time `json:"timestamp"`
}

// AppInfo distinguishes an observation failure from a definitely absent app.
type AppInfo struct {
	Running   bool
	Installed bool
	Stopped   bool
}

// Host separates observation/actions from policy. Implementations never invoke a shell.
type Host interface {
	Boot(context.Context) (bool, error)
	Hotspot(context.Context) (bool, error)
	App(context.Context) (AppInfo, error)
	Caddy(context.Context) (bool, error)
	Upstream(context.Context) error
	HTTPS(context.Context) error
	StartApp(context.Context) error
	StartCaddy(context.Context) error
	EnsureNAT(context.Context) error
}

type Keeper struct {
	Config Config
	Host   Host
	Now    func() time.Time
	// Hook persists attempt times BEFORE actions, preserving backoff across -once
	// invocations and daemon restarts. A failed write prevents the action.
	SaveAttempts func(Attempts) error
	Attempts     Attempts
}

type Attempts struct {
	App   time.Time `json:"app"`
	Caddy time.Time `json:"caddy"`
}

func due(now, previous time.Time, interval time.Duration) bool {
	// A backward clock jump does not create a launch storm.
	return previous.IsZero() || (!now.Before(previous) && now.Sub(previous) >= interval)
}

func (k *Keeper) reserve(app bool, now time.Time) error {
	next := k.Attempts
	if app {
		next.App = now
	} else {
		next.Caddy = now
	}
	if k.SaveAttempts != nil {
		if err := k.SaveAttempts(next); err != nil {
			return errors.New("attempt_state_write_failed")
		}
	}
	k.Attempts = next
	return nil
}

func (k *Keeper) Reconcile(ctx context.Context) State {
	now := time.Now()
	if k.Now != nil {
		now = k.Now()
	}
	s := State{Timestamp: now.UTC()}
	var problems []string
	add := func(code string) { problems = append(problems, code) }
	var err error
	s.BootReady, err = k.Host.Boot(ctx)
	if err != nil {
		s.BootReady = false
		add("boot_observation_failed")
	} else if !s.BootReady {
		add("boot_pending")
	}
	s.HotspotReady, err = k.Host.Hotspot(ctx)
	if err != nil {
		s.HotspotReady = false
		add("hotspot_observation_failed")
	} else if !s.HotspotReady {
		add("hotspot_ipv4_missing")
	}
	app, appErr := k.Host.App(ctx)
	s.AppRunning = app.Running
	if appErr != nil {
		add("app_observation_failed")
	} else if !app.Installed {
		add("app_not_installed")
	} else if app.Stopped {
		add("app_force_stopped")
	}
	caddy, caddyErr := k.Host.Caddy(ctx)
	s.CaddyRunning = caddy
	if caddyErr != nil {
		add("caddy_observation_failed")
	}
	if err := k.Host.Upstream(ctx); err != nil {
		add("upstream_" + probeClass(err))
	} else {
		s.UpstreamReady = true
	}
	if err := k.Host.HTTPS(ctx); err != nil {
		add("https_" + probeClass(err))
	} else {
		s.HTTPSReady = true
	}

	if s.BootReady && !s.UpstreamReady && !s.AppRunning && appErr == nil && app.Installed && !app.Stopped {
		if k.Config.PackageName != "com.diplay.tesla" {
			add("debug_autostart_disabled")
		} else if due(now, k.Attempts.App, appBackoff) {
			if err := k.reserve(true, now); err != nil {
				add("attempt_state_write_failed")
			} else if k.Host.StartApp(ctx) != nil {
				add("app_start_failed")
			}
		}
	}
	if s.BootReady && s.HotspotReady && s.UpstreamReady && !s.HTTPSReady && !s.CaddyRunning && caddyErr == nil && due(now, k.Attempts.Caddy, caddyBackoff) {
		if err := k.reserve(false, now); err != nil {
			add("attempt_state_write_failed")
		} else if k.Host.StartCaddy(ctx) != nil {
			add("caddy_start_failed")
		}
	}
	s.LocalReady = s.BootReady && s.HotspotReady && s.UpstreamReady && s.HTTPSReady
	if s.LocalReady {
		if k.Host.EnsureNAT(ctx) != nil {
			add("nat_rule_verification_failed")
		} else {
			s.NATReady = true
		}
	}
	s.LastError = strings.Join(problems, ";")
	return s
}

// All external errors are mapped to a fixed vocabulary. Never log response data,
// executable output, certificate text, URLs, tokens or wrapped error strings.
type probeError string

func (e probeError) Error() string { return string(e) }

func probeClass(err error) string {
	var p probeError
	if errors.As(err, &p) {
		switch p {
		case "timeout", "tls_certificate", "tls", "http_status", "connect":
			return string(p)
		}
	}
	return "probe_failed"
}

func stateKey(s State) string {
	s.Timestamp = time.Time{}
	b, _ := json.Marshal(s)
	return string(b)
}