package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/acme"
)

const testTerms = "https://letsencrypt.org/test-terms"

var testPassword = base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{27}, 32))

type fixture struct {
	m     *manager
	key   *ecdsa.PrivateKey
	caKey *ecdsa.PrivateKey
	ca    *x509.Certificate
	old   []byte
	good  []byte
}

func testKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	return k
}
func writeTest(t *testing.T, path string, b []byte) {
	t.Helper()
	if e := atomicWrite(path, b); e != nil {
		t.Fatal(e)
	}
}
func leafPEM(t *testing.T, key *ecdsa.PrivateKey, parent *x509.Certificate, caKey *ecdsa.PrivateKey, domain string, before, after time.Time) []byte {
	t.Helper()
	serial, e := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if e != nil {
		t.Fatal(e)
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: domain}, DNSNames: []string{domain}, NotBefore: before, NotAfter: after, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if parent == nil {
		parent = template
		caKey = key
	}
	der, e := x509.CreateCertificate(rand.Reader, template, parent, &key.PublicKey, caKey)
	if e != nil {
		t.Fatal(e)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	base := t.TempDir()
	state := filepath.Join(base, "state")
	if e := os.Mkdir(state, 0700); e != nil {
		t.Fatal(e)
	}
	c := config{Domain: "car.example.test", Listen: "192.168.10.2:8443", Origin: "https://car.example.test:8443", Directory: state, Certificate: filepath.Join(base, "fullchain.pem"), PrivateKey: filepath.Join(base, "key.pem"), ReloadScript: filepath.Join(base, "reload.sh"), GatewayAddress: "127.0.0.1:443", CADirectory: production}
	f := &fixture{key: testKey(t), caKey: testKey(t)}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ephemeral test CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(365 * 24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	der, e := x509.CreateCertificate(rand.Reader, template, template, &f.caKey.PublicKey, f.caKey)
	if e != nil {
		t.Fatal(e)
	}
	f.ca, e = x509.ParseCertificate(der)
	if e != nil {
		t.Fatal(e)
	}
	f.old = leafPEM(t, f.key, f.ca, f.caKey, c.Domain, now.Add(-time.Hour), now.Add(5*24*time.Hour))
	f.good = leafPEM(t, f.key, f.ca, f.caKey, c.Domain, now.Add(-time.Hour), now.Add(90*24*time.Hour))
	keyDER, e := x509.MarshalPKCS8PrivateKey(f.key)
	if e != nil {
		t.Fatal(e)
	}
	writeTest(t, c.PrivateKey, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	writeTest(t, c.Certificate, f.old)
	writeTest(t, filepath.Join(state, "admin-password"), []byte(testPassword))
	f.m, e = newManager(c)
	if e != nil {
		t.Fatal(e)
	}
	f.m.roots = x509.NewCertPool()
	f.m.roots.AddCert(f.ca)
	f.m.reload = func(context.Context) error { return nil }
	f.m.verifyGateway = func(context.Context, *x509.Certificate) error { return nil }
	return f
}

type fakeCA struct {
	order                                                    *acme.Order
	auth                                                     *acme.Authorization
	terms                                                    string
	chain                                                    [][]byte
	registered                                               bool
	newOrders, registers, accepts, finalizes, fetches, waits int
	failAuth, failFetch, failFinalize                        bool
	beforeAuth                                               func()
	csrPublic                                                []byte
	csrDomain                                                string
}

func makeFake(m *manager) *fakeCA {
	base := strings.TrimSuffix(m.c.CADirectory, "/directory")
	f := &fakeCA{terms: testTerms, registered: true}
	f.csrDomain = m.c.Domain
	if leaf, e := m.currentLeaf(); e == nil {
		f.csrPublic = leaf.RawSubjectPublicKeyInfo
	}
	f.order = &acme.Order{URI: base + "/order/1", Status: acme.StatusPending, Expires: time.Now().Add(24 * time.Hour), Identifiers: []acme.AuthzID{{Type: "dns", Value: m.c.Domain}}, AuthzURLs: []string{base + "/authz/1"}, FinalizeURL: base + "/finalize/1", CertURL: base + "/cert/1"}
	f.auth = &acme.Authorization{URI: base + "/authz/1", Status: acme.StatusPending, Identifier: acme.AuthzID{Type: "dns", Value: m.c.Domain}, Challenges: []*acme.Challenge{{URI: base + "/challenge/1", Type: "dns-01", Status: acme.StatusPending, Token: "token"}}}
	m.newCA = func(crypto.Signer, string) authority { return f }
	return f
}
func (f *fakeCA) Discover(context.Context) (acme.Directory, error) {
	return acme.Directory{Terms: f.terms}, nil
}
func (f *fakeCA) GetReg(context.Context, string) (*acme.Account, error) {
	if !f.registered {
		return nil, acme.ErrNoAccount
	}
	return &acme.Account{URI: strings.Split(f.order.URI, "/order/")[0] + "/acct/1"}, nil
}
func (f *fakeCA) Register(_ context.Context, _ *acme.Account, prompt func(string) bool) (*acme.Account, error) {
	f.registers++
	if !prompt(f.terms) {
		return nil, errors.New("terms refused")
	}
	f.registered = true
	return f.GetReg(context.Background(), "")
}
func (f *fakeCA) AuthorizeOrder(context.Context, []acme.AuthzID, ...acme.OrderOption) (*acme.Order, error) {
	f.newOrders++
	o := *f.order
	return &o, nil
}
func (f *fakeCA) GetOrder(context.Context, string) (*acme.Order, error) {
	o := *f.order
	return &o, nil
}
func (f *fakeCA) GetAuthorization(context.Context, string) (*acme.Authorization, error) {
	if f.beforeAuth != nil {
		f.beforeAuth()
	}
	if f.failAuth {
		return nil, errors.New("untrusted upstream response with secret")
	}
	return f.auth, nil
}
func (f *fakeCA) DNS01ChallengeRecord(string) (string, error) { return "expected-txt", nil }
func (f *fakeCA) Accept(_ context.Context, c *acme.Challenge) (*acme.Challenge, error) {
	f.accepts++
	c.Status = acme.StatusProcessing
	return c, nil
}
func (f *fakeCA) WaitOrder(context.Context, string) (*acme.Order, error) {
	f.waits++
	if f.order.Status == acme.StatusProcessing {
		f.order.Status = acme.StatusValid
	} else {
		f.order.Status = acme.StatusReady
	}
	return f.GetOrder(context.Background(), "")
}
func (f *fakeCA) CreateCertFromOrder(_ context.Context, _ *acme.Order, csr []byte, _ bool) ([][]byte, string, error) {
	f.finalizes++
	c, e := x509.ParseCertificateRequest(csr)
	if e != nil || c.CheckSignature() != nil {
		return nil, "", errors.New("invalid CSR")
	}
	if len(c.DNSNames) != 1 || c.DNSNames[0] != f.csrDomain || !bytes.Equal(c.RawSubjectPublicKeyInfo, f.csrPublic) {
		return nil, "", errors.New("CSR did not reuse original TLS key and domain")
	}
	f.order.Status = acme.StatusValid
	if f.failFinalize {
		return nil, "", errors.New("interrupted")
	}
	return f.chain, f.order.CertURL, nil
}
func (f *fakeCA) FetchCert(context.Context, string, bool) ([][]byte, error) {
	f.fetches++
	if f.failFetch {
		return nil, errors.New("private upstream failure")
	}
	return f.chain, nil
}

func attachOrder(t *testing.T, m *manager, f *fakeCA) {
	t.Helper()
	if _, e := m.accountKey(true); e != nil {
		t.Fatal(e)
	}
	s := m.snapshot()
	s.AccountURL = strings.Split(f.order.URI, "/order/")[0] + "/acct/1"
	if _, e := m.updateOrder(s, f.order); e != nil {
		t.Fatal(e)
	}
}
func setChain(t *testing.T, f *fakeCA, b []byte) {
	t.Helper()
	block, _ := pem.Decode(b)
	if block == nil {
		t.Fatal("bad test cert")
	}
	f.chain = [][]byte{block.Bytes}
}
func preparePending(t *testing.T, f *fixture) *fakeCA {
	t.Helper()
	ca := makeFake(f.m)
	ca.order.Status = acme.StatusValid
	setChain(t, ca, f.good)
	attachOrder(t, f.m, ca)
	writeTest(t, filepath.Join(f.m.c.Directory, "pending-fullchain.pem"), f.good)
	s := f.m.snapshot()
	s.PendingHash = digest(f.good)
	if e := f.m.save(s); e != nil {
		t.Fatal(e)
	}
	f.m.pending = true
	return ca
}

func request(m *manager, method, path, body string, auth bool, mutate func(*http.Request)) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, m.c.Origin+path, strings.NewReader(body))
	if auth {
		r.SetBasicAuth("admin", testPassword)
	}
	if method == "POST" {
		r.Header.Set("Origin", m.c.Origin)
		r.Header.Set("X-Cert-Manager", "1")
		r.Header.Set("Content-Type", "application/json")
	}
	if mutate != nil {
		mutate(r)
	}
	w := httptest.NewRecorder()
	m.ServeHTTP(w, r)
	return w
}

func TestConfiguration(t *testing.T) {
	f := newFixture(t)
	if e := f.m.c.validate(); e != nil {
		t.Fatal(e)
	}
	tests := []struct {
		name string
		edit func(*config)
	}{
		{"wildcard", func(c *config) { c.Domain = "*.example.test" }}, {"uppercase", func(c *config) { c.Domain = "Car.example.test" }}, {"IP domain", func(c *config) { c.Domain = "192.168.1.1" }}, {"empty label", func(c *config) { c.Domain = "car..test" }}, {"bad label", func(c *config) { c.Domain = "-car.example.test" }},
		{"wildcard bind", func(c *config) { c.Listen = "0.0.0.0:8443" }}, {"public bind", func(c *config) { c.Listen = "8.8.8.8:8443" }}, {"localhost name", func(c *config) { c.Listen = "localhost:8443" }}, {"IPv6", func(c *config) { c.Listen = "[fd00::1]:8443" }}, {"zero port", func(c *config) { c.Listen = "192.168.1.2:0" }}, {"overflow port", func(c *config) { c.Listen = "192.168.1.2:65536" }},
		{"wrong scheme", func(c *config) { c.Origin = "http://car.example.test:8443" }}, {"wrong port", func(c *config) { c.Origin = "https://car.example.test:443" }}, {"origin slash", func(c *config) { c.Origin += "/" }}, {"origin credentials", func(c *config) { c.Origin = "https://admin@car.example.test:8443" }},
		{"arbitrary CA", func(c *config) { c.CADirectory = "https://evil.example/directory" }}, {"relative directory", func(c *config) { c.Directory = "state" }}, {"path collision", func(c *config) { c.Certificate = c.PrivateKey }}, {"reserved path", func(c *config) { c.PrivateKey = filepath.Join(c.Directory, "state.json") }}, {"gateway hostname", func(c *config) { c.GatewayAddress = "car.example.test:443" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := f.m.c
			tt.edit(&c)
			if c.validate() == nil {
				t.Fatal("accepted unsafe config")
			}
		})
	}
	c := f.m.c
	c.CADirectory = staging
	if e := c.validate(); e != nil {
		t.Fatal(e)
	}
}

func TestSecurityBoundary(t *testing.T) {
	tests := []struct {
		name, method, path, body string
		auth                     bool
		edit                     func(*http.Request)
		want                     int
	}{
		{"unauthenticated status", "GET", "/api/status", "", false, nil, 401},
		{"unauthenticated terms", "GET", "/api/terms", "", false, nil, 401},
		{"valid status", "GET", "/api/status", "", true, nil, 200},
		{"wrong password", "GET", "/api/status", "", true, func(r *http.Request) { r.SetBasicAuth("admin", "wrong") }, 401},
		{"wrong user", "GET", "/api/status", "", true, func(r *http.Request) { r.SetBasicAuth("root", testPassword) }, 401},
		{"duplicate auth", "GET", "/api/status", "", true, func(r *http.Request) { r.Header.Add("Authorization", r.Header.Get("Authorization")) }, 401},
		{"header budget", "GET", "/api/status", "", true, func(r *http.Request) { r.Header.Set("X-Filler", strings.Repeat("a", 8<<10)) }, 431},
		{"host", "GET", "/api/status", "", true, func(r *http.Request) { r.Host = "evil.test:8443" }, 403},
		{"host no port", "GET", "/api/status", "", true, func(r *http.Request) { r.Host = "car.example.test" }, 403},
		{"origin", "GET", "/api/status", "", true, func(r *http.Request) { r.Header.Set("Origin", "https://evil.test") }, 403},
		{"null origin", "POST", "/api/complete", "{}", true, func(r *http.Request) { r.Header.Set("Origin", "null") }, 403},
		{"duplicate origin", "POST", "/api/complete", "{}", true, func(r *http.Request) { r.Header.Add("Origin", r.Header.Get("Origin")) }, 403},
		{"missing origin", "POST", "/api/complete", "{}", true, func(r *http.Request) { r.Header.Del("Origin") }, 403},
		{"missing csrf", "POST", "/api/complete", "{}", true, func(r *http.Request) { r.Header.Del("X-Cert-Manager") }, 403},
		{"duplicate csrf", "POST", "/api/complete", "{}", true, func(r *http.Request) { r.Header.Add("X-Cert-Manager", "1") }, 403},
		{"wrong media", "POST", "/api/complete", "{}", true, func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, 415},
		{"duplicate media", "POST", "/api/complete", "{}", true, func(r *http.Request) { r.Header.Add("Content-Type", "application/json") }, 415},
		{"GET mutation", "GET", "/api/complete", "", true, nil, 405},
		{"POST status", "POST", "/api/status", "{}", true, nil, 405},
		{"preflight", "OPTIONS", "/api/complete", "", true, nil, 405},
		{"empty", "POST", "/api/complete", "", true, nil, 400},
		{"null", "POST", "/api/complete", "null", true, nil, 400},
		{"array", "POST", "/api/complete", "[]", true, nil, 400},
		{"extra field", "POST", "/api/complete", `{"command":"bad"}`, true, nil, 400},
		{"two objects", "POST", "/api/complete", "{} {}", true, nil, 400},
		{"oversized", "POST", "/api/complete", strings.Repeat(" ", bodyLimit+1), true, nil, 413},
		{"chunked oversized", "POST", "/api/complete", strings.Repeat(" ", bodyLimit+1), true, func(r *http.Request) { r.ContentLength = -1 }, 413},
		{"install confirmation", "POST", "/api/install", "{}", true, nil, 400},
		{"unfetched terms", "POST", "/api/prepare", `{"termsURL":"https://letsencrypt.org/test-terms","acceptTerms":true}`, true, nil, 400},
		{"unknown endpoint", "GET", "/api/command", "", true, nil, 404},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			w := request(f.m, tt.method, tt.path, tt.body, tt.auth, tt.edit)
			if w.Code != tt.want {
				t.Fatalf("got %d %s, want %d", w.Code, w.Body.String(), tt.want)
			}
			if w.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatal("unexpected CORS")
			}
			if strings.Contains(w.Body.String(), testPassword) {
				t.Fatal("password leaked")
			}
		})
	}
	f := newFixture(t)
	w := request(f.m, "GET", "/", "", false, nil)
	if w.Code == 401 || w.Header().Get("WWW-Authenticate") != "" {
		t.Fatal("root must be public")
	}
	csp := w.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "frame-ancestors 'none'") || strings.Contains(csp, "unsafe-inline") || !strings.Contains(csp, "script-src 'self'") {
		t.Fatal(csp)
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatal("missing security headers")
	}
	if w = request(f.m, "GET", "/../state.json", "", false, nil); w.Code != 404 {
		t.Fatal("static traversal exposed")
	}
}

