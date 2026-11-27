// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package service owns process configuration and the HTTP server lifecycle.
// Protocol handlers and backend sessions are introduced by later spikes.
package service

import (
	"context"
	"crypto/tls"
	"errors"
	"ewsgo/internal/backend/jmap"
	"ewsgo/internal/backend/zimbra"
	"ewsgo/internal/core"
	"ewsgo/internal/frontend/autodiscover"
	"ewsgo/internal/frontend/ews"
	"ewsgo/internal/frontend/mapi"
	"ewsgo/internal/storage/sqlite"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	LogLevel                        string
	Listen                          string
	CertFile                        string
	KeyFile                         string
	ZimbraOrigin                    string
	JMAPSessionURL                  string
	StorePath                       string
	MAPISpike                       bool
	MAPISessionSpike                bool
	MAPIStoreSpike                  bool
	MAPIPersistentReplica           bool
	MAPIPublicZimbraFolder          string
	MAPIUnknownPropertyTrace        bool
	MAPISetPropertyTrace            bool
	MAPISetListTrace                bool
	MAPISynchronizationTrace        bool
	MAPISynchronizationOptionsTrace bool
	MAPICreateFolderTrace           bool
	MAPICreateFolderSpike           bool
	MAPIOSTPropertySpike            bool
	MAPIFolderMetadataSpike         bool
	MAPICalendarMetadataSpike       bool
	MAPIContactsMetadataSpike       bool
	MAPITasksMetadataSpike          bool
	MAPIDraftsReferenceSpike        bool
	MAPICalendarReferenceSetTrace   bool
	MAPIHierarchyDownloadSpike      bool
	MAPIHierarchyImport             bool
	MAPIFolderAggregates            bool
	MAPIContentsDownloadSpike       bool
	MAPIContentsTableSpike          bool
	MAPIFAICollectorSpike           bool
	MAPIReadStateImport             bool
	NSPIBindSpike                   bool
	NSPIDirectorySpike              bool
	OutlookLab                      bool
	OutlookProfileSpike             bool
	PublicOrigin                    string
	DeploymentID                    string
}

