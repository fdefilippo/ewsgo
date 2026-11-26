// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package service_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"ewsgo/internal/service"
)

func TestListenerPolicy(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  service.Config
		ok   bool
	}{
		{"loopback", service.Config{Listen: "127.0.0.1:8080"}, true},
		{"ipv6 loopback", service.Config{Listen: "[::1]:8080"}, true},
		{"public plaintext", service.Config{Listen: "0.0.0.0:8080"}, false},
		{"unspecified host", service.Config{Listen: ":8080"}, false},
		{"DNS plaintext", service.Config{Listen: "localhost:8080"}, false},
		{"partial TLS", service.Config{Listen: "127.0.0.1:8080", CertFile: "cert.pem"}, false},
		{"public TLS", service.Config{Listen: "0.0.0.0:8443", CertFile: "cert.pem", KeyFile: "key.pem"}, true},
		{"invalid port", service.Config{Listen: "127.0.0.1:65536"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.cfg.Validate(); (err == nil) != tc.ok {
				t.Fatalf("validation=%v, expected valid=%v", err, tc.ok)
			}
		})
	}
}

func TestHealthDoesNotAdvertiseGatewayReadiness(t *testing.T) {
	h := service.Handler()
	for _, tc := range []struct {
		path   string
		status int
	}{
		{"/health/live", 200}, {"/health/ready", 503}, {"/EWS/Exchange.asmx", 404},
	} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rr.Code != tc.status {
			t.Fatalf("%s: got %d, want %d", tc.path, rr.Code, tc.status)
		}
	}
}

func TestOutlookProfileRequiresExplicitSecureConfiguration(t *testing.T) {
	valid := service.Config{
		Listen: "127.0.0.1:8443", CertFile: "cert.pem", KeyFile: "key.pem",
		ZimbraOrigin: "https://backend.invalid", MAPISpike: true, OutlookLab: true,
		OutlookProfileSpike: true, PublicOrigin: "https://gateway.invalid",
		DeploymentID: "8f86be35-8131-4aab-8d63-7f23212d0885",
	}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*service.Config){
		func(c *service.Config) { c.OutlookProfileSpike = false },
		func(c *service.Config) { c.MAPISpike = false },
		func(c *service.Config) { c.OutlookLab = false },
		func(c *service.Config) { c.ZimbraOrigin = "" },
		func(c *service.Config) { c.DeploymentID = "" },
		func(c *service.Config) { c.PublicOrigin = "" },
		func(c *service.Config) { c.PublicOrigin = "http://gateway.invalid" },
		func(c *service.Config) { c.CertFile, c.KeyFile = "", "" },
	} {
		cfg := valid
		change(&cfg)
		if err := cfg.Validate(); err == nil {
			t.Fatal("incomplete profile configuration accepted")
		}
	}
}

func TestRunStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	started := make(startupLog, 1)
	go func() {
		finished <- service.Run(ctx, service.Config{Listen: "127.0.0.1:0"}, slog.New(slog.NewTextHandler(started, nil)))
	}()
	select {
	case <-started:
	case err := <-finished:
		t.Fatalf("server failed to start: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("server did not start")
	}
	cancel()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not stop")
	}
}

type startupLog chan struct{}

func (l startupLog) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("service listening")) {
		l <- struct{}{}
	}
	return len(p), nil
}

