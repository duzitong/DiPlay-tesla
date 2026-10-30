package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeHost struct {
	boot, hotspot, caddy bool
	app                  AppInfo
	bootErr, hotErr       error
	appErr, caddyErr      error
	upErr, tlsErr, natErr error
	appStartErr          error
	caddyStartErr        error
	appStarts            int
	caddyStarts          int
	natCalls             int
	observed             []string
}

func (h *fakeHost) Boot(context.Context) (bool, error) {
	h.observed = append(h.observed, "boot")
	return h.boot, h.bootErr
}
func (h *fakeHost) Hotspot(context.Context) (bool, error) {
	h.observed = append(h.observed, "hotspot")
	return h.hotspot, h.hotErr
}
func (h *fakeHost) App(context.Context) (AppInfo, error) {
	h.observed = append(h.observed, "app")
	return h.app, h.appErr
}
func (h *fakeHost) Caddy(context.Context) (bool, error) {
	h.observed = append(h.observed, "caddy")
	return h.caddy, h.caddyErr
}
func (h *fakeHost) Upstream(context.Context) error {
	h.observed = append(h.observed, "upstream")
	return h.upErr
}
func (h *fakeHost) HTTPS(context.Context) error {
	h.observed = append(h.observed, "https")
	return h.tlsErr
}
func (h *fakeHost) StartApp(context.Context) error {
	h.appStarts++
	return h.appStartErr
}
func (h *fakeHost) StartCaddy(context.Context) error {
	h.caddyStarts++
	return h.caddyStartErr
}
func (h *fakeHost) EnsureNAT(context.Context) error {
	h.natCalls++
	return h.natErr
}

func healthyHost() *fakeHost {
	return &fakeHost{boot: true, hotspot: true, caddy: true, app: AppInfo{Running: true, Installed: true}}
}

func testKeeper(h Host) *Keeper {
	return &Keeper{Config: sampleConfig(), Host: h, Now: func() time.Time { return time.Unix(1000, 0) }}
}

func TestDelayedPrerequisitesRecoverWithoutEarlyReturn(t *testing.T) {
	h := &fakeHost{app: AppInfo{Installed: true}, upErr: probeError("connect"), tlsErr: probeError("connect")}
	k := testKeeper(h)
	ctx := context.Background()
	s := k.Reconcile(ctx)
	if len(h.observed) != 6 || s.LocalReady || s.NATReady || h.appStarts != 0 || h.caddyStarts != 0 {
		t.Fatalf("bad initial reconciliation: %+v / %+v", s, h)
	}
	h.boot = true
	k.Reconcile(ctx)
	if h.appStarts != 1 || h.caddyStarts != 0 {
		t.Fatal("app should start after boot, independently of hotspot")
	}
	h.app.Running = true
	h.upErr = nil
	s = k.Reconcile(ctx)
	if !s.UpstreamReady || !s.AppRunning || s.HotspotReady || h.caddyStarts != 0 {
		t.Fatal("existing layers lost while hotspot pending")
	}
	h.hotspot = true
	k.Reconcile(ctx)
	if h.caddyStarts != 1 {
		t.Fatal("gateway not started when prerequisites recovered")
	}
	h.caddy = true
	h.tlsErr = nil
	s = k.Reconcile(ctx)
	if !s.LocalReady || !s.NATReady || !s.CaddyRunning || h.natCalls != 1 {
		t.Fatalf("failed to converge: %+v", s)
	}
	for i := 0; i < 10; i++ {
		k.Reconcile(ctx)
	}
	if h.appStarts != 1 || h.caddyStarts != 1 {
		t.Fatal("healthy state causes repeated starts")
	}
}

func TestExistingProcessesNeverRestartOnProbeFailure(t *testing.T) {
	h := healthyHost()
	h.upErr = errors.New("secret html token=not-for-status")
	h.tlsErr = probeError("tls_certificate")
	k := testKeeper(h)
	for i := 0; i < 100; i++ {
		s := k.Reconcile(context.Background())
		if !s.AppRunning || !s.CaddyRunning || s.NATReady || s.LocalReady || s.HTTPSReady || strings.Contains(s.LastError, "secret") {
			t.Fatalf("unsafe failure state: %+v", s)
		}
	}
	if h.appStarts != 0 || h.caddyStarts != 0 || h.natCalls != 0 {
		t.Fatal("must not restart/kill existing processes or install NAT on unhealthy gateway")
	}
}

