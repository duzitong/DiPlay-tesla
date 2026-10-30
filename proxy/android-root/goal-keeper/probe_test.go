package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func certificate(t *testing.T, domain string, expired bool) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: domain}, DNSNames: []string{domain}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true}
	if expired {
		template.NotBefore, template.NotAfter = now.Add(-2*time.Hour), now.Add(-time.Hour)
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(parsed)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, roots
}

func serverPort(t *testing.T, address string) int {
	t.Helper()
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestTLSRealGETForcedLocalDialAndCertificateValidation(t *testing.T) {
	for _, tc := range []struct {
		name, certDomain, want string
		expired, untrusted      bool
		status                 int
	}{
		{"valid", "no-dns-required.invalid", "", false, false, 200},
		{"wrong-host", "wrong.invalid", "tls_certificate", false, false, 200},
		{"expired", "no-dns-required.invalid", "tls_certificate", true, false, 200},
		{"unknown-root", "no-dns-required.invalid", "tls_certificate", false, true, 200},
		{"http-error", "no-dns-required.invalid", "http_status", false, false, 503},
		{"redirect", "no-dns-required.invalid", "http_status", false, false, 302},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cert, roots := certificate(t, tc.certDomain, tc.expired)
			seen := false
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = true
				if r.Method != "GET" || r.URL.Path != "/" || r.TLS.ServerName != "no-dns-required.invalid" {
					t.Error("wrong GET/SNI")
				}
				w.Header().Set("Location", "https://should-not-follow.invalid/token")
				w.WriteHeader(tc.status)
				io.WriteString(w, "secret body must not enter errors")
			}))
			server.Config.ErrorLog = log.New(io.Discard, "", 0)
			server.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
			server.StartTLS()
			defer server.Close()
			cfg := sampleConfig()
			cfg.Domain, cfg.GatewayIP, cfg.HTTPSPort = "no-dns-required.invalid", "127.0.0.1", serverPort(t, server.Listener.Addr().String())
			if tc.untrusted {
				roots = x509.NewCertPool()
			}
			p := newProber(cfg, roots)
			err := p.https(context.Background())
			if tc.want == "" {
				if err != nil || !seen {
					t.Fatalf("forced local TLS failed: %v", err)
				}
			} else if err == nil || probeClass(err) != tc.want {
				t.Fatalf("error class = %v, want %s", err, tc.want)
			}
		})
	}
}

func TestUpstreamGETStatusAndShortTimeout(t *testing.T) {
	status := 204
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/" {
			t.Error("not a real root GET")
		}
		w.WriteHeader(status)
	}))
	defer server.Close()
	cfg := sampleConfig()
	cfg.UpstreamPort = serverPort(t, server.Listener.Addr().String())
	p := newProber(cfg, nil)
	if err := p.upstream(context.Background()); err != nil {
		t.Fatal(err)
	}
	status = 500
	if err := p.upstream(context.Background()); err != probeError("http_status") {
		t.Fatalf("bad upstream classified as healthy: %v", err)
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if err := p.upstream(ctx); err != probeError("timeout") {
		t.Fatalf("timeout not classified: %v", err)
	}
	if p.plain.Timeout > 3*time.Second || p.secure.Timeout > 3*time.Second {
		t.Fatal("probe deadline too long")
	}
}

type unreadBody struct{ closed bool }

func (*unreadBody) Read([]byte) (int, error) { panic("probe must not read response body") }
func (b *unreadBody) Close() error          { b.closed = true; return nil }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestProbeNeverReadsHTMLOrExposesBody(t *testing.T) {
	for _, status := range []int{200, 500} {
		body := &unreadBody{}
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Body: body, Header: make(http.Header)}, nil
		})}
		err := probeGET(context.Background(), client, "http://127.0.0.1/")
		if !body.closed || (status == 200 && err != nil) || (status == 500 && err != probeError("http_status")) {
			t.Fatal("unexpected body handling")
		}
	}
}