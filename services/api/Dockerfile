# services/api/Dockerfile
#
# BUILD CONTEXT IS THE REPOSITORY ROOT, not services/api:
#   docker build -f services/api/Dockerfile -t ai-auditor-api .
# (compose passes context: .. from infra/). Kept consistent with the two Rust
# images, which *require* a root context to reach proto/.
#
# This image cannot be distroless or scratch. services/api shells out to two
# external binaries at runtime and fails closed / degrades without them:
#   - clamdscan  documents.go:112,325 -> clamav.go:46. Exit 2/126/127 is treated
#                as "scan unavailable" and the upload is REJECTED. No clamdscan
#                means every upload 500s.
#   - typst      findings.go:550 exec.LookPath("typst"). Missing -> the PDF is
#                silently skipped (slog.Warn at :351) and the report degrades to
#                metadata only. That is the current behaviour in this repo.

# ---------------------------------------------------------------------------
# Stage 1 — build the static Go binary
# ---------------------------------------------------------------------------
# go.mod declares `go 1.25.0`; the builder must be >= that or the go command
# spends every build downloading a toolchain.
FROM golang:1.25-bookworm AS builder

WORKDIR /src

# Dependency layer first so code edits don't re-download the module graph.
COPY services/api/go.mod services/api/go.sum ./
RUN go mod download

COPY services/api/ ./

# CGO off: every dependency is pure Go (pgx, chi, x/crypto argon2, aws-sdk-go-v2),
# so the binary is static and the runtime stage needs no libc matching.
# genproto/ is committed, so no protoc is needed for the Go build.
ENV CGO_ENABLED=0 GOOS=linux
RUN go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/server

# seed-demo is the only other main package; it is what loads demo firms/books,
# so it is genuinely useful to have inside the image for smoke tests.
RUN go build -trimpath -ldflags="-s -w" -o /out/seed-demo ./cmd/seed-demo

# ---------------------------------------------------------------------------
# Stage 2 — fetch the typst binary
# ---------------------------------------------------------------------------
# typst is not packaged in Debian bookworm. Upstream ships a static musl build,
# which runs fine on a glibc base precisely because it is static.
#
# NOT CHECKSUM-PINNED. I could not fetch upstream's checksum file from this
# sandbox (egress blocked), and a wrong hardcoded sha256 fails every build. Add
# one before this is production: download the release's .sha256 once, then
# `echo "<sum>  typst.tar.xz" | sha256sum -c -` here.
FROM debian:bookworm-slim AS typst
ARG TYPST_VERSION=0.12.0
ARG TARGETARCH
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates curl xz-utils; \
    rm -rf /var/lib/apt/lists/*; \
    case "${TARGETARCH:-amd64}" in \
      amd64) typst_arch="x86_64" ;; \
      arm64) typst_arch="aarch64" ;; \
      *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    url="https://github.com/typst/typst/releases/download/v${TYPST_VERSION}/typst-${typst_arch}-unknown-linux-musl.tar.xz"; \
    curl -fsSL "$url" -o /tmp/typst.tar.xz; \
    tar -xJf /tmp/typst.tar.xz -C /tmp; \
    mv "/tmp/typst-${typst_arch}-unknown-linux-musl/typst" /usr/local/bin/typst; \
    chmod +x /usr/local/bin/typst; \
    /usr/local/bin/typst --version

# ---------------------------------------------------------------------------
# Stage 3 — runtime
# ---------------------------------------------------------------------------
FROM debian:bookworm-slim

ENV DEBIAN_FRONTEND=noninteractive

# clamdscan is the CLIENT half of ClamAV. The daemon runs as its own compose
# service; this container needs only the client plus a clamd.conf telling it
# where that daemon is. If clamdscan is not on PATH the image is useless — every
# upload would be rejected by the fail-closed branch in
# internal/documents/clamav.go — so the build asserts it below and must fail
# here, loudly, rather than at 3am in staging.
#
# THE ASSERTION FIRED, and this is the CI "Docker Build" failure. Exit 127 was
# read as apt-get being absent (wrong base-image family). It is not: apt-get
# exists on debian:bookworm-slim, and apt-get on an unknown package exits 100,
# not 127. 127 came from `command -v clamdscan` on line 99. Measured, in this
# environment: `/bin/sh` is dash, and dash's `command -v` on a missing name
# exits 127 (bash exits 1) — and docker RUN is `/bin/sh -c`. So apt-get
# SUCCEEDED and clamdscan still was not installed.
#
# The reason is a packaging claim in this comment that was simply false. It used
# to say Debian "has moved clamdscan between the `clamav` and `clamav-daemon`
# binary packages across releases", so installing both was thought to cover it.
# Debian splits the ClamAV source into separate binary packages by ROLE:
# `clamav` ships the command-line scanner (clamscan), `clamav-daemon` ships
# clamd — and `clamdscan` is its own package. Neither of the two installed here
# ships the binary this service actually executes (clamav.go:46 invokes
# clamdscan and nothing else; clamscan is never called).
#
# `clamdscan` is added conditionally rather than appended to the list, because
# there is no apt in the environment this was fixed in: if the package name were
# wrong, an unconditional entry would trade a 127 for a 100 and prove nothing.
# `apt-cache show` decides that from the image's own package index, and the
# `command -v` assertion below is still what passes or fails the build.
#
# curl is here for exactly one reason: the compose healthcheck hits
# /healthz (cmd/server/main.go:205). A distroless-style image has no HTTP client,
# and a service with no healthcheck cannot be a `service_healthy` dependency —
# which web and agent-runtime both need it to be.
RUN set -eux; \
    apt-get update; \
    pkgs="ca-certificates clamav clamav-daemon curl tzdata"; \
    if apt-cache show clamdscan >/dev/null 2>&1; then \
      pkgs="$pkgs clamdscan"; \
    else \
      echo "note: no separate clamdscan package in this base image's index" >&2; \
    fi; \
    apt-get install -y --no-install-recommends $pkgs; \
    rm -rf /var/lib/apt/lists/*; \
    command -v clamdscan; \
    command -v curl

COPY --from=typst /usr/local/bin/typst /usr/local/bin/typst
COPY --from=builder /out/api /usr/local/bin/api
COPY --from=builder /out/seed-demo /usr/local/bin/seed-demo
COPY services/api/docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh

# Reports are written to the local filesystem (findings.go:568, REPORTS_DIR,
# default "reports") and NOT to S3 — without a mounted volume every generated
# PDF is lost on restart, so compose mounts api_reports at the path below.
#
# /etc/clamav must be writable by the app user: the entrypoint renders
# clamd.conf at start, and clamdscan reads only that fixed path (clamav.go:46
# passes no --config-file). mkdir -p first — which ClamAV binary package ships
# that directory is exactly the assumption that produced the 127 above, so this
# no longer depends on one.
RUN set -eux; \
    chmod +x /usr/local/bin/docker-entrypoint.sh; \
    command -v typst; \
    mkdir -p /var/lib/ai-auditor/reports /etc/clamav; \
    useradd --system --uid 10001 --home /nonexistent --shell /usr/sbin/nologin app; \
    chown -R app:app /var/lib/ai-auditor; \
    chown -R app:app /etc/clamav

USER app
WORKDIR /var/lib/ai-auditor

ENV REPORTS_DIR=/var/lib/ai-auditor/reports \
    CLAMAV_HOST=clamav \
    CLAMAV_PORT=3310

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["api"]
