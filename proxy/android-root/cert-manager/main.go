// cert-manager is a separate, root-owned HTTPS service for manual DNS-01 renewal.
// It never changes pairing, DNS-provider settings, or the gateway until install.
// Build with web/index.html, web/styles.css and web/app.js beside this file.
// The UI must keep Basic credentials ONLY in memory and send Authorization on API
// requests (never localStorage, cookies or URLs). Renew BEFORE expiry: browsers
// normally refuse this management page once its currently served cert expires.
// Android must set SSL_CERT_DIR to a maintained system-root directory. Staging
// chains are deliberately not installable. Run only one process per directory.
// Run as root; keep the config, state directory, fixed executable reload script
// and their parent directories root-controlled (no group/other write access).
// The script must have an Android-compatible shebang. The UI flow is terms ->
// prepare -> manually publish TXT -> complete -> explicitly confirm install.
package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/acme"
)

const (
	production = "https://acme-v02.api.letsencrypt.org/directory"
	staging    = "https://acme-staging-v02.api.letsencrypt.org/directory"
	bodyLimit  = 4096
	fileLimit  = 1 << 20
)

// webFiles requires the UI owner's index.html, styles.css and app.js at build time.
//
//go:embed web/*
var webFiles embed.FS

type config struct {
	Domain         string `json:"domain"`
	Listen         string `json:"listen"`
	Origin         string `json:"origin"`
	Directory      string `json:"directory"`
	Certificate    string `json:"certificate"`
	PrivateKey     string `json:"privateKey"`
	ReloadScript   string `json:"reloadScript"`
	GatewayAddress string `json:"gatewayAddress"`
	CADirectory    string `json:"caDirectory"`
}

func ipv4Port(s string, private bool) (string, error) {
	h, p, err := net.SplitHostPort(s)
	ip, e := netip.ParseAddr(h)
	n, pe := strconv.Atoi(p)
	if err != nil || e != nil || !ip.Is4() || pe != nil || n < 1 || n > 65535 || p != strconv.Itoa(n) || (private && !ip.IsPrivate()) || ip.IsUnspecified() || ip.IsMulticast() {
		return "", errors.New("address must be an explicit IPv4:port (listen must use a private IPv4)")
	}
	return p, nil
}

func (c config) validate() error {
	if len(c.Domain) > 253 || c.Domain != strings.ToLower(c.Domain) || !strings.Contains(c.Domain, ".") || net.ParseIP(c.Domain) != nil {
		return errors.New("domain must be a lowercase DNS hostname, not an IP or wildcard")
	}
	for _, label := range strings.Split(c.Domain, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return errors.New("invalid domain label")
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
				return errors.New("invalid domain character")
			}
		}
	}
	p, err := ipv4Port(c.Listen, true)
	if err != nil {
		return err
	}
	if c.Origin != "https://"+net.JoinHostPort(c.Domain, p) {
		return errors.New("origin must be exactly https://domain:listen-port, including explicit port")
	}
	if _, err = ipv4Port(c.GatewayAddress, false); err != nil {
		return err
	}
	if c.CADirectory != production && c.CADirectory != staging {
		return errors.New("caDirectory must be the Let's Encrypt production or staging directory")
	}
	seen := map[string]bool{}
	for _, p := range []string{c.Directory, c.Certificate, c.PrivateKey, c.ReloadScript} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p || seen[p] {
			return errors.New("directory and fixed file paths must be distinct, clean absolute paths")
		}
		seen[p] = true
	}
	for _, name := range []string{"admin-password", "account-key.pem", "state.json", "pending-fullchain.pem", "previous-fullchain.pem"} {
		p := filepath.Join(c.Directory, name)
		if seen[p] {
			return errors.New("configured path overlaps a reserved state file")
		}
	}
	return nil
}

func decodeJSON(b []byte, v any) error {
	b = bytes.TrimSpace(b)
	if len(b) == 0 || b[0] != '{' {
		return errors.New("expected a JSON object")
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return errors.New("invalid JSON or unknown fields")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("expected a single JSON object")
	}
	return nil
}

// Reject links, oversized/truncated files and insecure state instead of silently
// replacing an unreadable account/order with a new one.
func readFile(path string, private bool) ([]byte, error) {
	s, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !s.Mode().IsRegular() || s.Size() > fileLimit {
		return nil, errors.New("file is not a bounded regular file")
	}
	if private && runtime.GOOS != "windows" && s.Mode().Perm() != 0600 {
		return nil, errors.New("private state file must have mode 0600")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, fileLimit+1))
	if len(b) > fileLimit {
		return nil, errors.New("file too large")
	}
	return b, err
}

func stateDirectory(path string, create bool) error {
	if create {
		if err := os.MkdirAll(path, 0700); err != nil {
			return errors.New("cannot create private state directory")
		}
	}
	s, err := os.Lstat(path)
	if err != nil || !s.IsDir() || s.Mode()&os.ModeSymlink != 0 {
		return errors.New("state directory must be a real directory")
	}
	if runtime.GOOS != "windows" && s.Mode().Perm() != 0700 {
		return errors.New("state directory must have mode 0700")
	}
	return nil
}

