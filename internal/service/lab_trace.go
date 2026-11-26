// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package service

import (
	"ewsgo/internal/core"
	"ewsgo/internal/frontend/mapi"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// laboratoryTrace records a closed vocabulary, with bounded technical tags
// only under the separate laboratory opt-in. Never log values or identities.
func laboratoryTrace(logger *slog.Logger, next http.Handler, unknownPropertyTrace, setPropertyTrace, synchronizationTrace bool) http.Handler {
	return laboratoryOptionsTrace(logger, next, unknownPropertyTrace, setPropertyTrace, synchronizationTrace, false)
}

func laboratoryOptionsTrace(logger *slog.Logger, next http.Handler, unknownPropertyTrace, setPropertyTrace, synchronizationTrace, synchronizationOptionsTrace bool) http.Handler {
	return detailedLaboratoryTrace(logger, next, laboratoryTraceOptions{unknownPropertyTrace, setPropertyTrace, synchronizationTrace, synchronizationOptionsTrace, false, false, false})
}

type laboratoryTraceOptions struct{ unknownProperty, setProperty, synchronization, synchronizationOptions, createFolder, setList, calendarReferenceSet bool }

func detailedLaboratoryTrace(logger *slog.Logger, next http.Handler, opts laboratoryTraceOptions) http.Handler {
	return detailedLaboratoryTraceAtLevel(logger, next, slog.LevelInfo, opts)
}

func detailedLaboratoryTraceAtLevel(logger *slog.Logger, next http.Handler, level slog.Level, opts laboratoryTraceOptions) http.Handler {
	unknownPropertyTrace, setPropertyTrace, synchronizationTrace, synchronizationOptionsTrace := opts.unknownProperty, opts.setProperty, opts.synchronization, opts.synchronizationOptions
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		response := &labResponse{ResponseWriter: w, unknownPropertyTrace: unknownPropertyTrace, setPropertyTrace: setPropertyTrace, synchronizationTrace: synchronizationTrace, synchronizationOptionsTrace: synchronizationOptionsTrace, createFolderTrace: opts.createFolder, setListTrace: opts.setList, calendarReferenceSetTrace: opts.calendarReferenceSet}
		next.ServeHTTP(response, r)
		status := response.status
		if status == 0 {
			status = http.StatusOK
		}
		method := "other"
		switch r.Method {
		case "GET", "POST", "OPTIONS", "HEAD":
			method = r.Method
		}
		auth, parameters, multiple := laboratoryAuth(r.Header.Values("Authorization"))
		requestType := "other"
		switch r.Header.Get("X-RequestType") {
		case "Connect", "Execute", "Disconnect", "NotificationWait", "Bind", "Unbind", "QueryRows", "ResolveNames", "GetSpecialTable", "GetMatches", "GetProps", "GetPropList", "DNToMId", "GetHierarchyInfo", "GetNamesFromIDs", "GetIDsFromNames", "ResortRestriction", "CompareMIds", "SeekEntries", "UpdateStat", "ResolveNamesW", "PING", "Ping":
			requestType = r.Header.Get("X-RequestType")
		case "":
			requestType = "none"
		}
		transportCode := "none"
		if value := response.Header().Get("X-ResponseCode"); value != "" {
			transportCode = "other"
			if code, err := strconv.Atoi(value); err == nil && code >= 0 && code <= 18 {
				transportCode = strconv.Itoa(code)
			}
		}
		operationResult := response.mapiResult
		if operationResult == "" {
			operationResult = "none"
		}
		names, resolved := response.directoryNames, response.directoryResolved
		if names == "" {
			names, resolved = "none", "none"
		}
		properties := response.propertyTags
		if properties == nil {
			properties = []string{"none"}
		}
		values, missing := response.propertyValues, response.propertyErrors
		if values == "" {
			values, missing = "none", "none"
		}
		layout, first, flags := response.ropLayout, response.ropFirst, response.ropFlags
		if layout == "" {
			layout, first = "none", "none"
		}
		if flags == nil {
			flags = []string{"none"}
		}
		codepage := response.nspiCodepage
		if codepage == "" {
			codepage = "none"
		}
		logons, folders, ropErrors := response.storeLogons, response.storeFolders, response.storeErrors
		if logons == "" {
			logons, folders, ropErrors = "none", "none", "none"
		}
		unsupported := response.ropUnsupported
		if unsupported == "" {
			unsupported = "none"
		}
		propertyNames, objectTypes := response.storePropertyNames, response.storePropertyObjects
		if propertyNames == nil {
			propertyNames = []string{"none"}
		}
		if objectTypes == nil {
			objectTypes = []string{"none"}
		}
		propertyCounts := response.storePropertyCounts
		if propertyCounts[0] == "" {
			propertyCounts = [5]string{"none", "none", "none", "none", "none"}
		}
		attributes := []any{"route", laboratoryRoute(r.URL.Path),
			"method", method, "auth_scheme", auth, "auth_parameters_present", parameters, "auth_header_multiple", multiple, "request_type", requestType,
			"mapi_capability_present", r.Header.Get("X-MapiHttpCapability") != "",
			"status", status, "mapi_response_code", transportCode, "mapi_operation_result", operationResult,
			"nspi_names", names, "nspi_resolved", resolved,
			"nspi_properties", properties, "nspi_values", values, "nspi_errors", missing,
			"nspi_codepage", codepage,
			"rop_layout", layout, "rop_first", first, "rop_flags", flags,
			"rop_unsupported", unsupported,
			"store_logons", logons, "store_folders", folders, "store_rop_errors", ropErrors,
			"store_properties", propertyNames, "store_property_objects", objectTypes,
			"store_property_requests", propertyCounts[0], "store_property_requested", propertyCounts[1],
			"store_property_values", propertyCounts[2], "store_property_errors", propertyCounts[3], "store_property_other", propertyCounts[4],
			"elapsed_ms", time.Since(started).Milliseconds()}
		if failure, context := response.authenticationFailure.Fields(); failure != "none" {
			attributes = append(attributes, "authentication_failure", failure, "authentication_request_context", context)
		}
		if unknownPropertyTrace {
			tags, counts := response.unknownPropertyTags, response.unknownPropertyCounts
			if tags == nil {
				tags, counts = []string{"none"}, [3]string{"none", "none", "none"}
			}
			attributes = append(attributes, "store_property_unknown_tags", tags,
				"store_property_unknown_tag_overflow", counts[0], "store_property_unknown_high_ids", counts[1], "store_property_unknown_reserved_ids", counts[2])
		}
		if setPropertyTrace {
			inspection, count, tag := response.setProperty.Fields()
			attributes = append(attributes, "store_set_property_inspection", inspection,
				"store_set_property_count", count, "store_set_property_first_tag", tag)
		}
		if synchronizationTrace {
			inspection, kind := response.synchronization.Fields()
			attributes = append(attributes, "store_sync_inspection", inspection, "store_sync_type", kind)
		}
		if synchronizationOptionsTrace {
			flags, send, extra, rejections, restriction, tags := response.synchronizationOptions.Fields()
			attributes = append(attributes, "store_sync_flags", flags, "store_sync_send_options", send, "store_sync_extra_flags", extra, "store_sync_profile_rejections", rejections, "store_sync_restriction", restriction, "store_sync_tag_count", tags)
			stage, ops, firstError, buffers, calls := response.execution.Fields()
			attributes = append(attributes, "store_execute_stage", stage, "store_executed_rops", ops, "store_first_rop_error", firstError, "store_getbuffer_outcomes", buffers, "store_getbuffer_calls", calls)
			attributes = append(attributes, "store_rejected_table_flags", response.execution.RejectedTableFlags())
			if flags, scopes := response.execution.SearchOptions(); len(scopes) != 0 {
				attributes = append(attributes, "store_search_flags", flags, "store_search_scope_size", scopes)
			}
			if reasons := response.execution.SearchRejections(); len(reasons) != 0 {
				attributes = append(attributes, "store_search_rejections", reasons)
			}
			if restriction := response.execution.SearchRestrictionRejection(); restriction != "" {
				attributes = append(attributes, "store_search_restriction_rejection", restriction)
			}
			if operator, propertyType := response.execution.SearchRestrictionType(); operator != "" {
				attributes = append(attributes, "store_search_restriction_operator", operator, "store_search_restriction_property_type", propertyType)
			}
			if reason, propertyType := response.execution.SearchBindingRejection(); reason != "" {
				attributes = append(attributes, "store_search_binding_reason", reason, "store_search_binding_property_type", propertyType)
			}
			if modes := response.execution.CreateMessageModes(); len(modes) != 0 {
				attributes = append(attributes, "store_create_message_modes", modes)
			}
			if folders := response.execution.CreateMessageFolders(); len(folders) != 0 {
				attributes = append(attributes, "store_create_message_folders", folders)
			}
			if requests := response.execution.SubmissionRequests(); len(requests) != 0 {
				attributes = append(attributes, "store_submission_requests", requests)
			}
			if options := response.execution.DeleteMessageOptions(); len(options) != 0 {
				attributes = append(attributes, "store_delete_message_options", options)
			}
			if events, scopes := response.execution.NotificationRegistration(); len(scopes) != 0 {
				attributes = append(attributes, "store_notification_events", events, "store_notification_scopes", scopes)
			}
			if events := response.execution.NotificationDelivery(); len(events) != 0 {
				attributes = append(attributes, "store_notification_deliveries", events)
			}
			if response.notificationWait != "" {
				attributes = append(attributes, "store_notification_wait", response.notificationWait)
			}
			if unknownPropertyTrace {
				attributes = append(attributes, "store_table_profile", response.execution.TableProfiles(), "store_folder_open_profile", response.execution.FolderOpenProfiles())
				if profile := response.execution.HierarchyImportProperties(); len(profile.Tags) != 0 || profile.HighIDs != 0 || profile.ReservedIDs != 0 {
					attributes = append(attributes, "store_hierarchy_import_tags", profile.Tags,
						"store_hierarchy_import_tag_overflow", profile.Overflow, "store_hierarchy_import_high_ids", profile.HighIDs,
						"store_hierarchy_import_reserved_ids", profile.ReservedIDs)
				}
				if properties, objects := response.execution.OpenStreamProfile(); len(properties) != 0 {
					attributes = append(attributes, "store_open_stream_properties", properties, "store_open_stream_objects", objects)
				}
				if columns := response.execution.MailTableIdentity(); len(columns) != 0 {
					attributes = append(attributes, "store_mail_table_identity_columns", columns)
				}
				if operation, reason := response.execution.MalformedROP(); operation != "" {
					attributes = append(attributes, "store_malformed_rop", operation, "store_malformed_rop_reason", reason)
				}
				opened, queried, requested, missing := response.execution.RecoverableTable()
				if opened != "" {
					attributes = append(attributes, "store_recoverable_open_rows", opened)
				}
				if queried != "" {
					attributes = append(attributes, "store_recoverable_query_rows", queried,
						"store_recoverable_columns", requested.Tags, "store_recoverable_missing_columns", missing.Tags,
						"store_recoverable_column_overflow", requested.Overflow,
						"store_recoverable_high_columns", requested.HighIDs, "store_recoverable_reserved_columns", requested.ReservedIDs)
				}
				if modes := response.execution.RecoverablePlacement(); len(modes) != 0 {
					attributes = append(attributes, "store_recoverable_placement", modes)
				}
				if rows, requested, missing := response.execution.AttachmentTable(); rows != "" {
					attributes = append(attributes, "store_attachment_query_rows", rows,
						"store_attachment_columns", requested.Tags, "store_attachment_missing_columns", missing.Tags,
						"store_attachment_column_overflow", requested.Overflow,
						"store_attachment_high_columns", requested.HighIDs, "store_attachment_reserved_columns", requested.ReservedIDs)
				}
			}
		}
		if opts.createFolder {
			inspection, kind, encoding, existing, reserved := response.createFolder.Fields()
			attributes = append(attributes, "store_create_folder_inspection", inspection, "store_create_folder_type", kind, "store_create_folder_encoding", encoding, "store_create_folder_open_existing", existing, "store_create_folder_reserved", reserved)
			if result := response.createFolder.Result(); result != "" {
				attributes = append(attributes, "store_create_folder_result", result)
			}
		}
		if opts.setList {
			inspection, tags, visited, high, reserved := response.setList.Fields()
			attributes = append(attributes, "store_set_list_inspection", inspection, "store_set_list_tags", tags, "store_set_list_visited", visited, "store_set_list_high_ids", high, "store_set_list_reserved_ids", reserved)
		}
		if opts.calendarReferenceSet {
			outcomes, requests := response.calendarReferenceSet.Fields()
			attributes = append(attributes, "store_calendar_set_outcomes", outcomes, "store_calendar_set_requests", requests)
		}
		if metadata, ok := r.Context().Value(activityLogKey{}).(activityLogContext); ok {
			metadata.activity.mu.Lock()
			account := metadata.activity.account
			metadata.activity.mu.Unlock()
			if account == "" {
				account = "unauthenticated"
			}
			attributes = append(attributes, "request_id", metadata.requestID, "account", account)
		}
		logger.Log(r.Context(), level, "outlook_lab_http", attributes...)
	})
}

