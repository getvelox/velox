# Build stage
# Pin the exact patch (not the floating 1.25 tag) so the shipped binary always
# carries the patched stdlib — the build cache otherwise reuses an older 1.25.x
# layer and the image ships CVEs the Grype gate (critical) then rejects.
#
# This pin MUST be >= go.mod's `go` directive. The image runs GOTOOLCHAIN=local,
# so an older builder does not silently download a newer toolchain — it refuses:
# "go.mod requires go >= 1.25.13 (running go 1.25.12)". CI enforces the ordering
# (the go-version-consistency step in .github/workflows/ci.yml), because the
# docker job only runs on main: bumping go.mod in a PR used to leave main's
# image build broken with nothing failing until after the merge.
FROM golang:1.25.13-alpine AS builder

RUN apk add --no-cache git ca-certificates

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# VERSION is stamped by the CI release job (git tag) or defaults to "dev".
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X github.com/sagarsuperuser/velox/internal/version.Version=${VERSION}" \
    -o /velox ./cmd/velox

# Runtime stage — distroless has no shell, no package manager, minimal CVE surface
FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.source="https://github.com/getvelox/velox"
LABEL org.opencontainers.image.description="Velox — usage-based billing engine"
LABEL org.opencontainers.image.licenses="MIT"

COPY --from=builder /velox /usr/local/bin/velox
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

EXPOSE 8080

ENTRYPOINT ["velox"]
