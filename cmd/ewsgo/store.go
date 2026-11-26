// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"ewsgo/internal/storage/sqlite"
)

func runStoreCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] != "backup" && args[0] != "restore" {
		fmt.Fprintln(stderr, "usage: ewsgo store backup|restore -from SOURCE -to NEW_DATABASE")
		return 2
	}
	flags := flag.NewFlagSet("ewsgo store "+args[0], flag.ContinueOnError)
	flags.SetOutput(stderr)
	from := flags.String("from", "", "source database file")
	to := flags.String("to", "", "new destination database file")
	if err := flags.Parse(args[1:]); errors.Is(err, flag.ErrHelp) {
		return 0
	} else if err != nil {
		return 2
	}
	if *from == "" || *to == "" || flags.NArg() != 0 {
		flags.Usage()
		return 2
	}
	action := sqlite.Backup
	if args[0] == "restore" {
		action = sqlite.Restore
	}
	if err := action(ctx, *from, *to); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintln(stdout, "Database copy created")
	return 0
}