func laboratoryRoute(path string) string {
	switch strings.ToLower(path) {
	case "/autodiscover/autodiscover.xml":
		return "autodiscover_pox"
	case "/autodiscover/autodiscover.svc":
		return "autodiscover_soap"
	case "/mapi/emsmdb/":
		return "mapi_emsmdb"
	case "/mapi/nspi/":
		return "mapi_nspi"
	case "/ews/exchange.asmx":
		return "ews"
	case "/health/live", "/health/ready":
		return "health"
	default:
		return "other"
	}
}

type labResponse struct {
	http.ResponseWriter
	authenticationFailure                    core.AuthenticationFailureDiagnostic
	status                                   int
	mapiResult                               string
	directoryNames, directoryResolved        string
	propertyTags                             []string
	propertyValues, propertyErrors           string
	ropLayout, ropFirst                      string
	ropUnsupported                           string
	ropFlags                                 []string
	nspiCodepage                             string
	storeLogons, storeFolders, storeErrors   string
	storePropertyNames, storePropertyObjects []string
	storePropertyCounts                      [5]string
	unknownPropertyTrace                     bool
	unknownPropertyTags                      []string
	unknownPropertyCounts                    [3]string
	setPropertyTrace                         bool
	setProperty                              mapi.SetPropertyDiagnostic
	synchronizationTrace                     bool
	synchronizationOptionsTrace              bool
	createFolderTrace                        bool
	setListTrace                             bool
	calendarReferenceSetTrace                bool
	calendarReferenceSet                     mapi.CalendarReferenceSetDiagnostic
	setList                                  mapi.SetListDiagnostic
	createFolder                             mapi.CreateFolderDiagnostic
	synchronizationOptions                   mapi.SynchronizationOptionsDiagnostic
	synchronization                          mapi.SynchronizationDiagnostic
	execution                                mapi.ExecutionDiagnostic
	notificationWait                         string
}

