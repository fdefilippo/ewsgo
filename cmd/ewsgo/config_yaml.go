// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"go.yaml.in/yaml/v3"
)

func loadYAMLConfig(flags *flag.FlagSet, path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := yaml.NewDecoder(file)
	var options map[string]yaml.Node
	if err := decoder.Decode(&options); err != nil {
		return err
	}
	if options == nil {
		return errors.New("expected a YAML mapping of gateway options")
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err == nil {
		return errors.New("expected a single YAML document")
	} else if err != io.EOF {
		return err
	}
	for name, node := range options {
		option := flags.Lookup(name)
		if option == nil || name == "config" || name == "log-level" {
			return fmt.Errorf("line %d: unknown option %q", node.Line, name)
		}
		tag, nameOfType := "!!str", "a string"
		if boolean, ok := option.Value.(interface{ IsBoolFlag() bool }); ok && boolean.IsBoolFlag() {
			tag, nameOfType = "!!bool", "true or false"
		}
		if node.Kind != yaml.ScalarNode || node.ShortTag() != tag {
			return fmt.Errorf("line %d: option %q requires %s", node.Line, name, nameOfType)
		}
		if err := flags.Set(name, node.Value); err != nil {
			return fmt.Errorf("line %d: %w", node.Line, err)
		}
	}
	return nil
}
