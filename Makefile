SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c

.PHONY: test fmt fmt-check vet staticcheck gosec vuln secure check build

# GOWORK is off: every check verifies this module against its real pinned
# dependencies. Override it only to develop against an unreleased dependency
# (GOWORK=/path/to/throwaway/go.work make check); a release is never verified
# that way.
# Every go command is spelled through $(GO), which names GOWORK explicitly:
# `export` does not reach $(shell ...) in the make that ships with macOS.
GOWORK ?= off
GO := GOWORK=$(GOWORK) go

# Resolve gofmt from the module's Go baseline instead of trusting the default
# formatter on PATH. The same pinned version is selected by setup-go in CI.
GOFMT := $(shell GOTOOLCHAIN=go1.26.8 $(GO) env GOROOT)/bin/gofmt

# PIPEFAIL is stated in each piping recipe rather than inherited from
# .SHELLFLAGS: macOS ships GNU make 3.81, which ignores .SHELLFLAGS silently,
# and a pipeline would then report only its last stage's status.
PIPEFAIL := set -o pipefail;

test:
	$(GO) test -race ./...

fmt:
	$(PIPEFAIL) find . -type f -name '*.go' -not -path './.git/*' -print0 | xargs -0 $(GOFMT) -w

fmt-check:
	@$(PIPEFAIL) output="$$(mktemp)"; trap 'rm -f "$$output"' EXIT; \
		if ! find . -type f -name '*.go' -not -path './.git/*' -print0 | xargs -0 $(GOFMT) -l >"$$output"; then exit 1; fi; \
		if [[ -s "$$output" ]]; then cat "$$output"; exit 1; fi

vet:
	$(GO) vet ./...

staticcheck:
	$(GO) run honnef.co/go/tools/cmd/staticcheck@v0.8.1 ./...

CHECK_GO_DIRS = $(shell $(GO) list -f '{{.Dir}}' ./...)

gosec:
	$(GO) run github.com/securego/gosec/v2/cmd/gosec@v2.28.0 -quiet $(CHECK_GO_DIRS)

vuln:
	$(GO) mod verify
	$(GO) run golang.org/x/vuln/cmd/govulncheck@v1.6.0 ./...

secure: fmt-check vet staticcheck gosec vuln

build:
	$(GO) build ./...

check: fmt-check vet staticcheck gosec vuln test build