func (w *labResponse) RecordAuthenticationFailure(d core.AuthenticationFailureDiagnostic) {
	w.authenticationFailure = d
}

func (w *labResponse) RecordSynchronizationDiagnostic(d mapi.SynchronizationDiagnostic) {
	if w.synchronizationTrace {
		w.synchronization = d
	}
}

func (w *labResponse) RecordSetPropertyDiagnostic(d mapi.SetPropertyDiagnostic) {
	if w.setPropertyTrace {
		w.setProperty = d
	}
}

// The transport supplies an opaque name set, verified object kinds and counts.
// The opt-in snapshot permits only filtered technical ID/type pairs. Private
// names, values, handles and FIDs cannot cross either diagnostic interface.
func (w *labResponse) RecordStoreProperties(d mapi.StorePropertyDiagnostic) {
	if d.Requests < 1 || d.Requests > 1024 || d.Requested < 0 || d.Requested > d.Requests*256 ||
		d.Values < 0 || d.Errors < 0 || d.Values > d.Requested || d.Errors > d.Requested-d.Values ||
		d.Other < 0 || d.Other > d.Requested {
		w.storePropertyNames, w.storePropertyObjects = []string{"other"}, []string{"other"}
		w.storePropertyCounts = [5]string{"other", "other", "other", "other", "other"}
		if w.unknownPropertyTrace {
			w.unknownPropertyTags, w.unknownPropertyCounts = []string{"other"}, [3]string{"other", "other", "other"}
		}
		return
	}
	w.storePropertyNames, w.storePropertyObjects = d.PropertyNames(), []string{}
	if d.Folder {
		w.storePropertyObjects = append(w.storePropertyObjects, "folder")
	}
	if d.Logon {
		w.storePropertyObjects = append(w.storePropertyObjects, "logon")
	}
	if d.Message {
		w.storePropertyObjects = append(w.storePropertyObjects, "message")
	}
	w.storePropertyCounts = [5]string{strconv.Itoa(d.Requests), strconv.Itoa(d.Requested), strconv.Itoa(d.Values), strconv.Itoa(d.Errors), strconv.Itoa(d.Other)}
	if w.unknownPropertyTrace {
		snapshot := d.UnknownProperties()
		w.unknownPropertyTags = snapshot.Tags
		w.unknownPropertyCounts = [3]string{strconv.Itoa(snapshot.Overflow), strconv.Itoa(snapshot.HighIDs), strconv.Itoa(snapshot.ReservedIDs)}
	}
}