func TestLaunchBackoffAndPersistedReservations(t *testing.T) {
	t.Run("app", func(t *testing.T) {
		h := healthyHost()
		h.app.Running, h.upErr = false, probeError("connect")
		h.appStartErr = errors.New("failed")
		k := testKeeper(h)
		now := time.Unix(1000, 0)
		k.Now = func() time.Time { return now }
		var saved Attempts
		k.SaveAttempts = func(a Attempts) error { saved = a; return nil }
		k.Reconcile(context.Background())
		if saved.App != now || h.appStarts != 1 {
			t.Fatal("attempt not reserved")
		}
		k = &Keeper{Config: sampleConfig(), Host: h, Now: func() time.Time { return now }, Attempts: saved}
		for _, seconds := range []int{10, 30, 119} {
			now = time.Unix(1000+int64(seconds), 0)
			k.Reconcile(context.Background())
		}
		if h.appStarts != 1 {
			t.Fatal("120 second backoff bypassed")
		}
		now = time.Unix(1120, 0)
		k.Reconcile(context.Background())
		if h.appStarts != 2 {
			t.Fatal("failed launch not retried")
		}
	})
	t.Run("caddy", func(t *testing.T) {
		h := healthyHost()
		h.caddy, h.tlsErr, h.caddyStartErr = false, probeError("connect"), errors.New("failed")
		k := testKeeper(h)
		now := time.Unix(1000, 0)
		k.Now = func() time.Time { return now }
		k.Reconcile(context.Background())
		for _, sec := range []int{10, 20, 29} {
			now = time.Unix(1000+int64(sec), 0)
			k.Reconcile(context.Background())
		}
		if h.caddyStarts != 1 {
			t.Fatal("30 second backoff bypassed")
		}
		now = time.Unix(1030, 0)
		k.Reconcile(context.Background())
		if h.caddyStarts != 2 {
			t.Fatal("caddy retry missing")
		}
		// A certificate-manager restart is adopted; no keeper restart is needed.
		h.caddy, h.tlsErr = true, nil
		if !k.Reconcile(context.Background()).NATReady {
			t.Fatal("external recovery not adopted")
		}
	})
	if due(time.Unix(1, 0), time.Unix(100, 0), appBackoff) {
		t.Fatal("backward time jump bypasses backoff")
	}
}

func TestAppDiagnosticGatesAndUncertainIdentity(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*fakeHost, *Keeper)
		code string
	}{
		{"absent", func(h *fakeHost, k *Keeper) { h.app.Installed = false }, "app_not_installed"},
		{"force-stopped", func(h *fakeHost, k *Keeper) { h.app.Stopped = true }, "app_force_stopped"},
		{"debug", func(h *fakeHost, k *Keeper) { k.Config.PackageName += ".debug" }, "debug_autostart_disabled"},
		{"uncertain", func(h *fakeHost, k *Keeper) { h.appErr = errors.New("proc denied") }, "app_observation_failed"},
		{"state-write", func(h *fakeHost, k *Keeper) { k.SaveAttempts = func(Attempts) error { return errors.New("disk full") } }, "attempt_state_write_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := healthyHost()
			h.app.Running, h.upErr = false, probeError("connect")
			k := testKeeper(h)
			tc.edit(h, k)
			s := k.Reconcile(context.Background())
			if h.appStarts != 0 || !strings.Contains(s.LastError, tc.code) {
				t.Fatalf("incorrect app gate: %+v", s)
			}
		})
	}
	h := healthyHost()
	h.caddy, h.tlsErr, h.caddyErr = false, probeError("connect"), errors.New("cannot establish absence")
	testKeeper(h).Reconcile(context.Background())
	if h.caddyStarts != 0 {
		t.Fatal("must not launch when identity scan is uncertain")
	}
}

func TestLocalReadyIsNotNATOrExternalReady(t *testing.T) {
	h := healthyHost()
	h.natErr = errors.New("one rule failed")
	s := testKeeper(h).Reconcile(context.Background())
	if !s.LocalReady || s.NATReady || !strings.Contains(s.LastError, "nat_rule_verification_failed") {
		t.Fatalf("local/NAT readiness conflated: %+v", s)
	}
	h.boot = false
	s = testKeeper(h).Reconcile(context.Background())
	if !s.HotspotReady || !s.UpstreamReady || !s.HTTPSReady || !s.CaddyRunning || s.LocalReady || s.NATReady {
		t.Fatal("boot failure discarded independent observations")
	}
}

func TestObservationErrorsCannotClaimReadiness(t *testing.T) {
	h := healthyHost()
	h.bootErr, h.hotErr = errors.New("bad boot read"), errors.New("bad ip read")
	s := testKeeper(h).Reconcile(context.Background())
	if s.BootReady || s.HotspotReady || s.LocalReady || s.NATReady || !s.UpstreamReady {
		t.Fatalf("invalid readiness: %+v", s)
	}
}