func TestGlobalThrottle(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 10; i++ {
		w := request(f.m, "GET", "/api/status", "", false, func(r *http.Request) { r.RemoteAddr = string(rune('a'+i)) + ":123" })
		if w.Code != 401 {
			t.Fatalf("attempt %d: %d", i, w.Code)
		}
	}
	w := request(f.m, "GET", "/api/status", "", true, nil)
	if w.Code != 200 || w.Header().Get("Retry-After") != "" {
		t.Fatal("invalid failures locked out correct credentials", w.Code)
	}
	for _, edit := range []func(*http.Request){
		nil,
		func(r *http.Request) { r.SetBasicAuth("admin", "wrong") },
		func(r *http.Request) { r.SetBasicAuth("root", testPassword) },
		func(r *http.Request) { r.Header.Add("Authorization", r.Header.Get("Authorization")) },
	} {
		w = request(f.m, "GET", "/api/status", "", true, func(r *http.Request) {
			if edit == nil {
				r.Header.Del("Authorization")
			} else {
				edit(r)
			}
		})
		if w.Code != 429 || w.Header().Get("Retry-After") != "60" {
			t.Fatal("invalid credentials bypassed exhausted global budget", w.Code)
		}
	}
	f.m.authWindow = time.Now().Add(-time.Minute)
	if w = request(f.m, "GET", "/api/status", "", false, nil); w.Code != 401 {
		t.Fatal("window failed to reset")
	}
	if w = request(f.m, "GET", "/api/status", "", true, nil); w.Code != 200 || f.m.authFailures != 1 {
		t.Fatal("valid login changed the invalid-attempt budget")
	}
}

