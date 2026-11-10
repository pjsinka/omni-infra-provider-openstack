# syntax=docker/dockerfile:1

# build stage
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY api api
COPY cmd cmd
COPY internal internal
ARG TAG="v0.1.0-alpha.0"
RUN CGO_ENABLED=0 go build \
    -ldflags "-X internal/version.Tag=${TAG}" \
    -o /provider ./cmd/omni-infra-provider-openstack

# runtime stage: alpine keeps a debug shell around while brewing;
# switch to scratch/distroless for the published image later.
FROM alpine:3.22
RUN apk add --no-cache ca-certificates
COPY --from=build /provider /usr/local/bin/provider
ENTRYPOINT ["provider"]
