# SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
# SPDX-License-Identifier: AGPL-3.0-or-later

GO ?= go
GOFMT ?= $(shell $(GO) env GOROOT)/bin/gofmt

.PHONY: build test vet fmt-check check
build:
	$(GO) build -o bin/ewsgo ./cmd/ewsgo
test:
	$(GO) test -race ./...
	python3 -m unittest discover -s harness/mailcraft -p 'test_*.py'
vet:
	$(GO) vet ./...
fmt-check:
	@test -z "$$($(GOFMT) -l cmd internal)" || { echo 'Go files need gofmt'; exit 1; }
check: fmt-check vet test build
