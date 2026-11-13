MODULE := github.com/pjsinka/omni-infra-provider-openstack
CMD := ./cmd/omni-infra-provider-openstack
# Stamped into the binary via ldflags (internal/version.Tag); falls back to
# "dev" when git is unavailable (e.g. tarball builds without a .git dir).
VERSION := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)

.PHONY: build
build:
	go build -ldflags "-X $(MODULE)/internal/version.Tag=$(VERSION)" -o bin/provider $(CMD)

.PHONY: tidy
tidy:
	go mod tidy

.PHONY: vet
vet:
	go vet ./...

.PHONY: test
test:
	go test -race ./...

.PHONY: test-e2e
test-e2e:
	go test -tags=e2e -v -timeout 35m ./e2e/...