// This snapshot shares the batch's existing technical-tag budget. It may have
// no GetProperties requests; retain their counters and object kinds unchanged.
func (w *labResponse) RecordContentsBindingDiagnostic(d mapi.StorePropertyDiagnostic) {
	if !w.unknownPropertyTrace || !w.synchronizationOptionsTrace || w.storePropertyCounts[0] == "other" {
		return
	}
	for _, name := range d.PropertyNames() {
		found := false
		for _, previous := range w.storePropertyNames {
			if previous == name {
				found = true
				break
			}
		}
		if !found {
			w.storePropertyNames = append(w.storePropertyNames, name)
		}
	}
	snapshot := d.UnknownProperties()
	w.unknownPropertyTags = snapshot.Tags
	w.unknownPropertyCounts = [3]string{strconv.Itoa(snapshot.Overflow), strconv.Itoa(snapshot.HighIDs), strconv.Itoa(snapshot.ReservedIDs)}
}

// A later unsupported opcode may differ from the first inspected operation.
// Accept only the same closed public vocabulary; never request body fragments.
func (w *labResponse) RecordUnsupportedROP(label string) {
	w.ropUnsupported = mapi.SanitizeROPLabel(label)
}

// Only bounded operation counts leave the store engine. No FID, handle, GUID,
// native identifier or caller-supplied string crosses this logging boundary.
func (w *labResponse) RecordStoreResult(logons, folders, failures int) {
	if logons < 0 || folders < 0 || failures < 0 || logons+folders+failures > 1024 {
		w.storeLogons, w.storeFolders, w.storeErrors = "other", "other", "other"
		return
	}
	w.storeLogons, w.storeFolders, w.storeErrors = strconv.Itoa(logons), strconv.Itoa(folders), strconv.Itoa(failures)
}

