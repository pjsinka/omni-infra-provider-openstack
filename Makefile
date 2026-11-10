MODULE := github.com/pjsinka/omni-infra-provider-openstack
CMD := ./cmd/omni-infra-provider-openstack

.PHONY: build
build:
	go build -o bin/provider $(CMD)

.PHONY: tidy
tidy:
	go mod tidy

.PHONY: vet
vet:
	go vet ./...
