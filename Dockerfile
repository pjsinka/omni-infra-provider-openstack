# syntax=docker/dockerfile:1

# build stage
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY api api
COPY cmd cmd
COPY internal internal
# Stamped into the binary via ldflags (internal/version.Tag); the CI
# workflow passes TAG=${{ github.ref_name }}. "unknown" makes a missing
# stamping visible instead of silently reporting a stale version.
ARG TAG="unknown"
RUN CGO_ENABLED=0 go build \
    -ldflags "-X internal/version.Tag=${TAG}" \
    -o /provider ./cmd/omni-infra-provider-openstack

# runtime stage: alpine keeps a debug shell around while brewing;
# switch to scratch/distroless for the published image later.
FROM alpine:3.22
RUN apk add --no-cache ca-certificates
COPY --from=build /provider /usr/local/bin/provider
ENTRYPOINT ["provider"]