// Same-directory temp + 0600 + fsync + rename; fsync the directory on Unix too.
func atomicWrite(path string, b []byte) error {
	if s, err := os.Lstat(path); err == nil && !s.Mode().IsRegular() {
		return errors.New("refusing to replace non-regular file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".cert-manager-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err == nil {
		err = ce
	}
	if err != nil {
		return err
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		d, err := os.Open(filepath.Dir(path))
		if err != nil {
			return err
		}
		defer d.Close()
		return d.Sync()
	}
	return nil
}

func initPassword(c config) error {
	if err := stateDirectory(c.Directory, true); err != nil {
		return err
	}
	p := filepath.Join(c.Directory, "admin-password")
	if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
		return errors.New("password file already exists or cannot be inspected; not overwriting")
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return errors.New("password randomness unavailable")
	}
	if err := atomicWrite(p, []byte(base64.RawURLEncoding.EncodeToString(b))); err != nil {
		return errors.New("cannot write password file")
	}
	fmt.Println(p)
	return nil
}

type record struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Private disk format: never marshal this struct to HTTP responses.
type savedOrder struct {
	URI         string    `json:"uri"`
	Status      string    `json:"status"`
	Expires     time.Time `json:"expires"`
	AuthzURLs   []string  `json:"authzURLs"`
	FinalizeURL string    `json:"finalizeURL"`
	CertURL     string    `json:"certURL"`
	Records     []record  `json:"records"`
}
type diskState struct {
	Version        int         `json:"version"`
	Domain         string      `json:"domain"`
	CADirectory    string      `json:"caDirectory"`
	AccountURL     string      `json:"accountURL"`
	Order          *savedOrder `json:"order"`
	PendingHash    string      `json:"pendingHash"`
	IssuedNotAfter time.Time   `json:"issuedNotAfter"`
	// An interrupted install is explicitly recovered before any new writes.
	Installing bool `json:"installing"`
	// Durable confirmation: finish local cleanup on restart, without touching TLS.
	Abandoning bool `json:"abandoning"`
}

type authority interface {
	Discover(context.Context) (acme.Directory, error)
	Register(context.Context, *acme.Account, func(string) bool) (*acme.Account, error)
	GetReg(context.Context, string) (*acme.Account, error)
	AuthorizeOrder(context.Context, []acme.AuthzID, ...acme.OrderOption) (*acme.Order, error)
	GetOrder(context.Context, string) (*acme.Order, error)
	GetAuthorization(context.Context, string) (*acme.Authorization, error)
	DNS01ChallengeRecord(string) (string, error)
	Accept(context.Context, *acme.Challenge) (*acme.Challenge, error)
	WaitOrder(context.Context, string) (*acme.Order, error)
	CreateCertFromOrder(context.Context, *acme.Order, []byte, bool) ([][]byte, string, error)
	FetchCert(context.Context, string, bool) ([][]byte, error)
}

type manager struct {
	c             config
	mu            sync.RWMutex // only in-memory snapshots; NEVER held across IO/network/reload
	state         diskState
	busy          bool
	message       string
	terms         string
	termsAt       time.Time
	pending       bool
	passwordHash  [32]byte
	authMu        sync.Mutex
	authWindow    time.Time
	authFailures  int
	termsGate     chan struct{}
	newCA         func(crypto.Signer, string) authority
	lookupTXT     func(context.Context, string, string) ([]string, error)
	reload        func(context.Context) error
	verifyGateway func(context.Context, *x509.Certificate) error
	roots         *x509.CertPool // nil means system roots, SSL_CERT_DIR on Android
}

func publicResolver(address string) *net.Resolver {
	return &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, address)
	}}
}

func acmeHTTPClient() *http.Client {
	// Explicit DNS also applies to proxy hostname resolution. No dependency on
	// Android's absent /etc/resolv.conf, no insecure TLS or private root injection.
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second, Resolver: publicResolver("1.1.1.1:53")}
	t := &http.Transport{Proxy: http.ProxyFromEnvironment, DialContext: d.DialContext, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 15 * time.Second, MaxResponseHeaderBytes: 32 << 10, MaxConnsPerHost: 4, MaxIdleConns: 4, IdleConnTimeout: 30 * time.Second}
	return &http.Client{Transport: t, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("ACME redirects disabled") }}
}

func newManager(c config) (*manager, error) {
	if err := c.validate(); err != nil {
		return nil, err
	}
	if err := stateDirectory(c.Directory, false); err != nil {
		return nil, err
	}
	pw, err := readFile(filepath.Join(c.Directory, "admin-password"), true)
	if err != nil {
		return nil, errors.New("cannot read private admin-password; run -init-password first")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(string(pw))
	if err != nil || len(decoded) != 32 || len(pw) != 43 {
		return nil, errors.New("admin-password must contain exactly a base64url-encoded 32-byte password")
	}
	m := &manager{c: c, passwordHash: sha256.Sum256(pw), termsGate: make(chan struct{}, 1), message: "Renew before expiry; installation always requires confirmation."}
	m.state = diskState{Version: 1, Domain: c.Domain, CADirectory: c.CADirectory}
	b, err := readFile(filepath.Join(c.Directory, "state.json"), true)
	if err == nil {
		var loaded diskState // never inherit defaults into a damaged disk object
		if decodeJSON(b, &loaded) != nil || m.validateState(loaded) != nil {
			return nil, errors.New("state.json is corrupt or belongs to a different configuration; refusing to overwrite")
		}
		m.state = loaded
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("cannot read state.json; refusing to overwrite")
	}
	_, keyErr := os.Lstat(filepath.Join(c.Directory, "account-key.pem"))
	if !errors.Is(keyErr, os.ErrNotExist) || m.state.AccountURL != "" || m.state.Order != nil {
		if _, err := m.accountKey(false); err != nil {
			return nil, err
		}
	}
	if m.state.Abandoning {
		if err := m.finishAbandon(); err != nil {
			return nil, err
		}
	}
	pending, err := readFile(filepath.Join(c.Directory, "pending-fullchain.pem"), true)
	if err == nil {
		if _, err := tls.X509KeyPair(pending, mustKeyBytes(c.PrivateKey)); err != nil {
			return nil, errors.New("pending certificate is corrupt or does not match configured key")
		}
		if m.state.Order == nil {
			return nil, errors.New("orphaned pending certificate; refusing to overwrite")
		}
		if m.state.PendingHash != "" && digest(pending) != m.state.PendingHash {
			return nil, errors.New("pending certificate digest disagrees with state")
		}
		if m.state.Order.Status == "installed" {
			// Crash after installed-state commit, before pending unlink. Only clean
			// up the exact installed leaf, never silently discard unrelated data.
			current, e := m.currentLeaf()
			pair, pe := tls.X509KeyPair(pending, mustKeyBytes(c.PrivateKey))
			if e != nil || pe != nil || !bytes.Equal(current.Raw, pair.Certificate[0]) {
				return nil, errors.New("installed-state pending file disagrees with current certificate")
			}
			if os.Remove(filepath.Join(c.Directory, "pending-fullchain.pem")) != nil {
				return nil, errors.New("cannot clean up committed pending certificate")
			}
		} else {
			m.pending = true // also recovers crash after pending rename, before state commit
		}
	} else if !errors.Is(err, os.ErrNotExist) || m.state.PendingHash != "" && m.state.Order.Status != "installed" {
		return nil, errors.New("pending certificate is missing or unreadable")
	}
	if m.state.Installing {
		m.message = "Interrupted installation detected. Confirm install to restore the persistent backup first; no automatic reload has run."
	}
	client := acmeHTTPClient()
	m.newCA = func(key crypto.Signer, kid string) authority {
		return &acme.Client{Key: key, KID: acme.KeyID(kid), DirectoryURL: c.CADirectory, HTTPClient: client, UserAgent: "DiPlay-cert-manager/1"}
	}
	m.lookupTXT = func(ctx context.Context, resolver, name string) ([]string, error) {
		return publicResolver(resolver).LookupTXT(ctx, name)
	}
	m.reload = func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, c.ReloadScript) // one fixed root-owned executable, no args or shell
		// nil Stdout/Stderr go straight to the OS null device. io.Discard would
		// create copy pipes that a surviving child could hold open past timeout.
		if err := cmd.Run(); err != nil {
			return errors.New("fixed reload script failed or timed out")
		}
		return nil
	}
	m.verifyGateway = m.checkGateway
	return m, nil
}