// Validate permits plaintext only on a literal loopback address. Hostnames
// cannot turn a DNS change into an unintended public plaintext listener.
func (c Config) Validate() error {
	if _, err := ParseLogLevel(c.LogLevel); err != nil {
		return err
	}
	if c.StorePath != "" && c.ZimbraOrigin == "" && c.JMAPSessionURL == "" {
		return errors.New("persistent store requires a backend origin")
	}
	if c.JMAPSessionURL != "" {
		if c.ZimbraOrigin != "" || c.StorePath == "" || c.CertFile == "" || c.KeyFile == "" || c.OutlookLab {
			return errors.New("JMAP requires SQLite and frontend TLS, and is exclusive with Zimbra and the Outlook lab")
		}
		if _, err := jmap.New(c.JMAPSessionURL, nil); err != nil {
			return errors.New("jmap-session-url must be an HTTPS Session URL")
		}
	}
	if c.MAPIPersistentReplica && (!c.MAPIStoreSpike || c.StorePath == "") {
		return errors.New("persistent MAPI replica requires the laboratory store and a SQLite store path")
	}
	if c.MAPIPublicZimbraFolder != "" && (!c.MAPIStoreSpike || c.StorePath == "") {
		return errors.New("MAPI public Zimbra store requires MAPI mailbox execution and a SQLite store path")
	}
	if c.MAPIReadStateImport && (!c.MAPIFAICollectorSpike || !c.MAPIContentsDownloadSpike || !c.OutlookLab || !c.MAPIStoreSpike) {
		return errors.New("MAPI read-state import requires laboratory RAM store, Contents download and upload collector")
	}
	if c.MAPIHierarchyImport && !c.MAPIFAICollectorSpike {
		return errors.New("MAPI hierarchy import requires the laboratory hierarchy upload collector")
	}
	if c.MAPIFolderAggregates && !c.MAPIContentsDownloadSpike {
		return errors.New("MAPI folder aggregates require laboratory Contents download")
	}
	if c.MAPIContentsTableSpike && (!c.OutlookLab || !c.MAPIStoreSpike || !c.MAPIContentsDownloadSpike) {
		return errors.New("MAPI Contents table requires laboratory RAM store and Contents download spike")
	}
	if c.MAPIFAICollectorSpike && (!c.OutlookLab || !c.MAPIStoreSpike || !c.MAPIContentsDownloadSpike) {
		return errors.New("MAPI FAI collector requires laboratory RAM store and Contents download spike")
	}
	if c.MAPIContentsDownloadSpike && (!c.OutlookLab || !c.MAPIStoreSpike || !c.MAPIHierarchyDownloadSpike) {
		return errors.New("MAPI Contents download requires Outlook laboratory, RAM store and hierarchy download spike")
	}
	if c.MAPICalendarReferenceSetTrace && (!c.OutlookLab || !c.MAPIStoreSpike || !c.MAPICalendarMetadataSpike) {
		return errors.New("MAPI Calendar Set outcome tracing requires laboratory RAM store and native Calendar metadata spike")
	}
	if c.MAPIDraftsReferenceSpike && (!c.OutlookLab || !c.MAPIStoreSpike || !c.MAPIHierarchyDownloadSpike) {
		return errors.New("MAPI Drafts reference requires Outlook laboratory, RAM store and hierarchy download spike")
	}
	if c.MAPITasksMetadataSpike && (!c.OutlookLab || !c.MAPIStoreSpike || !c.MAPIHierarchyDownloadSpike) {
		return errors.New("MAPI Tasks metadata requires Outlook laboratory, RAM store and hierarchy download spike")
	}
	if c.MAPIContactsMetadataSpike && (!c.OutlookLab || !c.MAPIStoreSpike || !c.MAPIHierarchyDownloadSpike) {
		return errors.New("MAPI Contacts metadata requires Outlook laboratory, RAM store and hierarchy download spike")
	}
	if c.MAPICalendarMetadataSpike && (!c.OutlookLab || !c.MAPIStoreSpike || !c.MAPIHierarchyDownloadSpike) {
		return errors.New("MAPI Calendar metadata requires Outlook laboratory, RAM store and hierarchy download spike")
	}
	if c.MAPIFolderMetadataSpike && (!c.OutlookLab || !c.MAPIStoreSpike || !c.MAPIHierarchyDownloadSpike) {
		return errors.New("MAPI folder metadata requires Outlook laboratory, RAM store and hierarchy download spike")
	}
	if c.MAPISetListTrace && (!c.MAPISetPropertyTrace || !c.OutlookLab || !c.MAPIStoreSpike) {
		return errors.New("SetProperties list tracing requires header tracing and authenticated laboratory RAM store")
	}
	if c.MAPICreateFolderSpike && (!c.OutlookLab || !c.MAPIStoreSpike || !c.MAPIHierarchyDownloadSpike) {
		return errors.New("MAPI RAM CreateFolder requires Outlook laboratory, RAM store and hierarchy download spike")
	}
	if c.MAPICreateFolderTrace && (!c.OutlookLab || !c.MAPIStoreSpike) {
		return errors.New("MAPI CreateFolder tracing requires Outlook laboratory and the MAPI RAM store spike")
	}
	if c.MAPISynchronizationOptionsTrace && (!c.MAPISynchronizationTrace || !c.MAPIHierarchyDownloadSpike) {
		return errors.New("MAPI synchronization options tracing requires type tracing and the hierarchy download laboratory spike")
	}
	if c.MAPIHierarchyDownloadSpike && (!c.OutlookLab || !c.MAPIStoreSpike) {
		return errors.New("MAPI hierarchy download requires Outlook laboratory and the MAPI RAM store spike")
	}
	if c.MAPISynchronizationTrace && (!c.OutlookLab || !c.MAPIStoreSpike) {
		return errors.New("MAPI synchronization type tracing requires Outlook laboratory tracing and the MAPI store spike")
	}
	if c.MAPIOSTPropertySpike && (!c.OutlookLab || !c.MAPIStoreSpike) {
		return errors.New("MAPI OST property spike requires Outlook laboratory and the MAPI RAM store spike")
	}
	if c.MAPISetPropertyTrace && (!c.OutlookLab || !c.MAPIStoreSpike) {
		return errors.New("MAPI SetProperties tracing requires Outlook laboratory tracing and the MAPI store spike")
	}
	if c.MAPIUnknownPropertyTrace && (!c.OutlookLab || !c.MAPIStoreSpike) {
		return errors.New("unknown MAPI property tracing requires Outlook laboratory tracing and the MAPI store spike")
	}
	if c.MAPIStoreSpike && !c.MAPISessionSpike {
		return errors.New("MAPI store spike requires the MAPI session spike")
	}
	if c.NSPIDirectorySpike && !c.NSPIBindSpike {
		return errors.New("NSPI directory spike requires the NSPI Bind spike")
	}
	if c.NSPIBindSpike && !c.MAPISessionSpike {
		return errors.New("NSPI Bind spike requires the MAPI session spike")
	}
	if c.MAPISessionSpike && !c.OutlookProfileSpike {
		return errors.New("MAPI session spike requires the Outlook profile spike")
	}
	if c.OutlookProfileSpike {
		if !c.OutlookLab || !c.MAPISpike || c.ZimbraOrigin == "" {
			return errors.New("Outlook profile spike requires laboratory tracing and MAPI/backend spikes")
		}
		namespace, err := mapi.NewNamespace(c.DeploymentID)
		if err != nil {
			return err
		}
		if _, err := autodiscover.NewProfile(c.PublicOrigin, namespace); err != nil {
			return err
		}
	} else if c.PublicOrigin != "" || c.DeploymentID != "" {
		return errors.New("public origin and deployment ID require Outlook profile spike")
	}
	if c.OutlookLab && (c.CertFile == "" || c.KeyFile == "") {
		return errors.New("Outlook laboratory tracing requires TLS")
	}
	if c.MAPISpike && c.ZimbraOrigin == "" {
		return errors.New("MAPI spike requires an authenticated backend")
	}
	host, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return errors.New("listen must be an IP address and port")
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return errors.New("listen host must be a literal IP address")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return errors.New("listen port must be between 0 and 65535")
	}
	if (c.CertFile == "") != (c.KeyFile == "") {
		return errors.New("TLS requires both certificate and key")
	}
	if !ip.IsLoopback() && c.CertFile == "" {
		return errors.New("non-loopback listeners require TLS")
	}
	return nil
}

