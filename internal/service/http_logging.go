// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package service

import (
	"bytes"
	"log"
	"log/slog"
)

// HTTP server errors may contain a peer address, panic value or stack. Keep
// the standard server's reporting path inside the configured logging policy,
// retaining only the server-generated diagnostic category.
type httpErrorWriter struct{ logger *slog.Logger }

func (w httpErrorWriter) Write(data []byte) (int, error) {
	failure := "other"
	switch {
	case bytes.HasPrefix(data, []byte("http: panic serving ")):
		failure = "panic"
	case bytes.HasPrefix(data, []byte("http: TLS handshake error from ")):
		failure = "tls_handshake"
	}
	w.logger.Error("http_server_error", "failure", failure)
	return len(data), nil
}
func httpServerLogger(logger *slog.Logger) *log.Logger {
	return log.New(httpErrorWriter{logger}, "", 0)
}
