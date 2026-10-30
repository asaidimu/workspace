# Makefile — the targets from Workspace specification section 16.
#
#   make test         unit, property, integration and memory contracts (< 60s)
#   make test-anansi  the contract suite against anansi on SQLite
#   make fuzz         the fuzz targets
#   make bench        the benchmarks from section 3
#   make chaos        the failure-injection test, under the chaos build tag
#   make live         the live provider contract, build tag live
#   make golden       regenerate the golden files
#
# CI also runs go vet, staticcheck and govulncheck, and the import-boundary
# test in ./boundary.
#
# There is deliberately no `tidy` target: `go mod tidy` drops the pinned
# blobs and hestia requires until code imports them, and the pins come from
# the brief. Restore them by hand if tidy ever runs.

GO ?= go

.PHONY: test test-anansi fuzz bench chaos live golden vet staticcheck vulncheck build

test:
	$(GO) test -race -shuffle=on -count=1 -timeout 60s ./...

test-anansi:
	$(GO) test -race -count=1 -timeout 300s ./store/anansistore/...

fuzz:
	$(GO) test -run=^$$ -fuzz=. -fuzztime=30s ./...

bench:
	$(GO) test -run=^$$ -bench=. -benchmem ./...

chaos:
	$(GO) test -tags chaos -race -count=1 -timeout 300s ./...

live:
	$(GO) test -tags live -count=1 -timeout 600s ./...

golden:
	$(GO) test -run TestGolden -update ./...

vet:
	$(GO) vet ./...

staticcheck:
	staticcheck ./...

vulncheck:
	govulncheck ./...

build:
	$(GO) build ./...
