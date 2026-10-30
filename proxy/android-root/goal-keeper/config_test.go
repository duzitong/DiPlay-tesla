package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func sampleConfig() Config {
	return Config{Domain: "carplay.example.com", PublicIP: "203.0.113.10", GatewayIP: "192.168.0.1", Interface: "br0", HTTPSPort: 9443, UpstreamPort: 8081, PackageName: "com.diplay.tesla", GatewayDir: "/data/local/diplay-https", StateDir: "/data/local/diplay-goal-keeper"}
}

func TestExampleAndStrictConfig(t *testing.T) {
	c, err := LoadConfig("config.example.json")
	if err != nil || c != sampleConfig() {
		t.Fatalf("example invalid: %v", err)
	}
	valid, _ := os.ReadFile("config.example.json")
	if _, err := decodeConfig(strings.NewReader(string(valid))); err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{
		`null`, `[]`, `{}`, `{"domain":null}`,
		strings.Replace(string(valid), `"domain":`, `"Domain":`, 1),
		strings.Replace(string(valid), `"domain":`, `"extra":1,"domain":`, 1),
		strings.Replace(string(valid), `"domain":`, `"domain":"x.test","domain":`, 1),
		strings.Replace(string(valid), `9443`, `"9443"`, 1),
		strings.Replace(string(valid), `9443`, `null`, 1),
		strings.Replace(string(valid), `9443`, `9443.1`, 1),
		string(valid) + `{}`, string(valid) + `garbage`, strings.Repeat(" ", 16385),
	} {
		if _, err := decodeConfig(strings.NewReader(data)); err == nil {
			t.Errorf("accepted malformed configuration (length %d)", len(data))
		}
	}
}

func TestConfigRejectsInjectionAndEscapes(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Config)
	}{
		{"domain-shell", func(c *Config) { c.Domain = "a.test;id" }},
		{"domain-url", func(c *Config) { c.Domain = "https://a.test" }},
		{"domain-empty-label", func(c *Config) { c.Domain = "a..test" }},
		{"domain-ip", func(c *Config) { c.Domain = "192.168.0.1" }},
		{"domain-dash", func(c *Config) { c.Domain = "-a.test" }},
		{"domain-trailing", func(c *Config) { c.Domain = "a.test." }},
		{"domain-uppercase", func(c *Config) { c.Domain = "A.test" }},
		{"public-v6", func(c *Config) { c.PublicIP = "::1" }},
		{"public-mapped", func(c *Config) { c.PublicIP = "::ffff:203.0.113.1" }},
		{"public-shell", func(c *Config) { c.PublicIP = "203.0.113.1;id" }},
		{"public-zero", func(c *Config) { c.PublicIP = "0.0.0.0" }},
		{"public-multicast", func(c *Config) { c.PublicIP = "224.0.0.1" }},
		{"public-loopback", func(c *Config) { c.PublicIP = "127.0.0.1" }},
		{"gateway-invalid", func(c *Config) { c.GatewayIP = "192.168.0.1/24" }},
		{"equal-ip", func(c *Config) { c.PublicIP = c.GatewayIP }},
		{"interface-shell", func(c *Config) { c.Interface = "br0;id" }},
		{"interface-option", func(c *Config) { c.Interface = "--help" }},
		{"interface-long", func(c *Config) { c.Interface = strings.Repeat("b", 16) }},
		{"https-port", func(c *Config) { c.HTTPSPort = 65536 }},
		{"upstream-port", func(c *Config) { c.UpstreamPort = 0 }},
		{"equal-port", func(c *Config) { c.UpstreamPort = c.HTTPSPort }},
		{"other-package", func(c *Config) { c.PackageName = "com.other.app" }},
		{"package-shell", func(c *Config) { c.PackageName += ";id" }},
	}
	for _, field := range []string{"gateway", "state"} {
		for _, value := range []string{"/data/local", "/data/local/", "/data/local/../etc", "/data/local/a/../../x", "/data/local/a//b", "/data/local/a/./b", "/data/local/a/", "/data/local/a b", "/data/local/a;id", "/data/local/a\nb", "/data/local/a\\b", "/data/local/.hidden", "/data/locality/a", "data/local/a", "/tmp/a", "/data/local/" + strings.Repeat("x", 241)} {
			field, value := field, value
			cases = append(cases, struct {
				name string
				edit func(*Config)
			}{field + "-" + value, func(c *Config) {
				if field == "gateway" {
					c.GatewayDir = value
				} else {
					c.StateDir = value
				}
			}})
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := sampleConfig()
			tc.edit(&c)
			data, _ := json.Marshal(c)
			if _, err := decodeConfig(strings.NewReader(string(data))); err == nil {
				t.Fatal("accepted unsafe config")
			}
		})
	}
	c := sampleConfig()
	c.PackageName = "com.diplay.tesla.debug"
	if c.Validate() != nil {
		t.Fatal("debug may be observed, but must never autostart")
	}
}