package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"os"
	"path"
	"regexp"
	"strings"
)

type Config struct {
	Domain       string `json:"domain"`
	PublicIP     string `json:"public_ip"`
	GatewayIP    string `json:"gateway_ip"`
	Interface    string `json:"interface"`
	HTTPSPort    int    `json:"https_port"`
	UpstreamPort int    `json:"upstream_port"`
	PackageName  string `json:"package_name"`
	GatewayDir   string `json:"gateway_dir"`
	StateDir     string `json:"state_dir"`
}

var (
	safeSegment = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	safeIF      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,14}$`)
	domainLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
)

func (c Config) Validate() error {
	if len(c.Domain) > 253 || !strings.Contains(c.Domain, ".") {
		return errors.New("invalid domain")
	}
	for _, label := range strings.Split(c.Domain, ".") {
		if !domainLabel.MatchString(label) {
			return errors.New("invalid domain")
		}
	}
	if _, err := netip.ParseAddr(c.Domain); err == nil {
		return errors.New("domain must be a hostname")
	}
	for _, value := range []string{c.PublicIP, c.GatewayIP} {
		ip, err := netip.ParseAddr(value)
		if err != nil || !ip.Is4() || ip.String() != value || !ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			return errors.New("invalid unicast IPv4 address")
		}
	}
	if c.PublicIP == c.GatewayIP {
		return errors.New("public_ip and gateway_ip must differ")
	}
	if !safeIF.MatchString(c.Interface) {
		return errors.New("invalid interface")
	}
	if c.HTTPSPort < 1 || c.HTTPSPort > 65535 || c.UpstreamPort < 1 || c.UpstreamPort > 65535 || c.HTTPSPort == c.UpstreamPort {
		return errors.New("invalid or conflicting ports")
	}
	if c.PackageName != "com.diplay.tesla" && c.PackageName != "com.diplay.tesla.debug" {
		return errors.New("unsupported package_name")
	}
	if !safeAndroidDir(c.GatewayDir) || !safeAndroidDir(c.StateDir) {
		return errors.New("directories must be simple absolute paths below /data/local")
	}
	return nil
}

func safeAndroidDir(value string) bool {
	if !strings.HasPrefix(value, "/data/local/") || path.Clean(value) != value || len(value) > 240 {
		return false
	}
	for _, part := range strings.Split(strings.TrimPrefix(value, "/data/local/"), "/") {
		if !safeSegment.MatchString(part) {
			return false
		}
	}
	return true
}

func LoadConfig(filename string) (Config, error) {
	f, err := os.Open(filename)
	if err != nil {
		return Config{}, errors.New("cannot open config")
	}
	defer f.Close()
	return decodeConfig(f)
}

func decodeConfig(r io.Reader) (Config, error) {
	// A token pass rejects duplicates, case aliases and non-snake-case keys,
	// which encoding/json otherwise accepts even with DisallowUnknownFields.
	data, err := io.ReadAll(io.LimitReader(r, 16385))
	if err != nil || len(data) > 16384 {
		return Config{}, errors.New("cannot read config or config too large")
	}
	d := json.NewDecoder(strings.NewReader(string(data)))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return Config{}, errors.New("config must be an object")
	}
	allowed := map[string]bool{"domain": true, "public_ip": true, "gateway_ip": true, "interface": true, "https_port": true, "upstream_port": true, "package_name": true, "gateway_dir": true, "state_dir": true}
	seen := make(map[string]bool)
	for d.More() {
		t, e := d.Token()
		key, ok := t.(string)
		if e != nil || !ok || !allowed[key] || seen[key] {
			return Config{}, errors.New("unknown or duplicate config field")
		}
		seen[key] = true
		var value json.RawMessage
		if d.Decode(&value) != nil || string(value) == "null" {
			return Config{}, errors.New("invalid config field")
		}
	}
	if _, err := d.Token(); err != nil {
		return Config{}, errors.New("invalid config object")
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF || len(seen) != len(allowed) {
		return Config{}, errors.New("missing fields or trailing config data")
	}
	var c Config
	if json.Unmarshal(data, &c) != nil {
		return Config{}, errors.New("invalid config types")
	}
	return c, c.Validate()
}