func (w *labResponse) RecordNSPICodepage(cp uint32, present bool) {
	if !present {
		w.nspiCodepage = "none"
		return
	}
	switch cp {
	case 0:
		w.nspiCodepage = "zero"
	case 1200:
		w.nspiCodepage = "winunicode"
	case 1252:
		w.nspiCodepage = "windows_1252"
	case 65001:
		w.nspiCodepage = "utf8"
	default:
		w.nspiCodepage = "other"
	}
}

func (w *labResponse) RecordROPDiagnostic(layout, first string, flags []string) {
	w.ropLayout = "other"
	switch layout {
	case "plain", "packed", "compressed", "xor", "compressed_xor", "packed_xor", "packed_compressed", "packed_compressed_xor", "malformed", "limit":
		w.ropLayout = layout
	}
	w.ropFirst = mapi.SanitizeROPLabel(first)
	if len(flags) > 16 {
		w.ropFlags = []string{"limit"}
		return
	}
	w.ropFlags = make([]string, 0, len(flags))
	for _, flag := range flags {
		value := "other"
		switch flag {
		case "private", "undercover", "ghosted", "spooler_process", "other_logon_bits", "admin_privilege", "public", "home_logon", "take_ownership", "alternate_server", "ignore_home_mdb", "no_mail", "per_mdb_replid_mapping", "support_progress", "other_open_bits":
			value = flag
		case "table_depth", "table_deferred_errors", "table_no_notifications", "table_soft_deletes", "table_unicode", "table_suppress_notifications", "table_associated", "table_include_free_busy", "table_flags_zero", "table_other_bits":
			value = flag
		}
		w.ropFlags = append(w.ropFlags, value)
	}
}