// Handler exposes process liveness only. Readiness remains false until an
// actual protocol and backend have been wired and qualified.
func Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{\"status\":\"alive\"}\n"))
	})
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("{\"status\":\"not_ready\"}\n"))
	})
	return mux
}

func Run(ctx context.Context, cfg Config, logger *slog.Logger) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	threshold, _ := ParseLogLevel(cfg.LogLevel)
	logger = slog.New(thresholdHandler{logger.Handler(), threshold})
	handler := Handler()
	if cfg.ZimbraOrigin != "" {
		if cfg.CertFile == "" {
			return errors.New("backend authentication requires frontend TLS")
		}
		backend, err := zimbra.New(zimbra.Config{Origin: cfg.ZimbraOrigin})
		if err != nil {
			return err
		}
		defer backend.Close()
		mux := http.NewServeMux()
		frontend := ews.New()
		var store *sqlite.Store
		if cfg.StorePath != "" {
			store, err = sqlite.Open(ctx, cfg.StorePath)
			if err != nil {
				return err
			}
			defer store.Close()
			frontend, err = NewPersistentZimbraEWS(ctx, store, cfg.ZimbraOrigin, 200)
			if err != nil {
				return err
			}
			stopMaintenance := startJournalMaintenance(ctx, store, logger)
			defer stopMaintenance()
		}
		mux.Handle("/EWS/Exchange.asmx", core.BasicTLS(backend, frontend.Serve))
		discovery := core.BasicTLS(backend, autodiscover.Spike)
		if cfg.OutlookProfileSpike {
			namespace, err := mapi.NewNamespace(cfg.DeploymentID)
			if err != nil {
				return err
			}
			profile, err := autodiscover.NewProfile(cfg.PublicOrigin, namespace)
			if err != nil {
				return err
			}
			discovery = core.BasicTLS(backend, profile.Serve)
			if cfg.MAPISessionSpike {
				transport := mapi.NewSessionTransport(namespace)
				if cfg.MAPIStoreSpike {
					transport.EnableRAMStore()
					transport.EnableNotificationWait(newMailboxWakeups())
					if store != nil {
						transport.EnableObjectNotifications(newMAPINotificationJournal(store, cfg.ZimbraOrigin))
						transport.EnableNamedPropertyPersistence(bindMAPINamedProperties(store, cfg.ZimbraOrigin))
						if cfg.MAPIPublicZimbraFolder != "" {
							if err := transport.EnablePublicZimbraStore(cfg.MAPIPublicZimbraFolder, store, func(ctx context.Context, scope core.Scope) (string, error) {
								mailbox, err := store.BindMailbox(ctx, scope, cfg.ZimbraOrigin)
								return mailbox.MailboxID, err
							}); err != nil {
								return fmt.Errorf("invalid MAPI public Zimbra root: %w", err)
							}
						}
						if cfg.MAPIPersistentReplica {
							transport.EnableReplicaPersistence(store, func(ctx context.Context, scope core.Scope) (string, error) {
								mailbox, err := store.BindMailbox(ctx, scope, cfg.ZimbraOrigin)
								return mailbox.MailboxID, err
							})
						}
					}
					if cfg.MAPIHierarchyDownloadSpike {
						transport.EnableHierarchyDownloadSpike()
						if cfg.MAPIContentsDownloadSpike {
							transport.EnableContentsDownloadSpike()
							if cfg.MAPIFolderAggregates {
								transport.EnableFolderAggregates()
							}
							if cfg.MAPIContentsTableSpike {
								transport.EnableContentsTableSpike()
							}
							if cfg.MAPIFAICollectorSpike {
								transport.EnableFAICollectorSpike()
								if cfg.MAPICalendarMetadataSpike {
									transport.EnablePrivateOutlookConfiguration()
								}
								if store != nil {
									transport.EnableMailDrafts(MailDraftCreator{store: store, backendInstance: cfg.ZimbraOrigin})
									transport.EnableMailDraftSending(NewMailSender(store, cfg.ZimbraOrigin))
								}
								if cfg.MAPIHierarchyImport {
									transport.EnableHierarchyImport()
								}
								if cfg.MAPIReadStateImport {
									transport.EnableReadStateImport()
								}
							}
						}
					}
				}
				if cfg.MAPIUnknownPropertyTrace {
					transport.EnableUnknownPropertyTrace()
				}
				if cfg.MAPIOSTPropertySpike {
					transport.EnableOSTPropertySpike()
				}
				if cfg.MAPISetPropertyTrace {
					transport.EnableSetPropertyTrace()
				}
				if cfg.MAPISetListTrace {
					transport.EnableSetListTrace()
				}
				if cfg.MAPICalendarReferenceSetTrace {
					transport.EnableCalendarReferenceSetTrace()
				}
				if cfg.MAPIDraftsReferenceSpike {
					transport.EnableDraftsReferenceSpike()
				}
				if cfg.MAPITasksMetadataSpike {
					transport.EnableTasksMetadataSpike()
				}
				if cfg.MAPIContactsMetadataSpike {
					transport.EnableContactsMetadataSpike()
				}
				if cfg.MAPICalendarMetadataSpike {
					transport.EnableCalendarMetadataSpike()
				}
				if cfg.MAPIFolderMetadataSpike {
					transport.EnableFolderMetadataSpike()
				}
				if cfg.MAPICreateFolderSpike {
					transport.EnableCreateFolderSpike()
				}
				if cfg.MAPICreateFolderTrace {
					transport.EnableCreateFolderTrace()
				}
				if cfg.MAPISynchronizationOptionsTrace {
					transport.EnableSynchronizationOptionsTrace()
				}
				if cfg.MAPISynchronizationTrace {
					transport.EnableSynchronizationTrace()
				}
				if cfg.NSPIDirectorySpike {
					transport.EnableNativeDirectory()
				}
				mux.Handle("/mapi/emsmdb/", core.BasicTLS(backend, transport.Mailbox))
				if cfg.NSPIBindSpike {
					mux.Handle("/mapi/nspi/", core.BasicTLS(backend, transport.Directory))
				}
			} else {
				mux.Handle("/mapi/emsmdb/", core.BasicTLS(backend, namespace.MailboxSpike))
			}
			if !cfg.NSPIBindSpike {
				mux.Handle("/mapi/nspi/", core.BasicTLS(backend, namespace.DirectorySpike))
			}
		} else if cfg.MAPISpike {
			mux.Handle("/mapi/emsmdb/", core.BasicTLS(backend, mapi.Spike))
		}
		health := handler
		mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if cfg.OutlookLab && strings.EqualFold(r.URL.Path, "/autodiscover/autodiscover.xml") {
				discovery.ServeHTTP(w, r)
				return
			}
			health.ServeHTTP(w, r)
		}))
		handler = mux
	} else if cfg.JMAPSessionURL != "" {
		backend, err := jmap.New(cfg.JMAPSessionURL, nil)
		if err != nil {
			return err
		}
		store, err := sqlite.Open(ctx, cfg.StorePath)
		if err != nil {
			return err
		}
		defer store.Close()
		handler, err = jmapEWS(ctx, handler, backend, store, cfg.JMAPSessionURL)
		if err != nil {
			return err
		}
	}
	if cfg.OutlookLab {
		handler = detailedLaboratoryTraceAtLevel(logger, handler, slog.LevelDebug, laboratoryTraceOptions{cfg.MAPIUnknownPropertyTrace, cfg.MAPISetPropertyTrace, cfg.MAPISynchronizationTrace, cfg.MAPISynchronizationOptionsTrace, cfg.MAPICreateFolderTrace, cfg.MAPISetListTrace, cfg.MAPICalendarReferenceSetTrace})
	}
	handler = operationalTrace(logger, handler)
	var certificate tls.Certificate
	if cfg.CertFile != "" {
		var err error
		certificate, err = tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return errors.New("cannot load server TLS certificate and key")
		}
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	defer listener.Close()
	server := &http.Server{
		Handler:           withRequestBudget(handler, protocolRequestBudget),
		ErrorLog:          httpServerLogger(logger),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	if cfg.CertFile != "" {
		listener = tls.NewListener(listener, &tls.Config{
			MinVersion:   tls.VersionTLS12,
			Certificates: []tls.Certificate{certificate},
		})
	}
	logger.Info("service listening", "address", listener.Addr().String(), "tls", cfg.CertFile != "")
	if err := serveHTTP(ctx, server, listener, 5*time.Second); err != nil {
		return err
	}
	logger.Info("service stopped")
	return nil
}

func serveHTTP(ctx context.Context, server *http.Server, listener net.Listener, gracePeriod time.Duration) error {
	requests, cancelRequests := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelRequests()
	server.BaseContext = func(net.Listener) context.Context { return requests }
	finished := make(chan error, 1)
	go func() { finished <- server.Serve(listener) }()

	select {
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), gracePeriod)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			cancelRequests()
			_ = server.Close()
			<-finished
			return errors.New("graceful shutdown timed out")
		}
	case err := <-finished:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
	if err := <-finished; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
