package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const androidCertDirs = "/apex/com.android.conscrypt/cacerts:/system/etc/security/cacerts"

type prober struct {
	plain, secure *http.Client
	upstreamURL   string
	httpsURL      string
}

func newProber(c Config, roots *x509.CertPool) *prober {
	dialer := &net.Dialer{Timeout: 2 * time.Second}
	transport := func() *http.Transport {
		return &http.Transport{
			Proxy: nil, DialContext: dialer.DialContext,
			DisableKeepAlives: true, TLSHandshakeTimeout: 2 * time.Second,
			ResponseHeaderTimeout: 2 * time.Second, MaxResponseHeaderBytes: 32 * 1024,
		}
	}
	client := func(t *http.Transport) *http.Client {
		return &http.Client{Transport: t, Timeout: 3 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	secure := transport()
	// The URL and ServerName remain the domain; only the TCP destination changes.
	// This verifies hostname, validity and roots without DNS or public hairpin NAT.
	secure.TLSClientConfig = &tls.Config{ServerName: c.Domain, RootCAs: roots, MinVersion: tls.VersionTLS12}
	secure.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, "tcp", net.JoinHostPort(c.GatewayIP, strconv.Itoa(c.HTTPSPort)))
	}
	return &prober{
		plain: client(transport()), secure: client(secure),
		upstreamURL: "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(c.UpstreamPort)) + "/",
		httpsURL:    "https://" + net.JoinHostPort(c.Domain, strconv.Itoa(c.HTTPSPort)) + "/",
	}
}

func (p *prober) upstream(ctx context.Context) error { return probeGET(ctx, p.plain, p.upstreamURL) }
func (p *prober) https(ctx context.Context) error    { return probeGET(ctx, p.secure, p.httpsURL) }

func probeGET(ctx context.Context, client *http.Client, address string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return probeError("connect")
	}
	resp, err := client.Do(req)
	if err != nil {
		var cert *tls.CertificateVerificationError
		var unknown x509.UnknownAuthorityError
		var hostname x509.HostnameError
		var invalid x509.CertificateInvalidError
		if errors.As(err, &cert) || errors.As(err, &unknown) || errors.As(err, &hostname) || errors.As(err, &invalid) {
			return probeError("tls_certificate")
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return probeError("timeout")
		}
		var record tls.RecordHeaderError
		var alert tls.AlertError
		if errors.As(err, &record) || errors.As(err, &alert) {
			return probeError("tls")
		}
		// Do not expose url.Error (it contains URL/query data).
		var u *url.Error
		if errors.As(err, &u) && errors.Is(u.Err, context.DeadlineExceeded) {
			return probeError("timeout")
		}
		return probeError("connect")
	}
	defer resp.Body.Close()
	// Deliberately never read the body; even a successful response may contain tokens.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return probeError("http_status")
	}
	return nil
}