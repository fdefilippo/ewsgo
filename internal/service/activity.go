// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"ewsgo/internal/core"
	"ewsgo/internal/frontend/mapi"
)

// The allowlists are an output boundary: unknown client/extension strings are
// collapsed, even if an observer caller accidentally passes private values.
var activityNames = func() map[string]map[string]bool {
	vocabularies := map[string]string{
		"authentication": "Authenticate",
		"backend":        "Auth EndSession GetInfo GetPrefs SearchGal Sync MsgAction ItemAction MoveMailMessage CopyMailMessage TrashMailMessage SoftDeleteMailMessage RestoreMailMessage RecoverMailMessage ReadRecoverableMail ReadRecoverableDeletionTimes CreateFolder FolderAction AddMsg AppendMailMessage UploadMailMIME UploadDraftMIME SaveDraft SaveMailDraft MarkMailRead SetMailFlag SetMailMarks SetCustomMetadata GetFolder Search GetMsg GetMIME GetAttachment NoOp SendMsg CreateWaitSet WaitSet DestroyWaitSet GetContacts CreateContact ModifyContact ContactAction GetAppointment CreateAppointment ModifyAppointment CreateAppointmentException CancelAppointment GetFreeBusy Batch",
		"core":           "PrepareMailSend SubmitMailSend ObserveMailSend SendMIME SendDraft",
		"ews":            "GetFolder SyncFolderHierarchy SyncFolderItems GetItem FindItem UpdateItem CreateItem MoveItem CopyItem DeleteItem CreateFolder UpdateFolder MoveFolder DeleteFolder CreateAttachment DeleteAttachment Subscribe GetEvents Unsubscribe GetUserAvailabilityRequest",
		"mapi":           "Connect Execute Disconnect NotificationWait Bind Unbind QueryRows ResolveNames GetSpecialTable GetMatches GetProps GetPropList DNToMId GetHierarchyInfo GetNamesFromIDs GetIDsFromNames ResortRestriction CompareMIds SeekEntries UpdateStat ResolveNamesW PING Ping DraftSubmissionUnconfirmed DraftSubmissionUnconfirmedWithSentCopy TrashOutboxTransportCopy",
		"rop":            "Execute",
	}
	out := make(map[string]map[string]bool, len(vocabularies))
	for layer, names := range vocabularies {
		out[layer] = make(map[string]bool)
		for _, name := range strings.Fields(names) {
			out[layer][name] = true
		}
	}
	return out
}()

func activityOperation(layer, op string) string {
	if activityNames[layer][op] || layer == "rop" && mapi.IsExecutionOperationLabel(op) {
		return op
	}
	return "other"
}
func activityLayer(layer string) string {
	switch layer {
	case "authentication", "backend", "core", "ews", "mapi", "rop":
		return layer
	}
	return "other"
}

type requestActivity struct {
	mu              sync.Mutex
	account         string
	events          []core.Activity
	dropped         int
	result          core.ActivityResult
	operation       string
	operations      map[string]bool
	backendFailures int
	backendFailure  core.ActivityResult
}

func (a *requestActivity) Authenticated(account string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if account == "" || len(account) > 1024 || !utf8.ValidString(account) || strings.ContainsFunc(account, unicode.IsControl) {
		a.account = "unavailable"
		return
	}
	a.account = account
}
func (a *requestActivity) Record(event core.Activity) {
	a.mu.Lock()
	defer a.mu.Unlock()
	event.Layer = activityLayer(event.Layer)
	event.Operation = activityOperation(event.Layer, event.Operation)
	if event.Result > core.ActivityFailure {
		event.Result = core.ActivityFailure
	}
	if event.Layer == "backend" && event.Result != core.ActivitySuccess {
		a.backendFailures++
		if a.backendFailure == core.ActivitySuccess || event.Result >= core.ActivityOutcomeUnknown {
			a.backendFailure = event.Result
		}
	}
	// Rebuild the slice so a caller cannot mutate retained data after recording.
	ops := make([]string, 0, min(len(event.Operations), 32))
	for i, op := range event.Operations {
		if i == 32 {
			break
		}
		op = activityOperation("rop", op)
		if op != "other" {
			ops = append(ops, op)
			a.operations[op] = true
		}
	}
	event.Operations = ops
	if event.Layer == "ews" {
		a.operation = event.Operation
	}
	// A failed protocol outcome takes precedence; a failed cleanup/backend call
	// raises log severity separately but does not change the protocol outcome.
	if event.Layer != "backend" && event.Result != core.ActivitySuccess {
		if a.result == core.ActivitySuccess || event.Result >= core.ActivityOutcomeUnknown {
			a.result = event.Result
		}
	}
	if len(a.events) == 128 {
		a.dropped++
		return
	}
	a.events = append(a.events, event)
}

