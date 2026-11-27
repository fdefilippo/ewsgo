// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestYAMLConfigCLIOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ewsgo.yaml")
	contents := `listen: '127.0.0.1:8443'
loglevel: warn
store-path: '/var/lib/ewsgo/ews data.db'
jmap-session-url: https://mail.example.org/.well-known/jmap
mapi-spike: true
`
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	flags := flag.NewFlagSet("ewsgo", flag.ContinueOnError)
	cfg, err := parseConfig(flags, []string{"-loglevel=info", "-config", path, "-log-level=err", "-mapi-spike=false"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:8443" || cfg.StorePath != "/var/lib/ewsgo/ews data.db" || cfg.MAPISpike || cfg.LogLevel != "err" ||
		cfg.JMAPSessionURL != "https://mail.example.org/.well-known/jmap" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestYAMLConfigRejectsInvalidOptions(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"null", "null\n", "expected a YAML mapping"},
		{"unknown", "store-dir: /var/lib/ewsgo\n", "unknown option"},
		{"wrong_type", "mapi-spike: 'false'\n", "requires true or false"},
		{"wrong_shape", "tls-key: [a, b]\n", "requires a string"},
		{"duplicate", "loglevel: debug\nloglevel: info\n", "already defined"},
		{"recursive", "config: /etc/ewsgo/other.yaml\n", "unknown option"},
		{"extra_document", "loglevel: info\n---\nloglevel: debug\n", "expected a single YAML document"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ewsgo.yaml")
			if err := os.WriteFile(path, []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := parseConfig(flag.NewFlagSet("ewsgo", flag.ContinueOnError), []string{"-config", path})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}