// Tags are client-controlled integers. Only these public property/type names
// leave this boundary; unknown numbers cannot become a logging side channel.
func (w *labResponse) RecordNSPIProperties(tags []uint32, defaults bool) {
	if len(tags) > 256 {
		w.propertyTags = []string{"limit"}
		return
	}
	if defaults {
		w.propertyTags = []string{"defaults"}
		return
	}
	w.propertyTags = make([]string, 0, len(tags))
	for _, tag := range tags {
		name := "other"
		switch tag >> 16 {
		case 0x3001:
			name = "display_name"
		case 0x3002:
			name = "address_type"
		case 0x3003:
			name = "email_address"
		case 0x39fe:
			name = "smtp_address"
		case 0x300b:
			name = "search_key"
		case 0x0fff:
			name = "entry_id"
		case 0x0ff9:
			name = "record_key"
		case 0x0ff6:
			name = "instance_key"
		case 0x0ffe:
			name = "object_type"
		case 0x3900:
			name = "display_type"
		case 0x8006:
			name = "home_mdb"
		case 0x8c6d:
			name = "address_book_object_guid"
		case 0x800f:
			name = "proxy_addresses"
		}
		kind := "other"
		switch uint16(tag) {
		case 0:
			kind = "unspecified"
		case 0x0003:
			kind = "int32"
		case 0x000a:
			kind = "error"
		case 0x000d:
			kind = "object"
		case 0x001e:
			kind = "string8"
		case 0x001f:
			kind = "unicode"
		case 0x0102:
			kind = "binary"
		case 0x101e:
			kind = "multi_string8"
		case 0x101f:
			kind = "multi_unicode"
		}
		w.propertyTags = append(w.propertyTags, name+":"+kind)
	}
}

func (w *labResponse) RecordNSPIPropertyResult(values, missing int) {
	if values < 0 || values > 256 || missing < 0 || missing > values {
		w.propertyValues, w.propertyErrors = "other", "other"
		return
	}
	w.propertyValues, w.propertyErrors = strconv.Itoa(values), strconv.Itoa(missing)
}