func (m *manager) checkGateway(ctx context.Context, leaf *x509.Certificate) error {
	// roots is nil in all production construction paths: system roots only.
	d := tls.Dialer{NetDialer: &net.Dialer{Timeout: 15 * time.Second}, Config: &tls.Config{ServerName: m.c.Domain, RootCAs: m.roots, MinVersion: tls.VersionTLS12}}
	conn, err := d.DialContext(ctx, "tcp", m.c.GatewayAddress)
	if err != nil {
		return errors.New("gateway TLS verification failed")
	}
	defer conn.Close()
	peer := conn.(*tls.Conn).ConnectionState().PeerCertificates
	if len(peer) == 0 || !bytes.Equal(peer[0].Raw, leaf.Raw) {
		return errors.New("gateway did not serve the pending leaf certificate")
	}
	return nil
}

func mustKeyBytes(path string) []byte { b, _ := readFile(path, false); return b }
func digest(b []byte) string          { d := sha256.Sum256(b); return hex.EncodeToString(d[:]) }

func (m *manager) validCAURL(s string) bool {
	u, err := url.Parse(s)
	base, _ := url.Parse(m.c.CADirectory)
	return err == nil && u.Scheme == "https" && u.Host == base.Host && u.User == nil && u.Fragment == "" && u.Path != ""
}

func (m *manager) validateState(s diskState) error {
	bad := errors.New("invalid state")
	if s.Version != 1 || s.Domain != m.c.Domain || s.CADirectory != m.c.CADirectory {
		return bad
	}
	if s.Installing && s.Abandoning {
		return bad
	}
	if s.AccountURL != "" && !m.validCAURL(s.AccountURL) {
		return bad
	}
	if s.PendingHash != "" {
		b, e := hex.DecodeString(s.PendingHash)
		if e != nil || len(b) != 32 {
			return bad
		}
	}
	if s.Order != nil {
		o := s.Order
		if s.AccountURL == "" || !m.validCAURL(o.URI) || !m.validCAURL(o.FinalizeURL) || o.Expires.IsZero() {
			return bad
		}
		switch o.Status {
		case "pending", "processing", "ready", "valid", "invalid", "installed":
		default:
			return bad
		}
		if o.CertURL != "" && !m.validCAURL(o.CertURL) {
			return bad
		}
		for _, u := range o.AuthzURLs {
			if !m.validCAURL(u) {
				return bad
			}
		}
		for _, r := range o.Records {
			if r.Name != "_acme-challenge."+m.c.Domain || r.Value == "" || len(r.Value) > 128 {
				return bad
			}
		}
	} else if s.Installing || s.PendingHash != "" {
		return bad
	}
	return nil
}

func cloneState(s diskState) diskState {
	if s.Order != nil {
		o := *s.Order
		o.Records = append([]record{}, o.Records...)
		o.AuthzURLs = append([]string{}, o.AuthzURLs...)
		s.Order = &o
	}
	return s
}
func (m *manager) snapshot() diskState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return cloneState(m.state)
}
func (m *manager) save(s diskState) error {
	if err := m.validateState(s); err != nil {
		return errors.New("refusing invalid ACME state")
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return errors.New("cannot encode state")
	}
	if err = atomicWrite(filepath.Join(m.c.Directory, "state.json"), b); err != nil {
		return errors.New("cannot persist state; operation stopped")
	}
	m.mu.Lock()
	m.state = cloneState(s)
	m.mu.Unlock()
	return nil
}

func (m *manager) accountKey(create bool) (crypto.Signer, error) {
	p := filepath.Join(m.c.Directory, "account-key.pem")
	b, err := readFile(p, true)
	if errors.Is(err, os.ErrNotExist) && create {
		k, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if e != nil {
			return nil, errors.New("cannot generate account key")
		}
		der, e := x509.MarshalPKCS8PrivateKey(k)
		if e != nil {
			return nil, errors.New("cannot encode account key")
		}
		if atomicWrite(p, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})) != nil {
			return nil, errors.New("cannot persist account key")
		}
		return k, nil
	}
	if err != nil {
		return nil, errors.New("cannot read existing account key; refusing replacement")
	}
	block, rest := pem.Decode(b)
	if block == nil || block.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("corrupt account key; refusing replacement")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("corrupt account key; refusing replacement")
	}
	signer, ok := k.(crypto.Signer)
	if !ok {
		return nil, errors.New("unsupported account key")
	}
	return signer, nil
}