func TestAtomicFilesAndPasswordInitialization(t *testing.T) {
	f := newFixture(t)
	p := filepath.Join(f.m.c.Directory, "atomic")
	writeTest(t, p, []byte("old"))
	writeTest(t, p, []byte("new"))
	b, e := readFile(p, true)
	if e != nil || string(b) != "new" {
		t.Fatal("atomic replacement failed", e)
	}
	if runtime.GOOS != "windows" {
		s, e := os.Stat(p)
		if e != nil || s.Mode().Perm() != 0600 {
			t.Fatal("wrong mode", e)
		}
	}
	entries, e := os.ReadDir(f.m.c.Directory)
	if e != nil {
		t.Fatal(e)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".cert-manager-") {
			t.Fatal("temp file leaked")
		}
	}
	d := filepath.Join(f.m.c.Directory, "not-file")
	if e := os.Mkdir(d, 0700); e != nil {
		t.Fatal(e)
	}
	if atomicWrite(d, []byte("bad")) == nil {
		t.Fatal("replaced directory")
	}
	c := f.m.c
	c.Directory = filepath.Join(t.TempDir(), "private")
	// Capture output: the only permitted stdout is the path, never password.
	r, w, e := os.Pipe()
	if e != nil {
		t.Fatal(e)
	}
	old := os.Stdout
	os.Stdout = w
	e = initPassword(c)
	os.Stdout = old
	w.Close()
	out, _ := io.ReadAll(r)
	r.Close()
	if e != nil {
		t.Fatal(e)
	}
	passwordPath := filepath.Join(c.Directory, "admin-password")
	pw, e := readFile(passwordPath, true)
	if e != nil {
		t.Fatal(e)
	}
	raw, e := base64.RawURLEncoding.DecodeString(string(pw))
	if e != nil || len(raw) != 32 {
		t.Fatal("password entropy/encoding")
	}
	if string(out) != passwordPath+"\n" || bytes.Contains(out, pw) {
		t.Fatal("password output was not path-only")
	}
	if initPassword(c) == nil {
		t.Fatal("overwrote password")
	}
	again, _ := os.ReadFile(passwordPath)
	if !bytes.Equal(again, pw) {
		t.Fatal("password changed")
	}
	entries, _ = os.ReadDir(c.Directory)
	if len(entries) != 1 {
		t.Fatal("initialization created account/order")
	}
}

func TestCertificateValidationAndDynamicReload(t *testing.T) {
	f := newFixture(t)
	now := time.Now()
	for _, tt := range []struct {
		name  string
		b     []byte
		roots *x509.CertPool
	}{
		{"good", f.good, f.m.roots},
		{"hostname", leafPEM(t, f.key, f.ca, f.caKey, "wrong.example.test", now.Add(-time.Hour), now.Add(90*24*time.Hour)), f.m.roots},
		{"key", leafPEM(t, testKey(t), f.ca, f.caKey, f.m.c.Domain, now.Add(-time.Hour), now.Add(90*24*time.Hour)), f.m.roots},
		{"expired", leafPEM(t, f.key, f.ca, f.caKey, f.m.c.Domain, now.Add(-48*time.Hour), now.Add(-time.Hour)), f.m.roots},
		{"future", leafPEM(t, f.key, f.ca, f.caKey, f.m.c.Domain, now.Add(time.Hour), now.Add(90*24*time.Hour)), f.m.roots},
		{"untrusted", f.good, x509.NewCertPool()},
		{"older", leafPEM(t, f.key, f.ca, f.caKey, f.m.c.Domain, now.Add(-time.Hour), now.Add(2*24*time.Hour)), f.m.roots},
		{"same", f.old, f.m.roots},
		{"malformed", []byte("not a certificate"), f.m.roots},
	} {
		t.Run(tt.name, func(t *testing.T) {
			roots := f.m.roots
			f.m.roots = tt.roots
			defer func() { f.m.roots = roots }()
			_, e := f.m.validateCertificate(tt.b, true)
			if (e == nil) != (tt.name == "good") {
				t.Fatalf("unexpected result %v", e)
			}
		})
	}
	a, e := f.m.getCertificate(nil)
	if e != nil {
		t.Fatal(e)
	}
	writeTest(t, f.m.c.Certificate, f.good)
	b, e := f.m.getCertificate(nil)
	if e != nil || bytes.Equal(a.Certificate[0], b.Certificate[0]) {
		t.Fatal("TLS callback did not reread current file")
	}
	expired := leafPEM(t, f.key, f.ca, f.caKey, f.m.c.Domain, now.Add(-48*time.Hour), now.Add(-time.Hour))
	writeTest(t, f.m.c.Certificate, expired)
	if _, e := f.m.getCertificate(nil); e != nil {
		t.Fatal("TLS loading must permit expiry", e)
	}
}

func TestDNSNotPropagatedNeverAccepts(t *testing.T) {
	for _, missing := range []string{"1.1.1.1:53", "8.8.8.8:53"} {
		t.Run(missing, func(t *testing.T) {
			f := newFixture(t)
			ca := makeFake(f.m)
			setChain(t, ca, f.good)
			attachOrder(t, f.m, ca)
			f.m.lookupTXT = func(_ context.Context, resolver, name string) ([]string, error) {
				if name != "_acme-challenge."+f.m.c.Domain {
					t.Fatal(name)
				}
				if resolver == missing {
					return []string{"wrong"}, nil
				}
				return []string{"expected-txt"}, nil
			}
			e := f.m.complete(context.Background())
			if e == nil || !strings.Contains(e.Error(), missing) {
				t.Fatal("missing propagation error", e)
			}
			if ca.accepts != 0 || ca.finalizes != 0 {
				t.Fatal("submitted to CA before BOTH resolvers saw TXT")
			}
			f.m.lookupTXT = func(context.Context, string, string) ([]string, error) { return []string{"expected-txt"}, nil }
			if e = f.m.complete(context.Background()); e != nil {
				t.Fatal("retry failed", e)
			}
			if ca.accepts != 1 || ca.finalizes != 1 || !f.m.hasPending() {
				t.Fatal("retry did not complete issuance")
			}
			current, _ := os.ReadFile(f.m.c.Certificate)
			if !bytes.Equal(current, f.old) {
				t.Fatal("complete changed live gateway")
			}
		})
	}
}