type activityResponse struct {
	http.ResponseWriter
	status      int
	writeFailed bool
}

func (w *activityResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *activityResponse) WriteHeader(status int) {
	if status >= 100 && status < 200 {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
func (w *activityResponse) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	n, err := w.ResponseWriter.Write(data)
	if err != nil {
		w.writeFailed = true
	}
	return n, err
}

func activityProtocol(r *http.Request) (protocol, operation string) {
	switch strings.ToLower(r.URL.Path) {
	case "/ews/exchange.asmx":
		return "ews", "request"
	case "/mapi/emsmdb/":
		return "emsmdb", activityOperation("mapi", r.Header.Get("X-RequestType"))
	case "/mapi/nspi/":
		return "nspi", activityOperation("mapi", r.Header.Get("X-RequestType"))
	case "/autodiscover/autodiscover.xml":
		return "autodiscover", "Autodiscover"
	case "/health/live", "/health/ready":
		return "health", "health"
	default:
		return "http", "request"
	}
}

type activityLogKey struct{}
type activityLogContext struct {
	requestID string
	activity  *requestActivity
}

func operationalTrace(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !logger.Enabled(r.Context(), slog.LevelError) {
			next.ServeHTTP(w, r)
			return
		}
		protocol, operation := activityProtocol(r)
		started := time.Now()
		var random [16]byte
		// A missing correlation identifier must never fall back to a client value.
		correlation := "unavailable"
		if _, err := rand.Read(random[:]); err == nil {
			correlation = hex.EncodeToString(random[:])
		}
		activity := &requestActivity{operation: operation, operations: make(map[string]bool)}
		response := &activityResponse{ResponseWriter: w}
		completed := false
		defer func() {
			activity.mu.Lock()
			defer activity.mu.Unlock()
			status := response.status
			if status == 0 {
				status = 200
				if !completed {
					status = 500
				}
			}
			result := activity.result
			if !completed || response.writeFailed {
				result = core.ActivityFailure
			}
			level := slog.LevelInfo
			switch {
			case status >= 500:
				result = core.ActivityFailure
				level = slog.LevelError
			case result >= core.ActivityOutcomeUnknown:
				level = slog.LevelError
			case status >= 400:
				if result == core.ActivitySuccess {
					result = core.ActivityRejected
				}
				level = slog.LevelWarn
			case result != core.ActivitySuccess:
				level = slog.LevelWarn
			case activity.backendFailure >= core.ActivityOutcomeUnknown:
				level = slog.LevelError
			case activity.backendFailures > 0:
				level = slog.LevelWarn
			case protocol == "health":
				level = slog.LevelDebug
			}
			account := activity.account
			if account == "" {
				account = "unauthenticated"
			}
			operations := make([]string, 0, len(activity.operations))
			for op := range activity.operations {
				operations = append(operations, op)
			}
			slices.Sort(operations)
			method := "other"
			switch r.Method {
			case "GET", "POST", "PUT", "DELETE", "PATCH", "OPTIONS", "HEAD":
				method = r.Method
			}
			attrs := []any{"request_id", correlation, "account", account, "protocol", protocol,
				"method", method, "operation", activity.operation, "completed", completed, "response_write_failed", response.writeFailed, "result", result.String(), "http_status", status,
				"duration_ms", time.Since(started).Milliseconds(), "operations", operations, "detail_events_dropped", activity.dropped, "backend_failures", activity.backendFailures, "backend_result", activity.backendFailure.String()}
			logger.Log(r.Context(), level, "user_operation", attrs...)
			if logger.Enabled(r.Context(), slog.LevelDebug) {
				for _, event := range activity.events {
					fields := []any{"request_id", correlation, "account", account,
						"layer", event.Layer, "operation", event.Operation, "result", event.Result.String(),
						"duration_ms", max(int64(0), event.Duration.Milliseconds()), "operations", event.Operations}
					if event.Layer == "rop" && event.Operation != "other" && event.ROPID != 0 {
						fields = append(fields, "rop_id", fmt.Sprintf("0x%02x", event.ROPID),
							"rop_return_value", fmt.Sprintf("0x%08x", event.ROPReturnValue))
					}
					logger.DebugContext(r.Context(), "operation_step", fields...)
				}
			}
		}()
		ctx := core.WithActivityObserver(r.Context(), activity)
		ctx = context.WithValue(ctx, activityLogKey{}, activityLogContext{correlation, activity})
		next.ServeHTTP(response, r.WithContext(ctx))
		completed = true
	})
}