// RecordDirectoryResult accepts only counts from the NSPI operation. No body,
// name, native ID or MID is passed to the logger, including on invalid input.
func (w *labResponse) RecordDirectoryResult(names, resolved int) {
	if names < 0 || names > 256 || resolved < 0 || resolved > names {
		w.directoryNames, w.directoryResolved = "other", "other"
		return
	}
	w.directoryNames, w.directoryResolved = strconv.Itoa(names), strconv.Itoa(resolved)
}

func (w *labResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *labResponse) WriteHeader(status int) {
	if status < 200 {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	if w.status == 0 {
		w.status = status
		w.ResponseWriter.WriteHeader(status)
	}
}
func (w *labResponse) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

// RecordMAPIResult receives only result codes from the protocol writer. It
// never inspects or buffers response bodies, credentials or cookie values.
func (w *labResponse) RecordMAPIResult(status, method uint32) {
	if status != 0 {
		w.mapiResult = "failure_status"
		return
	}
	switch method {
	case 0:
		w.mapiResult = "success"
	case 1:
		w.mapiResult = "unbind_success"
	case 0x80040102:
		w.mapiResult = "not_supported"
	case 0x8004011e:
		w.mapiResult = "invalid_codepage"
	case 0x80070057:
		w.mapiResult = "invalid_parameter"
	case 0x80070005:
		w.mapiResult = "access_denied"
	case 0x00040380:
		w.mapiResult = "errors_returned"
	case 0x80040403:
		w.mapiResult = "table_too_big"
	case 0x80040405:
		w.mapiResult = "invalid_bookmark"
	default:
		w.mapiResult = "other"
	}
}

// laboratoryAuth classifies only a fixed set of scheme names. Parameters are
// never decoded, returned or logged; even unknown scheme names stay private.
func laboratoryAuth(values []string) (scheme string, parameters, multiple bool) {
	multiple = len(values) > 1
	if len(values) == 0 {
		return "none", false, multiple
	}
	value := strings.TrimSpace(values[0])
	if value == "" {
		return "none", false, multiple
	}
	end := strings.IndexAny(value, " \t")
	if end < 0 {
		end = len(value)
	} else {
		parameters = strings.TrimSpace(value[end:]) != ""
	}
	name := value[:end]
	for _, candidate := range []string{"Basic", "NTLM", "Negotiate", "Bearer", "Anonymous", "Nego2", "Kerberos", "Digest"} {
		if strings.EqualFold(name, candidate) {
			return candidate, parameters, multiple
		}
	}
	return "other", parameters, multiple
}

func (w *labResponse) RecordSynchronizationOptionsDiagnostic(d mapi.SynchronizationOptionsDiagnostic) {
	if w.synchronizationOptionsTrace {
		w.synchronizationOptions = d
	}
}

func (w *labResponse) RecordExecutionDiagnostic(d mapi.ExecutionDiagnostic) {
	if w.synchronizationOptionsTrace {
		w.execution = d
	}
}

func (w *labResponse) RecordNotificationWait(pending bool) {
	if w.synchronizationOptionsTrace {
		w.notificationWait = "no_event"
		if pending {
			w.notificationWait = "event_pending"
		}
	}
}

func (w *labResponse) RecordCreateFolderDiagnostic(d mapi.CreateFolderDiagnostic) {
	if w.createFolderTrace {
		w.createFolder = d
	}
}

func (w *labResponse) RecordSetListDiagnostic(d mapi.SetListDiagnostic) {
	if w.setListTrace {
		w.setList = d
	}
}

func (w *labResponse) RecordCalendarReferenceSetDiagnostic(d mapi.CalendarReferenceSetDiagnostic) {
	if w.calendarReferenceSetTrace {
		w.calendarReferenceSet = d
	}
}