func TestRecoverEachOrderState(t *testing.T) {
	for _, status := range []string{acme.StatusPending, acme.StatusProcessing, acme.StatusReady, acme.StatusValid} {
		t.Run(status, func(t *testing.T) {
			f := newFixture(t)
			ca := makeFake(f.m)
			ca.order.Status = status
			setChain(t, ca, f.good)
			// A pending authorization with a processing challenge MUST NOT be accepted again.
			ca.auth.Challenges[0].Status = acme.StatusProcessing
			attachOrder(t, f.m, ca)
			restarted, e := newManager(f.m.c)
			if e != nil {
				t.Fatal(e)
			}
			restarted.roots = f.m.roots
			restarted.newCA = func(crypto.Signer, string) authority { return ca }
			restarted.lookupTXT = func(context.Context, string, string) ([]string, error) {
				t.Fatal("processing/ready/valid state queried TXT")
				return nil, nil
			}
			keyBefore, _ := os.ReadFile(f.m.c.PrivateKey)
			if e = restarted.complete(context.Background()); e != nil {
				t.Fatal(e)
			}
			if ca.accepts != 0 {
				t.Fatal("reaccepted processing challenge")
			}
			if status == acme.StatusValid || status == acme.StatusProcessing {
				if ca.finalizes != 0 || ca.fetches != 1 {
					t.Fatal("valid recovery re-finalized", ca.finalizes, ca.fetches)
				}
			}
			if !restarted.hasPending() || restarted.snapshot().Order.Status != acme.StatusValid {
				t.Fatal("pending state not committed")
			}
			keyAfter, _ := os.ReadFile(restarted.c.PrivateKey)
			if !bytes.Equal(keyBefore, keyAfter) {
				t.Fatal("TLS key changed")
			}
			again, e := newManager(restarted.c)
			if e != nil || !again.hasPending() {
				t.Fatal("pending restart failed", e)
			}
		})
	}
}

func TestFinalizationAndDownloadInterruption(t *testing.T) {
	f := newFixture(t)
	ca := makeFake(f.m)
	ca.order.Status = acme.StatusReady
	ca.failFinalize = true
	setChain(t, ca, f.good)
	attachOrder(t, f.m, ca)
	if f.m.complete(context.Background()) == nil {
		t.Fatal("expected interrupted finalization")
	}
	if ca.finalizes != 1 || f.m.hasPending() {
		t.Fatal("bad interrupted state")
	}
	ca.failFetch = true
	if f.m.complete(context.Background()) == nil {
		t.Fatal("expected download error")
	}
	if ca.finalizes != 1 {
		t.Fatal("valid order finalized again")
	}
	ca.failFetch = false
	if e := f.m.complete(context.Background()); e != nil {
		t.Fatal(e)
	}
	if ca.finalizes != 1 || !f.m.hasPending() {
		t.Fatal("failed download recovery")
	}
}

func TestPrepareTermsPersistenceAndNoDuplicateOrder(t *testing.T) {
	f := newFixture(t)
	ca := makeFake(f.m)
	ca.registered = false
	if f.m.prepare(context.Background(), "https://evil.test/terms") == nil {
		t.Fatal("accepted arbitrary terms")
	}
	if _, e := os.Stat(filepath.Join(f.m.c.Directory, "account-key.pem")); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("generated account before terms accepted")
	}
	w := request(f.m, "GET", "/api/terms", "", true, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), testTerms) {
		t.Fatal(w.Body.String())
	}
	// CA state must already be on disk before the first authorization fetch.
	ca.beforeAuth = func() {
		b, e := readFile(filepath.Join(f.m.c.Directory, "state.json"), true)
		if e != nil || !bytes.Contains(b, []byte(ca.order.URI)) {
			t.Fatal("authorization fetched before order persisted")
		}
	}
	if e := f.m.prepare(context.Background(), testTerms); e != nil {
		t.Fatal(e)
	}
	if ca.registers != 1 || ca.newOrders != 1 {
		t.Fatal("account/order creation counts")
	}
	keyBefore, _ := os.ReadFile(filepath.Join(f.m.c.Directory, "account-key.pem"))
	restarted, e := newManager(f.m.c)
	if e != nil {
		t.Fatal(e)
	}
	restarted.roots = f.m.roots
	restarted.newCA = func(crypto.Signer, string) authority { return ca }
	if e = restarted.prepare(context.Background(), testTerms); e != nil {
		t.Fatal(e)
	}
	if ca.registers != 1 || ca.newOrders != 1 {
		t.Fatal("active order was replaced after restart")
	}
	keyAfter, _ := os.ReadFile(filepath.Join(f.m.c.Directory, "account-key.pem"))
	if !bytes.Equal(keyBefore, keyAfter) {
		t.Fatal("account key changed")
	}
	status := request(restarted, "GET", "/api/status", "", true, nil)
	for _, secret := range []string{"accountURL", "authzURLs", "finalizeURL", "certURL", ca.order.URI, testPassword, "PRIVATE KEY"} {
		if strings.Contains(status.Body.String(), secret) {
			t.Fatalf("status exposed %s", secret)
		}
	}
	var result map[string]any
	if json.Unmarshal(status.Body.Bytes(), &result) != nil || result["order"] == nil {
		t.Fatal("bad status shape")
	}
	// Invalid and expired orders can be replaced, but never when pending exists.
	ca.order.Status = acme.StatusInvalid
	if e = restarted.prepare(context.Background(), testTerms); e != nil {
		t.Fatal(e)
	}
	if ca.newOrders != 2 {
		t.Fatal("invalid order not replaced")
	}
	ca.order.Status = acme.StatusPending
	ca.order.Expires = time.Now().Add(-time.Minute)
	s := restarted.snapshot()
	s.Order.Status = acme.StatusPending
	s.Order.Expires = ca.order.Expires
	if e = restarted.save(s); e != nil {
		t.Fatal(e)
	}
	if e = restarted.prepare(context.Background(), testTerms); e != nil {
		t.Fatal(e)
	}
	if ca.newOrders != 3 {
		t.Fatal("expired order not replaced")
	}
}

func TestTermsRouteAndExplicitAcceptance(t *testing.T) {
	f := newFixture(t)
	ca := makeFake(f.m)
	w := request(f.m, "GET", "/api/terms", "", true, nil)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	for _, body := range []string{`{"termsURL":"https://evil.test","acceptTerms":true}`, `{"termsURL":"https://letsencrypt.org/test-terms","acceptTerms":false}`} {
		if w = request(f.m, "POST", "/api/prepare", body, true, nil); w.Code != 400 {
			t.Fatal("accepted wrong terms", w.Code)
		}
	}
	f.m.termsAt = time.Now().Add(-16 * time.Minute)
	if w = request(f.m, "POST", "/api/prepare", `{"termsURL":"https://letsencrypt.org/test-terms","acceptTerms":true}`, true, nil); w.Code != 400 {
		t.Fatal("stale terms accepted")
	}
	f.m.termsAt = time.Now()
	ca.terms = "https://letsencrypt.org/new-terms"
	if e := f.m.prepare(context.Background(), testTerms); e == nil {
		t.Fatal("did not refresh terms at prepare")
	}
	if f.m.terms != "" || ca.newOrders != 0 || ca.registers != 0 {
		t.Fatal("changed terms mutated ACME state")
	}
}

func TestRenewalRateProtection(t *testing.T) {
	f := newFixture(t)
	ca := makeFake(f.m)
	writeTest(t, f.m.c.Certificate, f.good)
	if !f.m.recentlyIssued() {
		t.Fatal("trusted long-lived cert should block")
	}
	if f.m.prepare(context.Background(), testTerms) == nil || ca.newOrders != 0 {
		t.Fatal("early renewal allowed")
	}
	f.m.terms = testTerms
	f.m.termsAt = time.Now()
	w := request(f.m, "POST", "/api/prepare", `{"termsURL":"https://letsencrypt.org/test-terms","acceptTerms":true}`, true, nil)
	if w.Code != 409 {
		t.Fatal(w.Code)
	}
	self := leafPEM(t, f.key, nil, nil, f.m.c.Domain, time.Now().Add(-time.Hour), time.Now().Add(90*24*time.Hour))
	writeTest(t, f.m.c.Certificate, self)
	if f.m.recentlyIssued() {
		t.Fatal("self-signed bootstrap must permit preparation")
	}
	s := f.m.snapshot()
	s.IssuedNotAfter = time.Now().Add(80 * 24 * time.Hour)
	if e := f.m.save(s); e != nil {
		t.Fatal(e)
	}
	if !f.m.recentlyIssued() {
		t.Fatal("issued-but-not-installed certificate bypassed rate protection")
	}
}

