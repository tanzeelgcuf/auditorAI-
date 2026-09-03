# services/verification/Dockerfile
#
# BUILD CONTEXT IS THE REPOSITORY ROOT — this one is not optional:
#   services/verification/build.rs:2
#     tonic_build::compile_protos("../../proto/verification.proto")
# With `context: ./services/verification` (what infra/docker-compose.yml
# declared before this commit) proto/ is outside the context, the build script
# cannot open the file, and the build fails before rustc ever runs.
#
#   docker build -f services/verification/Dockerfile -t ai-auditor-verification .
#
# protoc IS required. prost-build 0.11+ dropped the vendored protoc and expects
# the binary on PATH; Cargo.lock resolves prost-build via tonic-build 0.10, so
# `protobuf-compiler` is a hard build dependency, not a nicety.
#
# This is the liability-critical service: all money math lives here, rust_decimal
# only, never float (src/decimal_math/mod.rs:3).

# ---------------------------------------------------------------------------
# Stage 1 — build
# ---------------------------------------------------------------------------
# Floating on the newest Rust 1.x rather than an exact patch, deliberately:
# Cargo.lock resolves sentry 0.49.1, whose MSRV is above the 1.77 that
# .github/workflows/ci.yml pinned, and I could not compile here to establish the
# true minimum. Rust's 1.x stability guarantee makes this low-risk, and
# Cargo.lock — not the toolchain tag — is what pins dependency versions. Replace
# with an exact tag once you have one confirmed green.
FROM rust:1-bookworm AS builder

# Native build inputs, each traced to a specific entry in Cargo.lock:
#   protobuf-compiler  prost-build 0.11+ removed the vendored protoc and expects
#                      it on PATH (pulled in here by tonic-build 0.10).
#   pkg-config/libssl  openssl-sys 0.9.117 is in the lock and openssl-src is NOT,
#                      i.e. it links the SYSTEM OpenSSL rather than vendoring
#                      one. It arrives via sentry 0.49.1, whose default features
#                      include transport -> reqwest -> native-tls. Without these
#                      the build fails in openssl-sys's build script, which is a
#                      confusing place to discover a missing apt package.
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
      protobuf-compiler \
      pkg-config \
      libssl-dev; \
    rm -rf /var/lib/apt/lists/*; \
    protoc --version

# The in-image tree mirrors the repo tree exactly, so build.rs's
# "../../proto/verification.proto" resolves the same way it does on a laptop.
# Do not flatten this: cargo runs build scripts with CWD = crate root.
WORKDIR /build/services/verification
COPY proto/ /build/proto/

# Dependency layer: manifest + lock + build script only, compiled against a
# placeholder main so ~200 crates are not rebuilt on every source edit.
COPY services/verification/Cargo.toml services/verification/Cargo.lock ./
COPY services/verification/build.rs ./
RUN set -eux; \
    mkdir -p src; \
    echo 'fn main() {}' > src/main.rs; \
    cargo build --release --locked; \
    rm -rf src

COPY services/verification/src/ ./src/

# touch defeats cargo's mtime fingerprint from the placeholder build; without it
# cargo can decide the crate is up to date and ship the empty binary.
RUN set -eux; \
    touch src/main.rs; \
    cargo build --release --locked; \
    test -x target/release/ai-auditor-verification

# ---------------------------------------------------------------------------
# Stage 2 — runtime
# ---------------------------------------------------------------------------
FROM debian:bookworm-slim

ENV DEBIAN_FRONTEND=noninteractive

# libssl3 is a RUNTIME requirement, not just a build one: openssl-sys links the
# system OpenSSL dynamically (openssl-src absent from Cargo.lock), so a slim base
# without it produces a container that dies on exec with a loader error.
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates libssl3; \
    rm -rf /var/lib/apt/lists/*; \
    useradd --system --uid 10002 --home /nonexistent --shell /usr/sbin/nologin app

WORKDIR /app

COPY --from=builder /build/services/verification/target/release/ai-auditor-verification \
                    /usr/local/bin/verification

# The decision graphs are the rule source of truth: src/zen/mod.rs:93 SHA-256s
# this file's bytes into rule_version, which is what makes a finding traceable
# to the exact rule that produced it. They belong in the image, not on a mount —
# a mounted graph means rule_version identifies something you cannot reproduce
# from the image digest.
COPY services/verification/decision-graphs/ /app/decision-graphs/

USER app

ENV ZEN_DECISION_GRAPH_PATH=/app/decision-graphs/gl_reconciliation.json \
    GRPC_ADDR="[::]:50052" \
    RUST_LOG=info

EXPOSE 50052

ENTRYPOINT ["/usr/local/bin/verification"]
