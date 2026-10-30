package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestCheckIsReadOnlyAndInvalidConfigFails(t *testing.T) {
	var out, stderr bytes.Buffer
	if code := run([]string{"-check", "-config", "config.example.json"}, &out, &stderr); code != 0 {
		t.Fatalf("-check failed: %d %s", code, stderr.String())
	}
	dir := t.TempDir()
	filename := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(filename, []byte(`{"domain":"a.test;id"}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"-check", "-config", filename},
		{"-check", "-once", "-config", "config.example.json"},
		{"-config", "does-not-exist.json"},
		{"-check", "-config", "config.example.json", "unexpected"},
		{"-unknown"},
	} {
		if code := run(args, &out, &stderr); code != 2 {
			t.Fatalf("usage/config exit = %d, want 2", code)
		}
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatal("-check unexpectedly created state files")
	}
}