func TestInstallRollbackAndRetry(t *testing.T) {
	for _, failure := range []string{"reload", "verify", "rollback"} {
		t.Run(failure, func(t *testing.T) {
			f := newFixture(t)
			preparePending(t, f)
			calls := 0
			f.m.reload = func(context.Context) error {
				calls++
				if failure == "rollback" || failure == "reload" && calls == 1 {
					return errors.New("script output containing private material")
				}
				return nil
			}
			f.m.verifyGateway = func(context.Context, *x509.Certificate) error {
				if failure == "verify" {
					return errors.New("peer leaf differs")
				}
				return nil
			}
			e := f.m.install(context.Background())
			if e == nil {
				t.Fatal("failed install returned success")
			}
			if calls != 2 {
				t.Fatalf("reload calls %d; rollback not attempted", calls)
			}
			if strings.Contains(e.Error(), "private material") {
				t.Fatal("script output leaked")
			}
			if (failure == "rollback") != strings.Contains(e.Error(), "ROLLBACK FAILED") {
				t.Fatal("rollback error swallowed", e)
			}
			current, _ := os.ReadFile(f.m.c.Certificate)
			if !bytes.Equal(current, f.old) || !f.m.hasPending() {
				t.Fatal("old certificate not restored or pending lost")
			}
			backup, _ := readFile(filepath.Join(f.m.c.Directory, "previous-fullchain.pem"), true)
			if !bytes.Equal(backup, f.old) {
				t.Fatal("persistent backup not retained")
			}
			f.m.reload = func(context.Context) error { return nil }
			f.m.verifyGateway = func(_ context.Context, l *x509.Certificate) error {
				block, _ := pem.Decode(f.good)
				if !bytes.Equal(l.Raw, block.Bytes) {
					return errors.New("wrong leaf passed to verifier")
				}
				return nil
			}
			if failure == "rollback" {
				if e = f.m.install(context.Background()); e == nil || !strings.Contains(e.Error(), "interrupted install rolled back") {
					t.Fatal("failed rollback not recovered before retry", e)
				}
			}
			if e = f.m.install(context.Background()); e != nil {
				t.Fatal("retry", e)
			}
			current, _ = os.ReadFile(f.m.c.Certificate)
			if !bytes.Equal(current, f.good) || f.m.hasPending() || f.m.snapshot().Order.Status != "installed" {
				t.Fatal("successful install not committed")
			}
			if _, e = os.Stat(filepath.Join(f.m.c.Directory, "pending-fullchain.pem")); !errors.Is(e, os.ErrNotExist) {
				t.Fatal("pending file not removed")
			}
		})
	}
}

func TestInterruptedInstallRecovery(t *testing.T) {
	f := newFixture(t)
	preparePending(t, f)
	writeTest(t, filepath.Join(f.m.c.Directory, "previous-fullchain.pem"), f.old)
	s := f.m.snapshot()
	s.Installing = true
	if e := f.m.save(s); e != nil {
		t.Fatal(e)
	}
	writeTest(t, f.m.c.Certificate, f.good)
	restarted, e := newManager(f.m.c)
	if e != nil {
		t.Fatal(e)
	}
	restarted.reload = func(context.Context) error { return nil }
	if e = restarted.install(context.Background()); e == nil || !strings.Contains(e.Error(), "rolled back") {
		t.Fatal(e)
	}
	current, _ := os.ReadFile(f.m.c.Certificate)
	if !bytes.Equal(current, f.old) || restarted.snapshot().Installing || !restarted.hasPending() {
		t.Fatal("crash recovery lost prior certificate/pending")
	}
}

func TestStagingAndTamperedPendingNeverInstall(t *testing.T) {
	f := newFixture(t)
	preparePending(t, f)
	calls := 0
	f.m.reload = func(context.Context) error { calls++; return nil }
	f.m.c.CADirectory = staging
	if f.m.install(context.Background()) == nil || calls != 0 {
		t.Fatal("staging installed")
	}
	f.m.c.CADirectory = production
	writeTest(t, filepath.Join(f.m.c.Directory, "pending-fullchain.pem"), f.old)
	if f.m.install(context.Background()) == nil || calls != 0 {
		t.Fatal("tampered pending installed")
	}
	current, _ := os.ReadFile(f.m.c.Certificate)
	if !bytes.Equal(current, f.old) {
		t.Fatal("gateway modified")
	}
}

func TestRejectCorruptStateAndKeys(t *testing.T) {
	for _, content := range []string{"{", "null", `{}`, `{"version":1,"domain":"other","caDirectory":"bad"}`, `{"version":1,"unknown":true}`} {
		t.Run(content, func(t *testing.T) {
			f := newFixture(t)
			p := filepath.Join(f.m.c.Directory, "state.json")
			writeTest(t, p, []byte(content))
			if _, e := newManager(f.m.c); e == nil {
				t.Fatal("corruption ignored")
			}
			b, _ := os.ReadFile(p)
			if string(b) != content {
				t.Fatal("corrupt state overwritten")
			}
		})
	}
	f := newFixture(t)
	ca := makeFake(f.m)
	attachOrder(t, f.m, ca)
	p := filepath.Join(f.m.c.Directory, "account-key.pem")
	writeTest(t, p, []byte("secret corrupt key"))
	if _, e := newManager(f.m.c); e == nil || strings.Contains(e.Error(), "secret corrupt key") {
		t.Fatal("key corruption ignored/leaked", e)
	}
	if _, e := f.m.accountKey(true); e == nil {
		t.Fatal("corrupt key regenerated")
	}
	writeTest(t, p, []byte("unchanged"))
	s := f.m.snapshot()
	s.Order.URI = "http://127.0.0.1/private"
	if f.m.save(s) == nil {
		t.Fatal("unsafe stored ACME URL accepted")
	}
}

func TestNoAccountOrOrderOnStartup(t *testing.T) {
	f := newFixture(t)
	if _, e := f.m.getCertificate(nil); e != nil {
		t.Fatal(e)
	}
	if _, e := newManager(f.m.c); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"account-key.pem", "state.json", "pending-fullchain.pem"} {
		if _, e := os.Stat(filepath.Join(f.m.c.Directory, name)); !errors.Is(e, os.ErrNotExist) {
			t.Fatal("startup generated private ACME state", name)
		}
	}
}

func TestConcurrentWritesStatusAndDetachedContext(t *testing.T) {
	f := newFixture(t)
	release := make(chan struct{})
	entered := make(chan struct{})
	finished := make(chan struct{})
	var winners atomic.Int32
	var wg sync.WaitGroup
	work := func(ctx context.Context) error {
		close(entered)
		<-release
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return nil
	}
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if f.m.start(work) {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	<-entered
	if winners.Load() != 1 {
		t.Fatal("multiple writers admitted", winners.Load())
	}
	go func() {
		defer close(finished)
		for i := 0; i < 20; i++ {
			w := request(f.m, "GET", "/api/status", "", true, nil)
			if w.Code != 200 || !strings.Contains(w.Body.String(), `"busy":true`) {
				t.Error("status blocked or inaccurate", w.Body.String())
			}
		}
	}()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("status blocked on background task")
	}
	w := request(f.m, "POST", "/api/complete", "{}", true, nil)
	if w.Code != 409 {
		t.Fatal("concurrent API write not rejected", w.Code)
	}
	close(release)
	waitIdle(t, f.m)
	// An already-cancelled browser request still schedules a detached background job.
	ca := makeFake(f.m)
	ca.order.Status = acme.StatusValid
	setChain(t, ca, f.good)
	attachOrder(t, f.m, ca)
	r := httptest.NewRequest("POST", f.m.c.Origin+"/api/complete", strings.NewReader("{}"))
	r.SetBasicAuth("admin", testPassword)
	r.Header.Set("Origin", f.m.c.Origin)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Cert-Manager", "1")
	ctx, cancel := context.WithCancel(r.Context())
	cancel()
	r = r.WithContext(ctx)
	w = httptest.NewRecorder()
	f.m.ServeHTTP(w, r)
	if w.Code != 202 {
		t.Fatal(w.Code, w.Body.String())
	}
	waitIdle(t, f.m)
	if !f.m.hasPending() {
		t.Fatal("browser cancellation cancelled background issuance")
	}
}