func (m *manager) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	b, err := readFile(m.c.Certificate, false)
	if err != nil {
		return nil, errors.New("cannot load current fullchain")
	}
	k, err := readFile(m.c.PrivateKey, false)
	if err != nil {
		return nil, errors.New("cannot load current TLS key")
	}
	pair, err := tls.X509KeyPair(b, k)
	if err != nil {
		return nil, errors.New("current certificate/key cannot be loaded")
	}
	// Deliberately do not check expiry here, so TLS can still serve an expired
	// certificate. This does NOT bypass the browser's trust/expiry checks.
	return &pair, nil
}

func (m *manager) currentLeaf() (*x509.Certificate, error) {
	p, err := m.getCertificate(nil)
	if err != nil {
		return nil, err
	}
	l, err := x509.ParseCertificate(p.Certificate[0])
	if err != nil {
		return nil, errors.New("cannot parse current leaf")
	}
	return l, nil
}

func (m *manager) validateCertificate(b []byte, newer bool) (*x509.Certificate, error) {
	k, err := readFile(m.c.PrivateKey, false)
	if err != nil {
		return nil, errors.New("cannot load configured TLS key")
	}
	pair, err := tls.X509KeyPair(b, k)
	if err != nil {
		return nil, errors.New("certificate does not match configured key or is malformed")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, errors.New("invalid certificate leaf")
	}
	if leaf.VerifyHostname(m.c.Domain) != nil {
		return nil, errors.New("certificate hostname mismatch")
	}
	now := time.Now()
	if now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return nil, errors.New("certificate is expired or not yet valid")
	}
	inter := x509.NewCertPool()
	for _, der := range pair.Certificate[1:] {
		c, e := x509.ParseCertificate(der)
		if e != nil {
			return nil, errors.New("invalid intermediate certificate")
		}
		inter.AddCert(c)
	}
	if _, err = leaf.Verify(x509.VerifyOptions{DNSName: m.c.Domain, Roots: m.roots, Intermediates: inter, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return nil, errors.New("certificate chain is not trusted by system roots (check SSL_CERT_DIR)")
	}
	if newer {
		old, e := m.currentLeaf()
		if e != nil {
			return nil, e
		}
		if !leaf.NotAfter.After(old.NotAfter) || bytes.Equal(leaf.Raw, old.Raw) {
			return nil, errors.New("pending certificate is not newer than the current certificate")
		}
	}
	return leaf, nil
}

func (m *manager) recentlyIssued() bool {
	if m.snapshot().IssuedNotAfter.After(time.Now().Add(35 * 24 * time.Hour)) {
		return true
	}
	l, err := m.currentLeaf()
	if err != nil || !l.NotAfter.After(time.Now().Add(35*24*time.Hour)) {
		return false
	}
	// Only an actually self-signed bootstrap may bypass the guard. Missing
	// system roots must not accidentally permit repeated early CA orders.
	selfSigned := bytes.Equal(l.RawIssuer, l.RawSubject) && l.CheckSignature(l.SignatureAlgorithm, l.RawTBSCertificate, l.Signature) == nil
	return !selfSigned && l.VerifyHostname(m.c.Domain) == nil && !time.Now().Before(l.NotBefore)
}

func (m *manager) discover(ctx context.Context) (string, error) {
	d, err := m.newCA(nil, "").Discover(ctx)
	if err != nil {
		return "", errors.New("cannot discover Let's Encrypt terms; check public DNS, network and SSL_CERT_DIR")
	}
	u, e := url.Parse(d.Terms)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return "", errors.New("CA returned no usable HTTPS terms URL")
	}
	return d.Terms, nil
}

func (m *manager) updateOrder(s diskState, o *acme.Order) (diskState, error) {
	if o == nil || len(o.Identifiers) != 1 || o.Identifiers[0].Type != "dns" || o.Identifiers[0].Value != m.c.Domain {
		return s, errors.New("CA order identifiers do not match configured domain")
	}
	records := []record{}
	if s.Order != nil {
		records = s.Order.Records
	}
	s.Order = &savedOrder{URI: o.URI, Status: o.Status, Expires: o.Expires, AuthzURLs: o.AuthzURLs, FinalizeURL: o.FinalizeURL, CertURL: o.CertURL, Records: records}
	if err := m.save(s); err != nil {
		return s, err
	}
	return s, nil
}

func liveOrder(o *savedOrder) bool {
	return o != nil && o.Status != "installed" && o.Status != "invalid" && (o.Status == "valid" || time.Now().Before(o.Expires))
}

func (m *manager) prepare(ctx context.Context, accepted string) error {
	if m.snapshot().Abandoning {
		return errors.New("interrupted abandon must be finished using abandon first")
	}
	if m.recentlyIssued() {
		return errors.New("certificate has more than 35 days remaining; renewal blocked to protect CA limits")
	}
	terms, err := m.discover(ctx)
	if err != nil {
		return err
	}
	if terms != accepted {
		m.mu.Lock()
		m.terms = ""
		m.termsAt = time.Time{}
		m.mu.Unlock()
		return errors.New("CA terms changed; GET /api/terms again and explicitly accept them")
	}
	s := m.snapshot()
	if s.Installing {
		return errors.New("interrupted install must be recovered using install first")
	}
	k, err := m.accountKey(true)
	if err != nil {
		return err
	}
	ca := m.newCA(k, s.AccountURL)
	if s.AccountURL == "" {
		// Lookup first recovers a crash after registration but before disk commit.
		a, e := ca.GetReg(ctx, "")
		if errors.Is(e, acme.ErrNoAccount) {
			a, e = ca.Register(ctx, &acme.Account{}, func(t string) bool { return t == accepted })
			if errors.Is(e, acme.ErrAccountAlreadyExists) {
				a, e = ca.GetReg(ctx, "")
			}
		}
		if e != nil || a == nil || a.URI == "" {
			return errors.New("ACME account registration/recovery failed; retry after checking connectivity")
		}
		s.AccountURL = a.URI
		if err = m.save(s); err != nil {
			return err
		}
		ca = m.newCA(k, s.AccountURL)
	}
	if liveOrder(s.Order) {
		o, e := ca.GetOrder(ctx, s.Order.URI)
		if e != nil {
			return errors.New("cannot refresh existing order; refusing to replace it")
		}
		s, err = m.updateOrder(s, o)
		if err != nil {
			return err
		}
		if liveOrder(s.Order) {
			return m.refreshRecords(ctx, ca, s)
		}
	}
	if m.hasPending() {
		return errors.New("pending certificate must be installed before creating another order")
	}
	s.Order = nil
	s.PendingHash = ""
	o, err := ca.AuthorizeOrder(ctx, []acme.AuthzID{{Type: "dns", Value: m.c.Domain}})
	if err != nil {
		return errors.New("cannot create ACME order; retry without repeated requests")
	}
	// Persist the order URL BEFORE fetching authorizations or doing anything else.
	s, err = m.updateOrder(s, o)
	if err != nil {
		return err
	}
	return m.refreshRecords(ctx, ca, s)
}

