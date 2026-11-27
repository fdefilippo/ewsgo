// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"ewsgo/internal/service"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(os.Args) > 1 && os.Args[1] == "store" {
		os.Exit(runStoreCommand(ctx, os.Args[2:], os.Stdout, os.Stderr))
	}

	flags := flag.NewFlagSet(os.Args[0], flag.ExitOnError)
	cfg, err := parseConfig(flags, os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	logger, err := service.NewLogger(os.Stderr, cfg.LogLevel)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := service.Run(ctx, cfg, logger); err != nil {
		logger.Error("service stopped", "error", err)
		os.Exit(1)
	}
}

func parseConfig(flags *flag.FlagSet, args []string) (service.Config, error) {
	cfg := service.Config{}
	configPath := flags.String("config", "", "YAML configuration file; command-line flags override file settings")
	flags.StringVar(&cfg.LogLevel, "loglevel", "info", "cumulative JSON log threshold: none, err/error, warn, info, debug")
	flags.StringVar(&cfg.LogLevel, "log-level", "info", "alias for -loglevel")
	flags.StringVar(&cfg.Listen, "listen", "127.0.0.1:8080", "HTTP listen address; public listeners require TLS")
	flags.StringVar(&cfg.CertFile, "tls-cert", "", "PEM server certificate path")
	flags.StringVar(&cfg.KeyFile, "tls-key", "", "PEM server private key path")
	flags.StringVar(&cfg.ZimbraOrigin, "zimbra-origin", "", "HTTPS backend origin; requires frontend TLS")
	flags.StringVar(&cfg.StorePath, "store-path", "", "SQLite file for persistent EWS mailbox projections and synchronization")
	flags.BoolVar(&cfg.MAPISpike, "mapi-spike", false, "enable diagnostic MAPI decoding/native access; no mailbox ROP execution")
	flags.BoolVar(&cfg.MAPISessionSpike, "mapi-session-spike", false, "enable ephemeral MAPI/HTTP contexts and Connect; mailbox ROPs require store spike")
	flags.BoolVar(&cfg.MAPIStoreSpike, "mapi-store-spike", false, "enable laboratory own-mailbox store ROPs; requires session spike")
	flags.StringVar(&cfg.MAPIPublicZimbraFolder, "mapi-public-zimbra-folder", "", "optional dedicated Zimbra public root, as ACCOUNT-UUID:FOLDER-ID")
	flags.BoolVar(&cfg.MAPIPersistentReplica, "mapi-persistent-replica", false, "retain private MAPI identities and completed synchronization state in SQLite; requires store-path and mapi-store-spike")
	flags.BoolVar(&cfg.MAPIFAICollectorSpike, "mapi-fai-collector-spike", false, "enable bounded RAM-only Outlook FAI imports and hierarchy state contexts; requires Contents download spike; state lost on restart")
	flags.BoolVar(&cfg.MAPIHierarchyImport, "mapi-hierarchy-import", false, "enable native ICS create, rename and move of mail folders; requires laboratory hierarchy upload collector")
	flags.BoolVar(&cfg.MAPIFolderAggregates, "mapi-folder-aggregates", false, "observe mail folder commit time and verified removals in laboratory RAM; requires Contents download")
	flags.BoolVar(&cfg.MAPIReadStateImport, "mapi-read-state-import", false, "enable native message read/unread updates through MAPI ICS; requires laboratory Contents upload collector")
	flags.BoolVar(&cfg.MAPIContentsDownloadSpike, "mapi-contents-download-spike", false, "enable bounded readonly mail and RAM FAI Contents download in the laboratory; requires hierarchy download spike; state lost on restart")
	flags.BoolVar(&cfg.MAPIContentsTableSpike, "mapi-contents-table-spike", false, "enable bounded synchronous readonly Contents and Hierarchy table snapshots without notifications; requires laboratory RAM store and Contents download; lost on restart")
	flags.BoolVar(&cfg.MAPIHierarchyDownloadSpike, "mapi-hierarchy-download-spike", false, "enable bounded read-only hierarchy ICS download in the laboratory; requires outlook-lab and mapi-store-spike")
	flags.BoolVar(&cfg.MAPIUnknownPropertyTrace, "mapi-unknown-property-trace", false, "temporarily trace bounded unknown low property IDs/types; requires outlook-lab and mapi-store-spike; never records values")
	flags.BoolVar(&cfg.MAPIOSTPropertySpike, "mapi-ost-property-spike", false, "enable bounded OSTID property values on own-mailbox folders; requires outlook-lab and mapi-store-spike")
	flags.BoolVar(&cfg.MAPISetListTrace, "mapi-set-list-trace", false, "temporarily trace low technical tags in bounded scalar SetProperties lists; requires header tracing and laboratory RAM store; never records values or executes additional writes")
	flags.BoolVar(&cfg.MAPISetPropertyTrace, "mapi-set-property-trace", false, "temporarily trace only a SetProperties header count and first low property ID/type; requires outlook-lab and mapi-store-spike; never records values")
	flags.BoolVar(&cfg.MAPISynchronizationTrace, "mapi-synchronization-trace", false, "temporarily trace only a closed SynchronizationConfigure type; requires outlook-lab and mapi-store-spike; never records flags, restrictions, tags or values")
	flags.BoolVar(&cfg.MAPISynchronizationOptionsTrace, "mapi-synchronization-options-trace", false, "temporarily trace closed Configure protocol option names and filter presence/count buckets; requires type tracing and hierarchy download laboratory spike; never records tags, restrictions or identities")
	flags.BoolVar(&cfg.MAPICalendarReferenceSetTrace, "mapi-calendar-reference-set-trace", false, "trace only bounded canonical Calendar Set outcomes; requires authenticated laboratory Calendar metadata spike; never records EntryIDs, values, targets or error codes")
	flags.BoolVar(&cfg.MAPIDraftsReferenceSpike, "mapi-drafts-reference-spike", false, "enable canonical EntryID reference to own existing Drafts mail folder; requires laboratory RAM store and hierarchy; no native writes")
	flags.BoolVar(&cfg.MAPITasksMetadataSpike, "mapi-tasks-metadata-spike", false, "enable own native Tasks metadata and resolvable RAM EntryID; requires laboratory RAM store and hierarchy; no task objects or native writes")
	flags.BoolVar(&cfg.MAPIContactsMetadataSpike, "mapi-contacts-metadata-spike", false, "enable the own native Contacts folder in Outlook lab; Contents Table and Download enable card reads and cache download, while FAI Collector enables online create and edit")
	flags.BoolVar(&cfg.MAPICalendarMetadataSpike, "mapi-calendar-metadata-spike", false, "enable own native Calendar metadata, EntryID and appointment reads where supported; requires laboratory store and hierarchy")
	flags.BoolVar(&cfg.MAPIFolderMetadataSpike, "mapi-folder-metadata-spike", false, "enable a closed bounded RAM folder metadata bag; requires laboratory RAM store and hierarchy; never writes Zimbra or implements search/calendar semantics")
	flags.BoolVar(&cfg.MAPICreateFolderSpike, "mapi-create-folder-spike", false, "enable bounded generic Unicode folder creation in the laboratory store; requires hierarchy download; never writes Zimbra")
	flags.BoolVar(&cfg.MAPICreateFolderTrace, "mapi-create-folder-trace", false, "temporarily trace closed CreateFolder fixed-header options; requires Outlook laboratory and RAM store; never creates folders or records names/comments/handles")
	flags.BoolVar(&cfg.NSPIBindSpike, "nspi-bind-spike", false, "enable authenticated NSPI Bind contexts; no GAL or recipient resolution")
	flags.BoolVar(&cfg.NSPIDirectorySpike, "nspi-directory-spike", false, "enable bounded native GAL DNToMId/GetProps; no QueryRows or ANR")
	flags.BoolVar(&cfg.OutlookLab, "outlook-lab", false, "enable sanitized Outlook diagnostics at debug loglevel; requires TLS")
	flags.BoolVar(&cfg.OutlookProfileSpike, "outlook-profile-spike", false, "advertise experimental MAPI/HTTP endpoints; mailbox ROPs require store spike")
	flags.StringVar(&cfg.PublicOrigin, "public-origin", "", "explicit HTTPS gateway origin for the Outlook profile spike")
	flags.StringVar(&cfg.DeploymentID, "deployment-id", "", "stable installation UUID for the Outlook profile spike; retain across restarts")
	if err := flags.Parse(args); err != nil {
		return cfg, err
	}
	if flags.NArg() != 0 {
		return cfg, fmt.Errorf("unexpected positional arguments")
	}
	if *configPath != "" {
		if err := loadYAMLConfig(flags, *configPath); err != nil {
			return cfg, fmt.Errorf("configuration %q: %w", *configPath, err)
		}
		// Replay explicit flags in order, including aliases and explicit false.
		if err := flags.Parse(args); err != nil {
			return cfg, err
		}
	}
	return cfg, nil
}