func waitIdle(t *testing.T, m *manager) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		m.mu.RLock()
		busy := m.busy
		m.mu.RUnlock()
		if !busy {
			return
		}
		select {
		case <-deadline.C:
			t.Fatal("task timed out")
		case <-tick.C:
		}
	}
}

func TestACMETransportSafety(t *testing.T) {
	c := acmeHTTPClient()
	tr := c.Transport.(*http.Transport)
	if c.Timeout != 30*time.Second || tr.TLSClientConfig.InsecureSkipVerify || tr.DialContext == nil || c.CheckRedirect == nil {
		t.Fatal("unsafe HTTP transport")
	}
	if tr.TLSClientConfig.MinVersion < tls.VersionTLS12 {
		t.Fatal("TLS too old")
	}
	if c.CheckRedirect(nil, nil) == nil {
		t.Fatal("redirect allowed")
	}
	// Stored password comparison uses SHA256 of the entire file value, not a
	// variable-length constant-time operation or a prefix compare.
	f := newFixture(t)
	expected := sha256.Sum256([]byte(testPassword))
	if f.m.passwordHash != expected {
		t.Fatal("password digest mismatch")
	}
}

func TestInstalledCommitCleanupRecovery(t *testing.T) {
	f := newFixture(t)
	preparePending(t, f)
	writeTest(t, f.m.c.Certificate, f.good)
	s := f.m.snapshot()
	s.Order.Status = "installed"
	s.PendingHash = ""
	if e := f.m.save(s); e != nil {
		t.Fatal(e)
	}
	restarted, e := newManager(f.m.c)
	if e != nil {
		t.Fatal(e)
	}
	if restarted.hasPending() || restarted.snapshot().Order.Status != "installed" {
		t.Fatal("committed install not recovered")
	}
	if _, e = os.Stat(filepath.Join(f.m.c.Directory, "pending-fullchain.pem")); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("stale pending not cleaned up")
	}
	// Unrelated pending data is corruption, not permission to delete it.
	writeTest(t, filepath.Join(f.m.c.Directory, "pending-fullchain.pem"), f.old)
	if _, e = newManager(f.m.c); e == nil {
		t.Fatal("unrelated pending silently discarded")
	}
}

func TestStagingDownloadPreservedButNeverInstalled(t *testing.T) {
	f := newFixture(t)
	f.m.c.CADirectory = staging
	f.m.state.CADirectory = staging
	ca := makeFake(f.m)
	ca.order.Status = acme.StatusValid
	setChain(t, ca, f.good)
	attachOrder(t, f.m, ca)
	if e := f.m.complete(context.Background()); e == nil || !strings.Contains(e.Error(), "staging") {
		t.Fatal("staging issuance not isolated", e)
	}
	if !f.m.hasPending() {
		t.Fatal("staging download was not preserved")
	}
	calls := 0
	f.m.reload = func(context.Context) error { calls++; return nil }
	if f.m.install(context.Background()) == nil || calls != 0 {
		t.Fatal("staging gateway modified")
	}
	current, _ := os.ReadFile(f.m.c.Certificate)
	if !bytes.Equal(current, f.old) {
		t.Fatal("staging changed current certificate")
	}
}

func TestTrustFailurePreservesPendingAndCannotInstall(t *testing.T) {
	f := newFixture(t)
	ca := makeFake(f.m)
	ca.order.Status = acme.StatusValid
	setChain(t, ca, f.good)
	attachOrder(t, f.m, ca)
	f.m.roots = x509.NewCertPool()
	if e := f.m.complete(context.Background()); e == nil || !strings.Contains(e.Error(), "SSL_CERT_DIR") {
		t.Fatal("untrusted chain accepted", e)
	}
	if !f.m.hasPending() {
		t.Fatal("download lost on trust failure")
	}
	if f.m.install(context.Background()) == nil {
		t.Fatal("untrusted pending installed")
	}
	current, _ := os.ReadFile(f.m.c.Certificate)
	if !bytes.Equal(current, f.old) {
		t.Fatal("untrusted pending changed gateway")
	}
}

func TestAuthorizationFetchFailureRetainsOrder(t *testing.T) {
	f := newFixture(t)
	ca := makeFake(f.m)
	ca.failAuth = true
	if e := f.m.prepare(context.Background(), testTerms); e == nil || strings.Contains(e.Error(), "upstream response with secret") {
		t.Fatal("authorization failure ignored/leaked", e)
	}
	s := f.m.snapshot()
	if s.Order == nil || s.Order.URI != ca.order.URI || ca.newOrders != 1 {
		t.Fatal("order not retained after authorization failure")
	}
	ca.failAuth = false
	if e := f.m.prepare(context.Background(), testTerms); e != nil {
		t.Fatal(e)
	}
	if ca.newOrders != 1 || len(f.m.snapshot().Order.Records) != 1 {
		t.Fatal("retry replaced recoverable order")
	}
}

func TestAccountKeySeparateAndCorruptOrphanRefused(t *testing.T) {
	f := newFixture(t)
	k, e := f.m.accountKey(true)
	if e != nil {
		t.Fatal(e)
	}
	accountPublic, _ := x509.MarshalPKIXPublicKey(k.Public())
	tlsPublic, _ := x509.MarshalPKIXPublicKey(&f.key.PublicKey)
	if bytes.Equal(accountPublic, tlsPublic) {
		t.Fatal("ACME account reused TLS key")
	}
	p := filepath.Join(f.m.c.Directory, "account-key.pem")
	writeTest(t, p, []byte("orphaned corrupt key"))
	if _, e = newManager(f.m.c); e == nil {
		t.Fatal("corrupt orphan account key ignored")
	}
}

func TestPrivatePermissionsAndSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX ownership/mode/link enforcement is checked on Unix; Windows cannot enforce mode 0600")
	}
	f := newFixture(t)
	p := filepath.Join(f.m.c.Directory, "admin-password")
	if e := os.Chmod(p, 0644); e != nil {
		t.Fatal(e)
	}
	if _, e := newManager(f.m.c); e == nil {
		t.Fatal("world-readable password accepted")
	}
	if e := os.Chmod(p, 0600); e != nil {
		t.Fatal(e)
	}
	link := filepath.Join(f.m.c.Directory, "link")
	if e := os.Symlink(p, link); e != nil {
		t.Fatal(e)
	}
	if _, e := readFile(link, true); e == nil {
		t.Fatal("read symlink")
	}
	if atomicWrite(link, []byte("bad")) == nil {
		t.Fatal("replaced symlink")
	}
	if e := os.Chmod(f.m.c.Directory, 0755); e != nil {
		t.Fatal(e)
	}
	if stateDirectory(f.m.c.Directory, false) == nil {
		t.Fatal("non-private directory accepted")
	}
}

func TestRealGatewayTLSVerification(t *testing.T) {
	f := newFixture(t)
	pair, e := tls.X509KeyPair(f.good, mustKeyBytes(f.m.c.PrivateKey))
	if e != nil {
		t.Fatal(e)
	}
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	s.Config.ErrorLog = log.New(io.Discard, "", 0)
	s.TLS = &tls.Config{Certificates: []tls.Certificate{pair}}
	s.StartTLS()
	defer s.Close()
	f.m.c.GatewayAddress = s.Listener.Addr().String()
	newBlock, _ := pem.Decode(f.good)
	leaf, _ := x509.ParseCertificate(newBlock.Bytes)
	if e = f.m.checkGateway(context.Background(), leaf); e != nil {
		t.Fatal("trusted matching gateway refused", e)
	}
	oldBlock, _ := pem.Decode(f.old)
	oldLeaf, _ := x509.ParseCertificate(oldBlock.Bytes)
	if e = f.m.checkGateway(context.Background(), oldLeaf); e == nil || !strings.Contains(e.Error(), "pending leaf") {
		t.Fatal("wrong gateway leaf accepted", e)
	}
	f.m.c.Domain = "wrong.example.test"
	if f.m.checkGateway(context.Background(), leaf) == nil {
		t.Fatal("wrong SNI/hostname accepted")
	}
	f.m.c.Domain = f.certificateDomain()
	f.m.roots = x509.NewCertPool()
	if f.m.checkGateway(context.Background(), leaf) == nil {
		t.Fatal("untrusted gateway accepted")
	}
}