func dnsChallenge(a *acme.Authorization, domain string) (*acme.Challenge, error) {
	if a == nil || a.Identifier.Type != "dns" || a.Identifier.Value != domain || a.Wildcard {
		return nil, errors.New("authorization domain mismatch")
	}
	if a.Status == acme.StatusValid {
		return nil, nil
	}
	if a.Status != acme.StatusPending {
		return nil, errors.New("DNS authorization is invalid or expired; prepare a new order")
	}
	for _, c := range a.Challenges {
		if c != nil && c.Type == "dns-01" {
			return c, nil
		}
	}
	return nil, errors.New("CA did not offer DNS-01")
}

func (m *manager) refreshRecords(ctx context.Context, ca authority, s diskState) error {
	records := []record{}
	for _, u := range s.Order.AuthzURLs {
		a, err := ca.GetAuthorization(ctx, u)
		if err != nil {
			return errors.New("cannot read authorization; existing order retained for retry")
		}
		c, err := dnsChallenge(a, m.c.Domain)
		if err != nil {
			return err
		}
		if c == nil {
			continue
		}
		v, err := ca.DNS01ChallengeRecord(c.Token)
		if err != nil {
			return errors.New("cannot calculate DNS TXT value")
		}
		records = append(records, record{"_acme-challenge." + m.c.Domain, v})
	}
	s.Order.Records = records
	return m.save(s)
}

func (m *manager) checkDNS(ctx context.Context, records []record) error {
	for _, r := range records {
		for _, resolver := range []string{"1.1.1.1:53", "8.8.8.8:53"} {
			qctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			values, err := m.lookupTXT(qctx, resolver, r.Name)
			cancel()
			found := false
			for _, v := range values {
				if v == r.Value {
					found = true
				}
			}
			if err != nil || !found {
				return fmt.Errorf("DNS TXT is not visible at %s for %s; publish the exact record and wait, then retry complete (nothing submitted to CA)", resolver, r.Name)
			}
		}
	}
	return nil
}