func TestMAPISessionSpikeRequiresProfile(t *testing.T) {
	cfg := service.Config{Listen: "127.0.0.1:8443", CertFile: "cert.pem", KeyFile: "key.pem", ZimbraOrigin: "https://backend.invalid", MAPISpike: true, OutlookLab: true, MAPISessionSpike: true}
	if cfg.Validate() == nil {
		t.Fatal("session transport enabled without self-identity profile")
	}
	cfg.OutlookProfileSpike = true
	cfg.PublicOrigin = "https://gateway.invalid"
	cfg.DeploymentID = "8f86be35-8131-4aab-8d63-7f23212d0885"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestNSPIBindSpikeRequiresSessionTransport(t *testing.T) {
	cfg := service.Config{Listen: "127.0.0.1:8443", CertFile: "cert.pem", KeyFile: "key.pem", ZimbraOrigin: "https://backend.invalid", MAPISpike: true, OutlookLab: true, OutlookProfileSpike: true, PublicOrigin: "https://gateway.invalid", DeploymentID: "8f86be35-8131-4aab-8d63-7f23212d0885", NSPIBindSpike: true}
	if cfg.Validate() == nil {
		t.Fatal("NSPI enabled without session transport")
	}
	cfg.MAPISessionSpike = true
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestNSPIDirectoryRequiresBindSpike(t *testing.T) {
	cfg := service.Config{Listen: "127.0.0.1:8443", CertFile: "cert.pem", KeyFile: "key.pem", ZimbraOrigin: "https://backend.invalid", MAPISpike: true, OutlookLab: true, OutlookProfileSpike: true, PublicOrigin: "https://gateway.invalid", DeploymentID: "8f86be35-8131-4aab-8d63-7f23212d0885", MAPISessionSpike: true, NSPIDirectorySpike: true}
	if cfg.Validate() == nil {
		t.Fatal("native directory enabled without Bind")
	}
	cfg.NSPIBindSpike = true
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestMAPIStoreRequiresSessionSpike(t *testing.T) {
	cfg := service.Config{Listen: "127.0.0.1:8443", CertFile: "cert.pem", KeyFile: "key.pem", ZimbraOrigin: "https://backend.invalid", MAPISpike: true, OutlookLab: true, OutlookProfileSpike: true, PublicOrigin: "https://gateway.invalid", DeploymentID: "8f86be35-8131-4aab-8d63-7f23212d0885", MAPIStoreSpike: true}
	if cfg.Validate() == nil {
		t.Fatal("store enabled without session transport")
	}
	cfg.MAPISessionSpike = true
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.OutlookProfileSpike = false
	if cfg.Validate() == nil {
		t.Fatal("store enabled without profile safeguards")
	}
}

func TestUnknownMAPIPropertyTraceRequiresExplicitLaboratoryStore(t *testing.T) {
	cfg := service.Config{Listen: "127.0.0.1:8443", CertFile: "cert.pem", KeyFile: "key.pem", ZimbraOrigin: "https://backend.invalid", MAPISpike: true, OutlookLab: true, OutlookProfileSpike: true, PublicOrigin: "https://gateway.invalid", DeploymentID: "8f86be35-8131-4aab-8d63-7f23212d0885", MAPISessionSpike: true, MAPIStoreSpike: true}
	if cfg.MAPIUnknownPropertyTrace {
		t.Fatal("unknown tags enabled by default")
	}
	cfg.MAPIUnknownPropertyTrace = true
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, remove := range []func(*service.Config){
		func(c *service.Config) { c.OutlookLab = false },
		func(c *service.Config) { c.MAPIStoreSpike = false },
		func(c *service.Config) { c.MAPISessionSpike = false },
		func(c *service.Config) { c.OutlookProfileSpike = false },
		func(c *service.Config) { c.CertFile, c.KeyFile = "", "" },
		func(c *service.Config) { c.ZimbraOrigin = "" },
	} {
		broken := cfg
		remove(&broken)
		if broken.Validate() == nil {
			t.Fatal("unknown property trace escaped laboratory prerequisites")
		}
	}
}

func TestSetPropertyTraceRequiresExplicitLaboratoryStore(t *testing.T) {
	cfg := service.Config{Listen: "127.0.0.1:8443", CertFile: "cert.pem", KeyFile: "key.pem", ZimbraOrigin: "https://backend.invalid", MAPISpike: true, OutlookLab: true, OutlookProfileSpike: true, PublicOrigin: "https://gateway.invalid", DeploymentID: "8f86be35-8131-4aab-8d63-7f23212d0885", MAPISessionSpike: true, MAPIStoreSpike: true}
	if cfg.MAPISetPropertyTrace {
		t.Fatal("SetProperties trace enabled by default")
	}
	cfg.MAPISetPropertyTrace = true
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, remove := range []func(*service.Config){
		func(c *service.Config) { c.OutlookLab = false }, func(c *service.Config) { c.MAPIStoreSpike = false },
		func(c *service.Config) { c.MAPISessionSpike = false }, func(c *service.Config) { c.OutlookProfileSpike = false },
		func(c *service.Config) { c.CertFile, c.KeyFile = "", "" }, func(c *service.Config) { c.ZimbraOrigin = "" },
	} {
		broken := cfg
		remove(&broken)
		if broken.Validate() == nil {
			t.Fatal("SetProperties trace escaped prerequisites")
		}
	}
}

func TestSynchronizationTraceRequiresExplicitLaboratoryStore(t *testing.T) {
	cfg := service.Config{Listen: "127.0.0.1:8443", CertFile: "cert.pem", KeyFile: "key.pem", ZimbraOrigin: "https://backend.invalid", MAPISpike: true, OutlookLab: true, OutlookProfileSpike: true, PublicOrigin: "https://gateway.invalid", DeploymentID: "8f86be35-8131-4aab-8d63-7f23212d0885", MAPISessionSpike: true, MAPIStoreSpike: true}
	if cfg.MAPISynchronizationTrace {
		t.Fatal("Synchronization trace enabled by default")
	}
	cfg.MAPISynchronizationTrace = true
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, remove := range []func(*service.Config){
		func(c *service.Config) { c.OutlookLab = false }, func(c *service.Config) { c.MAPIStoreSpike = false },
		func(c *service.Config) { c.MAPISessionSpike = false }, func(c *service.Config) { c.OutlookProfileSpike = false },
		func(c *service.Config) { c.CertFile, c.KeyFile = "", "" }, func(c *service.Config) { c.ZimbraOrigin = "" },
	} {
		broken := cfg
		remove(&broken)
		if broken.Validate() == nil {
			t.Fatal("Synchronization trace escaped prerequisites")
		}
	}
}

func TestHierarchyDownloadRequiresExplicitLaboratoryStore(t *testing.T) {
	cfg := service.Config{Listen: "127.0.0.1:8443", CertFile: "cert.pem", KeyFile: "key.pem", ZimbraOrigin: "https://backend.invalid", MAPISpike: true, OutlookLab: true, OutlookProfileSpike: true, PublicOrigin: "https://gateway.invalid", DeploymentID: "8f86be35-8131-4aab-8d63-7f23212d0885", MAPISessionSpike: true, MAPIStoreSpike: true}
	if cfg.MAPIHierarchyDownloadSpike {
		t.Fatal("download enabled by default")
	}
	cfg.MAPIHierarchyDownloadSpike = true
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, remove := range []func(*service.Config){
		func(c *service.Config) { c.OutlookLab = false }, func(c *service.Config) { c.MAPIStoreSpike = false }, func(c *service.Config) { c.MAPISessionSpike = false }, func(c *service.Config) { c.OutlookProfileSpike = false }, func(c *service.Config) { c.CertFile, c.KeyFile = "", "" }, func(c *service.Config) { c.ZimbraOrigin = "" },
	} {
		broken := cfg
		remove(&broken)
		if broken.Validate() == nil {
			t.Fatal("hierarchy escaped prerequisites")
		}
	}
}

func TestOSTPropertySpikeRequiresExplicitLaboratoryStore(t *testing.T) {
	cfg := service.Config{Listen: "127.0.0.1:8443", CertFile: "cert.pem", KeyFile: "key.pem", ZimbraOrigin: "https://backend.invalid", MAPISpike: true, OutlookLab: true, OutlookProfileSpike: true, PublicOrigin: "https://gateway.invalid", DeploymentID: "8f86be35-8131-4aab-8d63-7f23212d0885", MAPISessionSpike: true, MAPIStoreSpike: true}
	if cfg.MAPIOSTPropertySpike {
		t.Fatal("OST property spike enabled by default")
	}
	cfg.MAPIOSTPropertySpike = true
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, remove := range []func(*service.Config){
		func(c *service.Config) { c.OutlookLab = false }, func(c *service.Config) { c.MAPIStoreSpike = false },
		func(c *service.Config) { c.MAPISessionSpike = false }, func(c *service.Config) { c.OutlookProfileSpike = false },
		func(c *service.Config) { c.CertFile, c.KeyFile = "", "" }, func(c *service.Config) { c.ZimbraOrigin = "" },
	} {
		broken := cfg
		remove(&broken)
		if broken.Validate() == nil {
			t.Fatal("OST property spike escaped prerequisites")
		}
	}
}

func TestSynchronizationOptionsRequiresExplicitHierarchyLaboratory(t *testing.T) {
	cfg := service.Config{Listen: "127.0.0.1:8443", CertFile: "cert.pem", KeyFile: "key.pem", ZimbraOrigin: "https://backend.invalid", MAPISpike: true, OutlookLab: true, OutlookProfileSpike: true, PublicOrigin: "https://gateway.invalid", DeploymentID: "8f86be35-8131-4aab-8d63-7f23212d0885", MAPISessionSpike: true, MAPIStoreSpike: true, MAPIHierarchyDownloadSpike: true, MAPISynchronizationTrace: true}
	if cfg.MAPISynchronizationOptionsTrace {
		t.Fatal("options trace default")
	}
	cfg.MAPISynchronizationOptionsTrace = true
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, remove := range []func(*service.Config){func(c *service.Config) { c.MAPISynchronizationTrace = false }, func(c *service.Config) { c.MAPIHierarchyDownloadSpike = false }, func(c *service.Config) { c.OutlookLab = false }, func(c *service.Config) { c.MAPIStoreSpike = false }, func(c *service.Config) { c.MAPISessionSpike = false }, func(c *service.Config) { c.CertFile, c.KeyFile = "", "" }, func(c *service.Config) { c.ZimbraOrigin = "" }} {
		broken := cfg
		remove(&broken)
		if broken.Validate() == nil {
			t.Fatal("options trace escaped prerequisite")
		}
	}
}

func TestCreateFolderTraceRequiresExplicitLaboratory(t *testing.T) {
	cfg := service.Config{Listen: "127.0.0.1:8443", CertFile: "cert.pem", KeyFile: "key.pem", ZimbraOrigin: "https://backend.invalid", MAPISpike: true, OutlookLab: true, OutlookProfileSpike: true, PublicOrigin: "https://gateway.invalid", DeploymentID: "8f86be35-8131-4aab-8d63-7f23212d0885", MAPISessionSpike: true, MAPIStoreSpike: true}
	if cfg.MAPICreateFolderTrace {
		t.Fatal("CreateFolder tracing default")
	}
	cfg.MAPICreateFolderTrace = true
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, remove := range []func(*service.Config){func(c *service.Config) { c.OutlookLab = false }, func(c *service.Config) { c.MAPIStoreSpike = false }, func(c *service.Config) { c.MAPISessionSpike = false }, func(c *service.Config) { c.CertFile, c.KeyFile = "", "" }, func(c *service.Config) { c.ZimbraOrigin = "" }} {
		bad := cfg
		remove(&bad)
		if bad.Validate() == nil {
			t.Fatal("CreateFolder trace escaped prerequisites")
		}
	}
}

func TestRAMCreateFolderRequiresLaboratoryHierarchy(t *testing.T) {
	cfg := service.Config{Listen: "127.0.0.1:8443", CertFile: "cert.pem", KeyFile: "key.pem", ZimbraOrigin: "https://backend.invalid", MAPISpike: true, OutlookLab: true, OutlookProfileSpike: true, PublicOrigin: "https://gateway.invalid", DeploymentID: "8f86be35-8131-4aab-8d63-7f23212d0885", MAPISessionSpike: true, MAPIStoreSpike: true, MAPIHierarchyDownloadSpike: true}
	if cfg.MAPICreateFolderSpike {
		t.Fatal("default enabled")
	}
	cfg.MAPICreateFolderSpike = true
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, remove := range []func(*service.Config){func(c *service.Config) { c.OutlookLab = false }, func(c *service.Config) { c.MAPIStoreSpike = false }, func(c *service.Config) { c.MAPIHierarchyDownloadSpike = false }, func(c *service.Config) { c.MAPISessionSpike = false }, func(c *service.Config) { c.CertFile, c.KeyFile = "", "" }, func(c *service.Config) { c.ZimbraOrigin = "" }} {
		bad := cfg
		remove(&bad)
		if bad.Validate() == nil {
			t.Fatal("missing prerequisite")
		}
	}
}

func TestSetListTraceRequiresExplicitLaboratoryHeaderTrace(t *testing.T) {
	cfg := service.Config{Listen: "127.0.0.1:8443", CertFile: "cert.pem", KeyFile: "key.pem", ZimbraOrigin: "https://backend.invalid", MAPISpike: true, OutlookLab: true, OutlookProfileSpike: true, PublicOrigin: "https://gateway.invalid", DeploymentID: "8f86be35-8131-4aab-8d63-7f23212d0885", MAPISessionSpike: true, MAPIStoreSpike: true, MAPISetPropertyTrace: true}
	if cfg.MAPISetListTrace {
		t.Fatal("default")
	}
	cfg.MAPISetListTrace = true
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, remove := range []func(*service.Config){func(c *service.Config) { c.OutlookLab = false }, func(c *service.Config) { c.MAPISetPropertyTrace = false }, func(c *service.Config) { c.MAPIStoreSpike = false }, func(c *service.Config) { c.MAPISessionSpike = false }, func(c *service.Config) { c.CertFile, c.KeyFile = "", "" }, func(c *service.Config) { c.ZimbraOrigin = "" }} {
		bad := cfg
		remove(&bad)
		if bad.Validate() == nil {
			t.Fatal("missing gate")
		}
	}
}

func TestFolderMetadataRequiresLaboratoryHierarchy(t *testing.T) {
	cfg := service.Config{Listen: "127.0.0.1:8443", CertFile: "cert.pem", KeyFile: "key.pem", ZimbraOrigin: "https://backend.invalid", MAPISpike: true, OutlookLab: true, OutlookProfileSpike: true, PublicOrigin: "https://gateway.invalid", DeploymentID: "8f86be35-8131-4aab-8d63-7f23212d0885", MAPISessionSpike: true, MAPIStoreSpike: true, MAPIHierarchyDownloadSpike: true}
	if cfg.MAPIFolderMetadataSpike {
		t.Fatal("default enabled")
	}
	cfg.MAPIFolderMetadataSpike = true
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, remove := range []func(*service.Config){func(c *service.Config) { c.OutlookLab = false }, func(c *service.Config) { c.MAPIStoreSpike = false }, func(c *service.Config) { c.MAPIHierarchyDownloadSpike = false }, func(c *service.Config) { c.MAPISessionSpike = false }, func(c *service.Config) { c.CertFile, c.KeyFile = "", "" }, func(c *service.Config) { c.ZimbraOrigin = "" }} {
		bad := cfg
		remove(&bad)
		if bad.Validate() == nil {
			t.Fatal("missing prerequisite")
		}
	}
}

func TestCalendarMetadataRequiresLaboratoryHierarchy(t *testing.T) {
	cfg := service.Config{Listen: "127.0.0.1:8443", CertFile: "cert.pem", KeyFile: "key.pem", ZimbraOrigin: "https://backend.invalid", MAPISpike: true, OutlookLab: true, OutlookProfileSpike: true, PublicOrigin: "https://gateway.invalid", DeploymentID: "8f86be35-8131-4aab-8d63-7f23212d0885", MAPISessionSpike: true, MAPIStoreSpike: true, MAPIHierarchyDownloadSpike: true}
	if cfg.MAPICalendarMetadataSpike {
		t.Fatal("default enabled")
	}
	cfg.MAPICalendarMetadataSpike = true
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, remove := range []func(*service.Config){func(c *service.Config) { c.OutlookLab = false }, func(c *service.Config) { c.MAPIStoreSpike = false }, func(c *service.Config) { c.MAPIHierarchyDownloadSpike = false }, func(c *service.Config) { c.MAPISessionSpike = false }, func(c *service.Config) { c.CertFile, c.KeyFile = "", "" }, func(c *service.Config) { c.ZimbraOrigin = "" }} {
		bad := cfg
		remove(&bad)
		if bad.Validate() == nil {
			t.Fatal("missing prerequisite")
		}
	}
}

func TestCalendarSetOutcomeTraceRequiresExplicitCalendarLaboratory(t *testing.T) {
	cfg := service.Config{Listen: "127.0.0.1:8443", CertFile: "cert.pem", KeyFile: "key.pem", ZimbraOrigin: "https://backend.invalid", MAPISpike: true, OutlookLab: true, OutlookProfileSpike: true, PublicOrigin: "https://gateway.invalid", DeploymentID: "8f86be35-8131-4aab-8d63-7f23212d0885", MAPISessionSpike: true, MAPIStoreSpike: true, MAPIHierarchyDownloadSpike: true, MAPICalendarMetadataSpike: true}
	if cfg.MAPICalendarReferenceSetTrace {
		t.Fatal("default enabled")
	}
	cfg.MAPICalendarReferenceSetTrace = true
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, remove := range []func(*service.Config){func(c *service.Config) { c.OutlookLab = false }, func(c *service.Config) { c.MAPIStoreSpike = false }, func(c *service.Config) { c.MAPIHierarchyDownloadSpike = false }, func(c *service.Config) { c.MAPICalendarMetadataSpike = false }, func(c *service.Config) { c.MAPISessionSpike = false }, func(c *service.Config) { c.CertFile, c.KeyFile = "", "" }, func(c *service.Config) { c.ZimbraOrigin = "" }} {
		bad := cfg
		remove(&bad)
		if bad.Validate() == nil {
			t.Fatal("missing prerequisite")
		}
	}
}

func TestContentsDownloadRequiresExplicitLaboratoryPrerequisites(t *testing.T) {
	cfg := service.Config{Listen: "127.0.0.1:8443", CertFile: "cert.pem", KeyFile: "key.pem", ZimbraOrigin: "https://backend.invalid", MAPISpike: true, OutlookLab: true, OutlookProfileSpike: true, PublicOrigin: "https://gateway.invalid", DeploymentID: "8f86be35-8131-4aab-8d63-7f23212d0885", MAPISessionSpike: true, MAPIStoreSpike: true, MAPIHierarchyDownloadSpike: true}
	if cfg.MAPIContentsDownloadSpike {
		t.Fatal("Contents enabled by default")
	}
	cfg.MAPIContentsDownloadSpike = true
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, remove := range []func(*service.Config){func(c *service.Config) { c.OutlookLab = false }, func(c *service.Config) { c.MAPIStoreSpike = false }, func(c *service.Config) { c.MAPIHierarchyDownloadSpike = false }, func(c *service.Config) { c.MAPISessionSpike = false }, func(c *service.Config) { c.CertFile, c.KeyFile = "", "" }, func(c *service.Config) { c.ZimbraOrigin = "" }} {
		broken := cfg
		remove(&broken)
		if broken.Validate() == nil {
			t.Fatal("Contents escaped prerequisites")
		}
	}
}

func TestFAICollectorRequiresExplicitLaboratoryPrerequisites(t *testing.T) {
	cfg := service.Config{Listen: "127.0.0.1:8443", CertFile: "cert.pem", KeyFile: "key.pem", ZimbraOrigin: "https://backend.invalid", MAPISpike: true, OutlookLab: true, OutlookProfileSpike: true, PublicOrigin: "https://gateway.invalid", DeploymentID: "8f86be35-8131-4aab-8d63-7f23212d0885", MAPISessionSpike: true, MAPIStoreSpike: true, MAPIHierarchyDownloadSpike: true}
	cfg.MAPIContentsDownloadSpike = true
	if cfg.MAPIFAICollectorSpike {
		t.Fatal("FAI collector enabled by default")
	}
	cfg.MAPIFAICollectorSpike = true
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, remove := range []func(*service.Config){func(c *service.Config) { c.MAPIContentsDownloadSpike = false }, func(c *service.Config) { c.OutlookLab = false }, func(c *service.Config) { c.MAPIStoreSpike = false }, func(c *service.Config) { c.MAPIHierarchyDownloadSpike = false }, func(c *service.Config) { c.MAPISessionSpike = false }, func(c *service.Config) { c.CertFile, c.KeyFile = "", "" }, func(c *service.Config) { c.ZimbraOrigin = "" }} {
		broken := cfg
		remove(&broken)
		if broken.Validate() == nil {
			t.Fatal("FAI collector escaped prerequisites")
		}
	}
}

func TestContactsMetadataRequiresLaboratoryHierarchy(t *testing.T) {
	cfg := service.Config{Listen: "127.0.0.1:8443", CertFile: "cert.pem", KeyFile: "key.pem", ZimbraOrigin: "https://backend.invalid", MAPISpike: true, OutlookLab: true, OutlookProfileSpike: true, PublicOrigin: "https://gateway.invalid", DeploymentID: "8f86be35-8131-4aab-8d63-7f23212d0885", MAPISessionSpike: true, MAPIStoreSpike: true, MAPIHierarchyDownloadSpike: true}
	if cfg.MAPIContactsMetadataSpike {
		t.Fatal("default enabled")
	}
	cfg.MAPIContactsMetadataSpike = true
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, remove := range []func(*service.Config){func(c *service.Config) { c.OutlookLab = false }, func(c *service.Config) { c.MAPIStoreSpike = false }, func(c *service.Config) { c.MAPIHierarchyDownloadSpike = false }, func(c *service.Config) { c.MAPISessionSpike = false }, func(c *service.Config) { c.CertFile, c.KeyFile = "", "" }, func(c *service.Config) { c.ZimbraOrigin = "" }} {
		bad := cfg
		remove(&bad)
		if bad.Validate() == nil {
			t.Fatal("missing prerequisite")
		}
	}
}

func TestTasksMetadataRequiresLaboratoryHierarchy(t *testing.T) {
	cfg := service.Config{Listen: "127.0.0.1:8443", CertFile: "cert.pem", KeyFile: "key.pem", ZimbraOrigin: "https://backend.invalid", MAPISpike: true, OutlookLab: true, OutlookProfileSpike: true, PublicOrigin: "https://gateway.invalid", DeploymentID: "8f86be35-8131-4aab-8d63-7f23212d0885", MAPISessionSpike: true, MAPIStoreSpike: true, MAPIHierarchyDownloadSpike: true}
	if cfg.MAPITasksMetadataSpike {
		t.Fatal("default enabled")
	}
	cfg.MAPITasksMetadataSpike = true
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, remove := range []func(*service.Config){func(c *service.Config) { c.OutlookLab = false }, func(c *service.Config) { c.MAPIStoreSpike = false }, func(c *service.Config) { c.MAPIHierarchyDownloadSpike = false }, func(c *service.Config) { c.MAPISessionSpike = false }, func(c *service.Config) { c.CertFile, c.KeyFile = "", "" }, func(c *service.Config) { c.ZimbraOrigin = "" }} {
		bad := cfg
		remove(&bad)
		if bad.Validate() == nil {
			t.Fatal("missing prerequisite")
		}
	}
}

func TestDraftsReferenceRequiresLaboratoryHierarchy(t *testing.T) {
	cfg := service.Config{Listen: "127.0.0.1:8443", CertFile: "cert.pem", KeyFile: "key.pem", ZimbraOrigin: "https://backend.invalid", MAPISpike: true, OutlookLab: true, OutlookProfileSpike: true, PublicOrigin: "https://gateway.invalid", DeploymentID: "8f86be35-8131-4aab-8d63-7f23212d0885", MAPISessionSpike: true, MAPIStoreSpike: true, MAPIHierarchyDownloadSpike: true}
	if cfg.MAPIDraftsReferenceSpike {
		t.Fatal("default enabled")
	}
	cfg.MAPIDraftsReferenceSpike = true
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, remove := range []func(*service.Config){func(c *service.Config) { c.OutlookLab = false }, func(c *service.Config) { c.MAPIStoreSpike = false }, func(c *service.Config) { c.MAPIHierarchyDownloadSpike = false }, func(c *service.Config) { c.MAPISessionSpike = false }, func(c *service.Config) { c.CertFile, c.KeyFile = "", "" }, func(c *service.Config) { c.ZimbraOrigin = "" }} {
		bad := cfg
		remove(&bad)
		if bad.Validate() == nil {
			t.Fatal("missing prerequisite")
		}
	}
}

func TestContentsTableRequiresLaboratoryContents(t *testing.T) {
	cfg := service.Config{Listen: "127.0.0.1:8443", CertFile: "cert.pem", KeyFile: "key.pem", ZimbraOrigin: "https://backend.invalid", MAPISpike: true, OutlookLab: true, OutlookProfileSpike: true, PublicOrigin: "https://gateway.invalid", DeploymentID: "8f86be35-8131-4aab-8d63-7f23212d0885", MAPISessionSpike: true, MAPIStoreSpike: true, MAPIHierarchyDownloadSpike: true, MAPIContentsDownloadSpike: true}
	if cfg.MAPIContentsTableSpike {
		t.Fatal("Table enabled by default")
	}
	cfg.MAPIContentsTableSpike = true
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, remove := range []func(*service.Config){func(c *service.Config) { c.OutlookLab = false }, func(c *service.Config) { c.MAPIStoreSpike = false }, func(c *service.Config) { c.MAPIContentsDownloadSpike = false }, func(c *service.Config) { c.MAPIHierarchyDownloadSpike = false }, func(c *service.Config) { c.MAPISessionSpike = false }, func(c *service.Config) { c.CertFile, c.KeyFile = "", "" }} {
		broken := cfg
		remove(&broken)
		if broken.Validate() == nil {
			t.Fatal("Table escaped prerequisites")
		}
	}
}

func TestReadStateImportRequiresContentsCollector(t *testing.T) {
	cfg := service.Config{Listen: "127.0.0.1:8443", CertFile: "cert.pem", KeyFile: "key.pem", ZimbraOrigin: "https://backend.invalid", MAPISpike: true, OutlookLab: true, OutlookProfileSpike: true, PublicOrigin: "https://gateway.invalid", DeploymentID: "8f86be35-8131-4aab-8d63-7f23212d0885", MAPISessionSpike: true, MAPIStoreSpike: true, MAPIHierarchyDownloadSpike: true, MAPIContentsDownloadSpike: true, MAPIFAICollectorSpike: true}
	if cfg.MAPIReadStateImport {
		t.Fatal("native read-state import enabled by default")
	}
	cfg.MAPIReadStateImport = true
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, remove := range []func(*service.Config){func(c *service.Config) { c.MAPIFAICollectorSpike = false }, func(c *service.Config) { c.MAPIContentsDownloadSpike = false }, func(c *service.Config) { c.OutlookLab = false }, func(c *service.Config) { c.CertFile = "" }} {
		bad := cfg
		remove(&bad)
		if bad.Validate() == nil {
			t.Fatal("native read-state import escaped prerequisites")
		}
	}
}