func (f *fixture) certificateDomain() string { return "car.example.test" }

func TestIntermediateChainRequired(t *testing.T) {
	f := newFixture(t)
	k := testKey(t)
	now := time.Now()
	template := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "intermediate"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(180 * 24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, e := x509.CreateCertificate(rand.Reader, template, f.ca, &k.PublicKey, f.caKey)
	if e != nil {
		t.Fatal(e)
	}
	inter, e := x509.ParseCertificate(der)
	if e != nil {
		t.Fatal(e)
	}
	leaf := leafPEM(t, f.key, inter, k, f.m.c.Domain, now.Add(-time.Hour), now.Add(90*24*time.Hour))
	if _, e = f.m.validateCertificate(leaf, true); e == nil {
		t.Fatal("missing intermediate accepted")
	}
	chain := append(append([]byte{}, leaf...), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	if _, e = f.m.validateCertificate(chain, true); e != nil {
		t.Fatal("valid fullchain rejected", e)
	}
	chain = append(append([]byte{}, leaf...), f.old...)
	if _, e = f.m.validateCertificate(chain, true); e == nil {
		t.Fatal("unrelated intermediate accepted")
	}
}

func TestCheckCLIIsLocalOnly(t *testing.T) {
	f := newFixture(t)
	// Use only generated test configuration and TLS files. A check must not
	// require a password/state directory or create an account/order.
	c := f.m.c
	c.Directory = filepath.Join(t.TempDir(), "not-created")
	b, e := json.Marshal(c)
	if e != nil {
		t.Fatal(e)
	}
	p := filepath.Join(t.TempDir(), "config.json")
	writeTest(t, p, b)
	args, flags := os.Args, flag.CommandLine
	defer func() { os.Args = args; flag.CommandLine = flags }()
	os.Args = []string{"cert-manager", "-config", p, "-check"}
	flag.CommandLine = flag.NewFlagSet("test-check", flag.ContinueOnError)
	if e = run(); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(c.Directory); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("check created private state")
	}
	writeTest(t, c.PrivateKey, []byte("not a TLS key"))
	flag.CommandLine = flag.NewFlagSet("test-check", flag.ContinueOnError)
	if run() == nil {
		t.Fatal("check accepted malformed TLS")
	}
}

// Every abandon path must leave gateway material, credentials and account
// untouched. All fixtures and callbacks are local; no CA or live gateway used.
func protectAbandonFiles(t *testing.T, m *manager) func() {
	t.Helper()
	writeTest(t, m.c.ReloadScript, []byte("must not execute"))
	writeTest(t, filepath.Join(m.c.Directory, "previous-fullchain.pem"), []byte("keep backup"))
	paths := []string{m.c.Certificate, m.c.PrivateKey, m.c.ReloadScript,
		filepath.Join(m.c.Directory, "previous-fullchain.pem"),
		filepath.Join(m.c.Directory, "admin-password"),
		filepath.Join(m.c.Directory, "account-key.pem")}
	before := make(map[string][]byte)
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		before[p] = b
	}
	m.newCA = func(crypto.Signer, string) authority {
		t.Error("abandon contacted CA")
		return &fakeCA{}
	}
	m.lookupTXT = func(context.Context, string, string) ([]string, error) {
		t.Error("abandon queried DNS")
		return nil, errors.New("unexpected DNS query")
	}
	m.reload = func(context.Context) error { t.Error("abandon reloaded gateway"); return nil }
	m.verifyGateway = func(context.Context, *x509.Certificate) error {
		t.Error("abandon contacted gateway")
		return nil
	}
	return func() {
		t.Helper()
		for p, want := range before {
			got, err := os.ReadFile(p)
			if want == nil {
				if !errors.Is(err, os.ErrNotExist) {
					t.Errorf("abandon created %s", p)
				}
			} else if err != nil || !bytes.Equal(got, want) {
				t.Errorf("abandon changed %s: %v", p, err)
			}
		}
	}
}

func assertAbandoned(t *testing.T, m *manager, account string, issued time.Time) {
	t.Helper()
	s := m.snapshot()
	if s.Order != nil || s.PendingHash != "" || s.Abandoning || s.Installing || m.hasPending() || s.AccountURL != account || !s.IssuedNotAfter.Equal(issued) {
		t.Fatalf("bad abandoned state: %+v, pending=%v", s, m.hasPending())
	}
	if _, err := os.Lstat(filepath.Join(m.c.Directory, "pending-fullchain.pem")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("pending file survived abandon", err)
	}
	b, err := readFile(filepath.Join(m.c.Directory, "state.json"), true)
	var saved diskState
	if err != nil || decodeJSON(b, &saved) != nil {
		t.Fatal("cannot read reset state", err)
	}
	want, _ := json.Marshal(s)
	got, _ := json.Marshal(saved)
	if !bytes.Equal(want, got) {
		t.Fatal("reset state not committed")
	}
}

func TestAbandonConfirmedRepeatedAndRestart(t *testing.T) {
	for _, kind := range []string{"empty", "account only", "pending order", "expired order", "issued", "expired certificate", "corrupt certificate", "installed"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			var issued time.Time
			if kind != "empty" {
				ca := makeFake(f.m)
				attachOrder(t, f.m, ca)
				s := f.m.snapshot()
				switch kind {
				case "account only":
					s.Order = nil
				case "expired order":
					s.Order.Expires = time.Now().Add(-time.Hour)
				case "issued", "expired certificate", "corrupt certificate":
					s.Order.Status = acme.StatusValid
					pending := f.good
					if kind == "expired certificate" {
						pending = leafPEM(t, f.key, f.ca, f.caKey, f.m.c.Domain, time.Now().Add(-48*time.Hour), time.Now().Add(-time.Hour))
					} else if kind == "corrupt certificate" {
						pending = []byte("unusable download")
					}
					writeTest(t, filepath.Join(f.m.c.Directory, "pending-fullchain.pem"), pending)
					s.PendingHash = digest(pending)
					f.m.pending = true
					issued = time.Now().Add(90 * 24 * time.Hour).UTC().Truncate(time.Second)
					s.IssuedNotAfter = issued
				case "installed":
					s.Order.Status = "installed"
				}
				if err := f.m.save(s); err != nil {
					t.Fatal(err)
				}
			}
			account := f.m.snapshot().AccountURL
			check := protectAbandonFiles(t, f.m)
			for i := 0; i < 2; i++ {
				w := request(f.m, "POST", "/api/abandon", `{"confirmAbandon":true}`, true, nil)
				if w.Code != 202 {
					t.Fatal(w.Code, w.Body.String())
				}
				waitIdle(t, f.m)
				assertAbandoned(t, f.m, account, issued)
				if !issued.IsZero() && !f.m.recentlyIssued() {
					t.Fatal("abandon bypassed issuance guard")
				}
				restarted, err := newManager(f.m.c)
				if err != nil {
					t.Fatal(err)
				}
				restarted.newCA = f.m.newCA
				restarted.lookupTXT = f.m.lookupTXT
				restarted.reload = f.m.reload
				restarted.verifyGateway = f.m.verifyGateway
				f.m = restarted
				assertAbandoned(t, f.m, account, issued)
			}
			check()
		})
	}
}