func (m *manager) complete(ctx context.Context) error {
	s := m.snapshot()
	if s.Abandoning {
		return errors.New("interrupted abandon must be finished using abandon first")
	}
	if s.Installing {
		return errors.New("interrupted install must be recovered using install first")
	}
	if s.Order == nil || s.Order.Status == "installed" {
		return errors.New("no unfinished order; prepare first")
	}
	k, err := m.accountKey(false)
	if err != nil {
		return err
	}
	ca := m.newCA(k, s.AccountURL)
	o, err := ca.GetOrder(ctx, s.Order.URI)
	if err != nil {
		return errors.New("cannot refresh order; retry complete")
	}
	s, err = m.updateOrder(s, o)
	if err != nil {
		return err
	}
	if o.Status != acme.StatusValid && !time.Now().Before(o.Expires) {
		return errors.New("order expired; prepare a new order")
	}
	if o.Status == acme.StatusPending {
		// Collect/check ALL outstanding TXT records BEFORE the first Accept.
		var challenges []*acme.Challenge
		var records []record
		for _, u := range o.AuthzURLs {
			a, e := ca.GetAuthorization(ctx, u)
			if e != nil {
				return errors.New("cannot refresh DNS authorization; retry complete")
			}
			c, e := dnsChallenge(a, m.c.Domain)
			if e != nil {
				return e
			}
			if c == nil {
				continue
			}
			if !m.validCAURL(c.URI) {
				return errors.New("invalid challenge endpoint")
			}
			if c.Status == acme.StatusProcessing || c.Status == acme.StatusValid {
				continue
			}
			if c.Status != acme.StatusPending {
				return errors.New("DNS challenge failed; prepare a new order after CA marks it invalid")
			}
			v, e := ca.DNS01ChallengeRecord(c.Token)
			if e != nil {
				return errors.New("cannot calculate DNS TXT value")
			}
			records = append(records, record{"_acme-challenge." + m.c.Domain, v})
			challenges = append(challenges, c)
		}
		s.Order.Records = records
		if err = m.save(s); err != nil {
			return err
		}
		if err = m.checkDNS(ctx, records); err != nil {
			return err
		}
		for _, c := range challenges {
			if _, err = ca.Accept(ctx, c); err != nil {
				return errors.New("CA challenge submission failed; retry complete to recover its current state")
			}
		}
	}
	if o.Status == acme.StatusPending || o.Status == acme.StatusProcessing {
		o, err = ca.WaitOrder(ctx, o.URI)
		if err != nil {
			return errors.New("CA validation/issuance not ready or failed; retry complete to refresh status")
		}
		s, err = m.updateOrder(s, o)
		if err != nil {
			return err
		}
	}
	var chain [][]byte
	switch o.Status {
	case acme.StatusReady:
		pair, e := m.getCertificate(nil)
		if e != nil {
			return e
		}
		signer, ok := pair.PrivateKey.(crypto.Signer)
		if !ok {
			return errors.New("TLS key cannot sign CSR")
		}
		csr, e := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: []string{m.c.Domain}}, signer)
		if e != nil {
			return errors.New("cannot generate CSR with existing TLS key")
		}
		chain, _, err = ca.CreateCertFromOrder(ctx, o, csr, true)
		if err != nil {
			return errors.New("certificate finalization/download interrupted; retry complete (valid orders are fetched, not re-finalized)")
		}
		// Refresh/persist the valid order before committing its certificate locally.
		o, err = ca.GetOrder(ctx, o.URI)
		if err != nil {
			return errors.New("cannot persist issued order; retry complete to fetch it")
		}
		s, err = m.updateOrder(s, o)
		if err != nil {
			return err
		}
		if o.Status != acme.StatusValid {
			return errors.New("CA has not marked the finalized order valid; retry complete")
		}
	case acme.StatusValid:
		if !m.validCAURL(o.CertURL) {
			return errors.New("valid order has no safe certificate URL")
		}
		chain, err = ca.FetchCert(ctx, o.CertURL, true)
		if err != nil {
			return errors.New("cannot download issued certificate; retry complete")
		}
	default:
		return errors.New("order is invalid; prepare a new order")
	}
	var b []byte
	for _, der := range chain {
		b = append(b, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	// First preserve the downloaded chain. A failed trust/hostname/expiry check
	// leaves evidence for retry, NOT permission to install. Install revalidates.
	if err = atomicWrite(filepath.Join(m.c.Directory, "pending-fullchain.pem"), b); err != nil {
		return errors.New("cannot persist pending fullchain")
	}
	m.mu.Lock()
	m.pending = true
	m.mu.Unlock()
	s.PendingHash = digest(b)
	if err = m.save(s); err != nil {
		return err
	}
	leaf, err := m.validateCertificate(b, true)
	if err != nil {
		return err
	}
	if m.c.CADirectory == staging {
		return errors.New("staging issuance saved for inspection only; staging certificates cannot be installed")
	}
	s.IssuedNotAfter = leaf.NotAfter
	return m.save(s)
}

func (m *manager) hasPending() bool { m.mu.RLock(); defer m.mu.RUnlock(); return m.pending }

// Called only through start (or before serving on startup). Abandoning is a
// durable, explicit confirmation to discard local order/pending data, not an
// ACME cancellation or revocation. Keep the account and issuance rate guard.
func (m *manager) abandon(context.Context) error {
	s := m.snapshot()
	if s.Installing {
		return errors.New("interrupted install must be recovered using install first; cannot abandon")
	}
	if !s.Abandoning {
		s.Abandoning = true
		if err := m.save(s); err != nil {
			return err
		}
	}
	return m.finishAbandon()
}

func (m *manager) finishAbandon() error {
	s := m.snapshot()
	if !s.Abandoning || s.Installing {
		return errors.New("no safe confirmed abandon to finish")
	}
	p := filepath.Join(m.c.Directory, "pending-fullchain.pem")
	if info, err := os.Lstat(p); err == nil {
		// Never follow a link or remove a directory (even an empty one).
		if !info.Mode().IsRegular() {
			return errors.New("cannot abandon non-regular pending file; cleanup remains pending")
		}
		if err := os.Remove(p); err != nil {
			return errors.New("cannot remove abandoned pending certificate; retry abandon")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("cannot inspect abandoned pending certificate; retry abandon")
	}
	// Unlink MUST be durable before committing the reset, otherwise a power
	// loss could resurrect an orphan pending file with no recovery marker.
	if runtime.GOOS != "windows" {
		d, err := os.Open(m.c.Directory)
		if err != nil {
			return errors.New("cannot open state directory for abandon sync")
		}
		err = d.Sync()
		ce := d.Close()
		if err != nil || ce != nil {
			return errors.New("cannot sync abandoned pending cleanup; retry abandon")
		}
	}
	m.mu.Lock()
	m.pending = false
	m.mu.Unlock()
	s.Order = nil
	s.PendingHash = ""
	s.Abandoning = false
	return m.save(s)
}

// Rollback always has its own timeout, even if the primary task timed out.
func (m *manager) rollback(s diskState) error {
	b, err := readFile(filepath.Join(m.c.Directory, "previous-fullchain.pem"), true)
	if err != nil {
		return errors.New("ROLLBACK FAILED: cannot read persistent previous fullchain; manual recovery required")
	}
	if _, err = tls.X509KeyPair(b, mustKeyBytes(m.c.PrivateKey)); err != nil {
		return errors.New("ROLLBACK FAILED: backup does not match current TLS key")
	}
	if atomicWrite(m.c.Certificate, b) != nil {
		return errors.New("ROLLBACK FAILED: cannot restore previous fullchain; manual recovery required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if m.reload(ctx) != nil {
		return errors.New("ROLLBACK FAILED: old fullchain restored on disk but reload failed; manual restart required")
	}
	s.Installing = false
	if m.save(s) != nil {
		return errors.New("ROLLBACK FAILED: old fullchain reloaded but recovery state could not be saved")
	}
	return nil
}

func (m *manager) install(ctx context.Context) error {
	s := m.snapshot()
	if s.Abandoning {
		return errors.New("interrupted abandon must be finished using abandon first")
	}
	if s.Installing {
		if err := m.rollback(s); err != nil {
			return err
		}
		return errors.New("interrupted install rolled back safely; retry install explicitly")
	}
	if m.c.CADirectory != production {
		return errors.New("staging certificates cannot be installed")
	}
	if !m.hasPending() || s.Order == nil || s.Order.Status != acme.StatusValid {
		return errors.New("no issued pending certificate to install")
	}
	b, err := readFile(filepath.Join(m.c.Directory, "pending-fullchain.pem"), true)
	if err != nil {
		return errors.New("cannot read pending fullchain")
	}
	if s.PendingHash != "" && digest(b) != s.PendingHash {
		return errors.New("pending fullchain changed unexpectedly; refusing installation")
	}
	leaf, err := m.validateCertificate(b, true)
	if err != nil {
		return err
	}
	old, err := readFile(m.c.Certificate, false)
	if err != nil {
		return errors.New("cannot back up current fullchain")
	}
	if atomicWrite(filepath.Join(m.c.Directory, "previous-fullchain.pem"), old) != nil {
		return errors.New("cannot persist previous fullchain; installation stopped")
	}
	s.Installing = true
	if err = m.save(s); err != nil {
		return err
	}
	if atomicWrite(m.c.Certificate, b) != nil {
		err = errors.New("cannot replace gateway fullchain")
	} else if m.reload(ctx) != nil {
		err = errors.New("gateway reload failed")
	} else if m.verifyGateway(ctx, leaf) != nil {
		err = errors.New("gateway verification failed: trusted TLS with the expected new leaf was not observed")
	}
	if err != nil {
		if re := m.rollback(s); re != nil {
			return fmt.Errorf("%s; %s", err, re)
		}
		return fmt.Errorf("%s; previous fullchain restored and reloaded; pending kept for retry", err)
	}
	s.Installing = false
	s.Order.Status = "installed"
	s.PendingHash = ""
	s.IssuedNotAfter = leaf.NotAfter
	if err = m.save(s); err != nil {
		// A failed commit is a failed install, not success with lost recovery data.
		rs := m.snapshot()
		rs.Order.Status = acme.StatusValid
		if re := m.rollback(rs); re != nil {
			return fmt.Errorf("install state commit failed; %s", re)
		}
		return errors.New("install state commit failed; previous fullchain restored; pending retained")
	}
	if err = os.Remove(filepath.Join(m.c.Directory, "pending-fullchain.pem")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("certificate installed but pending file cleanup failed; restart for cleanup")
	}
	m.mu.Lock()
	m.pending = false
	m.mu.Unlock()
	return nil
}

func (m *manager) start(work func(context.Context) error) bool {
	m.mu.Lock()
	if m.busy {
		m.mu.Unlock()
		return false
	}
	m.busy = true
	m.message = "Operation running; closing the browser does not cancel it."
	m.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		err := work(ctx)
		m.mu.Lock()
		defer m.mu.Unlock()
		m.busy = false
		if err != nil {
			m.message = err.Error()
		} else {
			m.message = "Operation completed. Review state; installation is never automatic."
		}
	}()
	return true
}

func jsonResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func apiError(w http.ResponseWriter, status int, message string) {
	jsonResponse(w, status, map[string]string{"error": message})
}

func (m *manager) authenticated(w http.ResponseWriter, r *http.Request) bool {
	// Compare before throttling so invalid requests cannot lock out the admin.
	u, p, ok := r.BasicAuth()
	uh, ph := sha256.Sum256([]byte(u)), sha256.Sum256([]byte(p))
	admin := sha256.Sum256([]byte("admin"))
	matched := subtle.ConstantTimeCompare(uh[:], admin[:]) & subtle.ConstantTimeCompare(ph[:], m.passwordHash[:])
	if ok && len(r.Header.Values("Authorization")) == 1 && matched == 1 {
		return true
	}
	// A global fixed window bounds invalid attempts, independent of spoofed IPs.
	// Ten failures per minute; no per-client map that can exhaust RAM.
	m.authMu.Lock()
	defer m.authMu.Unlock()
	if time.Since(m.authWindow) >= time.Minute {
		m.authWindow = time.Now()
		m.authFailures = 0
	}
	if m.authFailures >= 10 {
		w.Header().Set("Retry-After", "60")
		apiError(w, 429, "authentication temporarily throttled")
		return false
	}
	m.authFailures++
	w.Header().Set("WWW-Authenticate", `Basic realm="cert-manager", charset="UTF-8"`)
	apiError(w, 401, "authentication required")
	return false
}

func (m *manager) status(w http.ResponseWriter) {
	m.mu.RLock()
	s := cloneState(m.state)
	busy, msg, terms, pending := m.busy, m.message, m.terms, m.pending
	m.mu.RUnlock()
	var order any
	if s.Order != nil {
		order = struct {
			Status  string    `json:"status"`
			Expires time.Time `json:"expires"`
			Records []record  `json:"records"`
		}{s.Order.Status, s.Order.Expires, s.Order.Records}
	}
	l, err := m.currentLeaf()
	if err != nil {
		apiError(w, 503, "cannot read current TLS certificate")
		return
	}
	days := int(math.Floor(time.Until(l.NotAfter).Hours() / 24))
	jsonResponse(w, 200, struct {
		Domain             string    `json:"domain"`
		NotAfter           time.Time `json:"notAfter"`
		DaysRemaining      int       `json:"daysRemaining"`
		RenewRecommended   bool      `json:"renewRecommended"`
		Busy               bool      `json:"busy"`
		Message            string    `json:"message"`
		TermsURL           string    `json:"termsURL"`
		Order              any       `json:"order"`
		PendingCertificate bool      `json:"pendingCertificate"`
	}{m.c.Domain, l.NotAfter, days, time.Until(l.NotAfter) <= 35*24*time.Hour, busy, msg, terms, order, pending})
}

func (m *manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; object-src 'none'; base-uri 'none'; frame-src 'none'; frame-ancestors 'none'; form-action 'none'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	headerSize := len(r.Host) + len(r.RequestURI)
	for name, values := range r.Header {
		for _, v := range values {
			headerSize += len(name) + len(v) + 4
		}
	}
	if headerSize > 8<<10 {
		apiError(w, 431, "request headers too large")
		return
	}
	authority := strings.TrimPrefix(m.c.Origin, "https://")
	if r.Host != authority {
		apiError(w, 403, "Host is not the configured origin authority")
		return
	}
	if origins := r.Header.Values("Origin"); len(origins) > 1 || len(origins) == 1 && origins[0] != m.c.Origin {
		apiError(w, 403, "Origin mismatch")
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			apiError(w, 405, "method not allowed")
			return
		}
		name := map[string]string{"/": "web/index.html", "/index.html": "web/index.html", "/styles.css": "web/styles.css", "/app.js": "web/app.js"}[r.URL.Path]
		if name == "" {
			http.NotFound(w, r)
			return
		}
		b, err := webFiles.ReadFile(name)
		if err != nil {
			apiError(w, 503, "management UI assets are not present in this build")
			return
		}
		w.Header().Set("Content-Type", mime.TypeByExtension(filepath.Ext(name)))
		w.WriteHeader(200)
		if r.Method != http.MethodHead {
			_, _ = w.Write(b)
		}
		return
	}
	if !m.authenticated(w, r) {
		return
	}
	wanted := map[string]string{"/api/status": "GET", "/api/terms": "GET", "/api/prepare": "POST", "/api/complete": "POST", "/api/install": "POST", "/api/abandon": "POST"}[r.URL.Path]
	if wanted == "" {
		apiError(w, 404, "unknown API")
		return
	}
	if r.Method != wanted {
		w.Header().Set("Allow", wanted)
		apiError(w, 405, "method not allowed")
		return
	}
	if wanted == "GET" {
		if r.URL.Path == "/api/status" {
			m.status(w)
			return
		}
		select {
		case m.termsGate <- struct{}{}:
			defer func() { <-m.termsGate }()
		default:
			apiError(w, 409, "terms discovery already running")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		t, err := m.discover(ctx)
		if err != nil {
			apiError(w, 502, err.Error())
			return
		}
		m.mu.Lock()
		m.terms = t
		m.termsAt = time.Now()
		m.mu.Unlock()
		jsonResponse(w, 200, map[string]string{"termsURL": t})
		return
	}
	if len(r.Header.Values("Origin")) != 1 || r.Header.Get("Origin") != m.c.Origin || len(r.Header.Values("X-Cert-Manager")) != 1 || r.Header.Get("X-Cert-Manager") != "1" {
		apiError(w, 403, "POST requires exact Origin and X-Cert-Manager: 1")
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" || len(r.Header.Values("Content-Type")) != 1 {
		apiError(w, 415, "Content-Type must be application/json")
		return
	}
	if r.ContentLength > bodyLimit {
		apiError(w, 413, "request body too large")
		return
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, bodyLimit))
	if err != nil {
		apiError(w, 413, "request body too large or unreadable")
		return
	}
	if len(bytes.TrimSpace(b)) == 0 || bytes.TrimSpace(b)[0] != '{' {
		apiError(w, 400, "expected a JSON object")
		return
	}
	var work func(context.Context) error
	switch r.URL.Path {
	case "/api/prepare":
		var v struct {
			TermsURL    string `json:"termsURL"`
			AcceptTerms bool   `json:"acceptTerms"`
		}
		if decodeJSON(b, &v) != nil {
			apiError(w, 400, "invalid prepare JSON")
			return
		}
		m.mu.RLock()
		t, at := m.terms, m.termsAt
		m.mu.RUnlock()
		if !v.AcceptTerms || t == "" || v.TermsURL != t || time.Since(at) > 15*time.Minute {
			apiError(w, 400, "GET /api/terms first, then explicitly accept its current termsURL (within 15 minutes)")
			return
		}
		if m.recentlyIssued() {
			apiError(w, 409, "certificate has more than 35 days remaining; renewal blocked")
			return
		}
		work = func(ctx context.Context) error { return m.prepare(ctx, v.TermsURL) }
	case "/api/complete":
		if decodeJSON(b, &struct{}{}) != nil {
			apiError(w, 400, "complete expects {}")
			return
		}
		work = m.complete
	case "/api/install":
		var v struct {
			ConfirmRestart bool `json:"confirmRestart"`
		}
		if decodeJSON(b, &v) != nil || !v.ConfirmRestart {
			apiError(w, 400, "install requires confirmRestart:true")
			return
		}
		work = m.install
	case "/api/abandon":
		var v struct {
			ConfirmAbandon bool `json:"confirmAbandon"`
		}
		if decodeJSON(b, &v) != nil || !v.ConfirmAbandon {
			apiError(w, 400, "abandon requires confirmAbandon:true")
			return
		}
		if m.snapshot().Installing {
			apiError(w, 409, "interrupted install must be recovered using install first; cannot abandon")
			return
		}
		work = m.abandon
	}
	if !m.start(work) {
		apiError(w, 409, "another write operation is busy")
		return
	}
	jsonResponse(w, 202, map[string]string{"message": "operation accepted; poll /api/status"})
}

func run() error {
	path := flag.String("config", "", "absolute path to JSON configuration")
	init := flag.Bool("init-password", false, "create admin-password only; print its path, never its contents")
	check := flag.Bool("check", false, "validate configuration and existing TLS locally; no ACME network or issuance")
	flag.Parse()
	if flag.NArg() != 0 || *path == "" || !filepath.IsAbs(*path) || *init && *check {
		return errors.New("use -config ABSOLUTE_JSON_PATH with at most one of -init-password or -check")
	}
	b, err := readFile(*path, false)
	if err != nil {
		return errors.New("cannot read configuration")
	}
	var c config
	if decodeJSON(b, &c) != nil {
		return errors.New("invalid configuration JSON")
	}
	if err = c.validate(); err != nil {
		return err
	}
	if *init {
		return initPassword(c)
	}
	if *check {
		m := &manager{c: c}
		if _, err = m.getCertificate(nil); err != nil {
			return err
		}
		fmt.Println("Configuration and current TLS load OK; no network requests made. Renew before expiry.")
		return nil
	}
	m, err := newManager(c)
	if err != nil {
		return err
	}
	if _, err = m.getCertificate(nil); err != nil {
		return err
	}
	srv := &http.Server{Addr: c.Listen, Handler: m, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: m.getCertificate}, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 40 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8 << 10, ErrorLog: log.New(io.Discard, "", 0)}
	// Disable HTTP/2 so its independent stream/header limits cannot bypass the
	// deliberately small HTTP/1 server resource budget on a headless device.
	srv.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){}
	return srv.ListenAndServeTLS("", "")
}

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}
