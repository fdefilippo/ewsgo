// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package labconfig reads the user's private colon-delimited laboratory file.
package labconfig

import (
	"bufio"
	"errors"
	"os"
	"strings"
)

type Credentials struct{ URL, User, Password, DeniedMailbox string }

func (Credentials) String() string { return "laboratory credentials [redacted]" }
func Read(path string) (Credentials, error) {
	f, err := os.Open(path)
	if err != nil {
		return Credentials{}, errors.New("cannot open laboratory credentials")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return Credentials{}, errors.New("credentials must be a regular file readable only by its owner (0600)")
	}
	values := make(map[string]string)
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		key = strings.ToLower(strings.TrimSpace(key))
		if !ok {
			return Credentials{}, errors.New("invalid colon-delimited credential file")
		}
		switch key {
		case "url", "user", "pass", "denied_mailbox":
		default:
			return Credentials{}, errors.New("unknown credential field")
		}
		if _, exists := values[key]; exists {
			return Credentials{}, errors.New("duplicate credential field")
		}
		values[key] = strings.TrimSpace(value)
	}
	if scan.Err() != nil {
		return Credentials{}, errors.New("cannot read laboratory credentials")
	}
	c := Credentials{URL: values["url"], User: values["user"], Password: values["pass"], DeniedMailbox: values["denied_mailbox"]}
	if c.URL == "" || c.User == "" || c.Password == "" {
		return Credentials{}, errors.New("missing url, user or pass credential field")
	}
	return c, nil
}