func TestAbandonSecurityAndSerialization(t *testing.T) {
	tests := []struct {
		name, method, body string
		auth               bool
		edit               func(*http.Request)
		want               int
	}{
		{"unauthenticated", "POST", `{"confirmAbandon":true}`, false, nil, 401},
		{"GET", "GET", "", true, nil, 405},
		{"missing confirmation", "POST", `{}`, true, nil, 400},
		{"false", "POST", `{"confirmAbandon":false}`, true, nil, 400},
		{"null", "POST", `{"confirmAbandon":null}`, true, nil, 400},
		{"string", "POST", `{"confirmAbandon":"true"}`, true, nil, 400},
		{"unknown field", "POST", `{"confirmAbandon":true,"extra":1}`, true, nil, 400},
		{"two objects", "POST", `{"confirmAbandon":true} {}`, true, nil, 400},
		{"missing origin", "POST", `{"confirmAbandon":true}`, true, func(r *http.Request) { r.Header.Del("Origin") }, 403},
		{"missing csrf", "POST", `{"confirmAbandon":true}`, true, func(r *http.Request) { r.Header.Del("X-Cert-Manager") }, 403},
		{"wrong media", "POST", `{"confirmAbandon":true}`, true, func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, 415},
		{"oversized", "POST", strings.Repeat(" ", bodyLimit+1), true, nil, 413},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			preparePending(t, f)
			before, _ := os.ReadFile(filepath.Join(f.m.c.Directory, "state.json"))
			check := protectAbandonFiles(t, f.m)
			w := request(f.m, tt.method, "/api/abandon", tt.body, tt.auth, tt.edit)
			if w.Code != tt.want {
				t.Fatal(w.Code, w.Body.String())
			}
			after, _ := os.ReadFile(filepath.Join(f.m.c.Directory, "state.json"))
			pending, _ := os.ReadFile(filepath.Join(f.m.c.Directory, "pending-fullchain.pem"))
			if !bytes.Equal(before, after) || !bytes.Equal(pending, f.good) {
				t.Fatal("rejected request mutated state/pending")
			}
			check()
		})
	}
	f := newFixture(t)
	preparePending(t, f)
	release := make(chan struct{})
	if !f.m.start(func(context.Context) error { <-release; return nil }) {
		t.Fatal("cannot start test operation")
	}
	w := request(f.m, "POST", "/api/abandon", `{"confirmAbandon":true}`, true, nil)
	close(release)
	waitIdle(t, f.m)
	if w.Code != 409 || !f.m.hasPending() || f.m.snapshot().Abandoning {
		t.Fatal("abandon was not serialized", w.Code)
	}
	s := f.m.snapshot()
	s.Installing = true
	if err := f.m.save(s); err != nil {
		t.Fatal(err)
	}
	check := protectAbandonFiles(t, f.m)
	w = request(f.m, "POST", "/api/abandon", `{"confirmAbandon":true}`, true, nil)
	if w.Code != 409 || f.m.abandon(context.Background()) == nil || !f.m.hasPending() || !f.m.snapshot().Installing {
		t.Fatal("abandoned interrupted install", w.Code)
	}
	s.Abandoning = true
	if f.m.save(s) == nil {
		t.Fatal("accepted simultaneous install and abandon")
	}
	check()
}

func TestAbandonCrashRecovery(t *testing.T) {
	for _, stage := range []string{"before unlink", "after unlink", "before pending hash commit", "empty order", "corrupt pending", "changed pending"} {
		t.Run(stage, func(t *testing.T) {
			f := newFixture(t)
			preparePending(t, f)
			s := f.m.snapshot()
			s.Abandoning = true
			s.IssuedNotAfter = time.Now().Add(80 * 24 * time.Hour).UTC().Truncate(time.Second)
			if stage == "before pending hash commit" {
				s.PendingHash = ""
			}
			if stage == "corrupt pending" {
				writeTest(t, filepath.Join(f.m.c.Directory, "pending-fullchain.pem"), []byte("partial download"))
			} else if stage == "changed pending" {
				writeTest(t, filepath.Join(f.m.c.Directory, "pending-fullchain.pem"), f.old)
			}
			if stage == "after unlink" || stage == "empty order" {
				if err := os.Remove(filepath.Join(f.m.c.Directory, "pending-fullchain.pem")); err != nil {
					t.Fatal(err)
				}
			}
			if stage == "empty order" {
				s.Order = nil
				s.PendingHash = ""
			}
			if err := f.m.save(s); err != nil {
				t.Fatal(err)
			}
			check := protectAbandonFiles(t, f.m)
			for i := 0; i < 2; i++ {
				restarted, err := newManager(f.m.c)
				if err != nil {
					t.Fatal("interrupted abandon did not recover", err)
				}
				assertAbandoned(t, restarted, s.AccountURL, s.IssuedNotAfter)
				if !restarted.recentlyIssued() {
					t.Fatal("restart lost issuance rate guard")
				}
			}
			check()
		})
	}
}

func TestAbandonPersistenceAndCleanupFailures(t *testing.T) {
	for _, stage := range []string{"intent save", "unlink", "reset save"} {
		t.Run(stage, func(t *testing.T) {
			f := newFixture(t)
			preparePending(t, f)
			check := protectAbandonFiles(t, f.m)
			s := f.m.snapshot()
			statePath := filepath.Join(f.m.c.Directory, "state.json")
			backup := statePath + ".test-backup"
			pending := filepath.Join(f.m.c.Directory, "pending-fullchain.pem")
			if stage != "intent save" {
				s.Abandoning = true
				if err := f.m.save(s); err != nil {
					t.Fatal(err)
				}
			}
			blocked := statePath
			if stage == "unlink" {
				blocked = pending
				backup = pending + ".test-backup"
			}
			if err := os.Rename(blocked, backup); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(blocked, 0700); err != nil {
				t.Fatal(err)
			}
			if err := f.m.abandon(context.Background()); err == nil {
				t.Fatal("abandon ignored disk failure")
			}
			if stage == "intent save" {
				b, err := os.ReadFile(pending)
				if err != nil || !bytes.Equal(b, f.good) || f.m.snapshot().Abandoning {
					t.Fatal("deleted pending before durable intent")
				}
			} else {
				if !f.m.snapshot().Abandoning {
					t.Fatal("failed cleanup lost recovery marker")
				}
				markerPath := statePath
				if stage == "reset save" {
					markerPath = backup
					if _, err := os.Lstat(pending); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("reset attempted before pending deletion")
					}
				}
				b, err := readFile(markerPath, true)
				var marker diskState
				if err != nil || decodeJSON(b, &marker) != nil || !marker.Abandoning || marker.Order == nil || marker.PendingHash != s.PendingHash {
					t.Fatal("durable abandon intent/order/hash lost on failure", err)
				}
				if f.m.prepare(context.Background(), testTerms) == nil || f.m.complete(context.Background()) == nil || f.m.install(context.Background()) == nil {
					t.Fatal("other writers allowed during interrupted abandon")
				}
			}
			if stage == "unlink" {
				if _, err := newManager(f.m.c); err == nil {
					t.Fatal("startup ignored unsafe pending cleanup")
				}
			}
			if err := os.Remove(blocked); err != nil {
				t.Fatal("cleanup removed a non-regular file", err)
			}
			if err := os.Rename(backup, blocked); err != nil {
				t.Fatal(err)
			}
			if stage == "intent save" {
				if err := f.m.abandon(context.Background()); err != nil {
					t.Fatal(err)
				}
			} else {
				restarted, err := newManager(f.m.c)
				if err != nil {
					t.Fatal(err)
				}
				f.m = restarted
			}
			assertAbandoned(t, f.m, s.AccountURL, s.IssuedNotAfter)
			check()
		})
	}
}

func TestAbandonPreserves35DayRateBoundary(t *testing.T) {
	for _, days := range []int{90, 36, 35, 34, -1} {
		t.Run(time.Duration(days*24).String(), func(t *testing.T) {
			f := newFixture(t)
			preparePending(t, f)
			s := f.m.snapshot()
			s.IssuedNotAfter = time.Now().Add(time.Duration(days) * 24 * time.Hour).UTC().Truncate(time.Second)
			if err := f.m.save(s); err != nil {
				t.Fatal(err)
			}
			check := protectAbandonFiles(t, f.m)
			if err := f.m.abandon(context.Background()); err != nil {
				t.Fatal(err)
			}
			restarted, err := newManager(f.m.c)
			if err != nil {
				t.Fatal(err)
			}
			assertAbandoned(t, restarted, s.AccountURL, s.IssuedNotAfter)
			if restarted.recentlyIssued() != (days > 35) {
				t.Fatal("abandon changed 35-day rate-limit boundary", days)
			}
			check()
		})
	}
}
