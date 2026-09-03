# services/ingestion/Dockerfile
#
# BUILD CONTEXT IS THE REPOSITORY ROOT — mandatory, same reason as verification:
#   services/ingestion/build.rs:2
#     tonic_build::compile_protos("../../proto/ingestion.proto")
#
#   docker build -f services/ingestion/Dockerfile -t ai-auditor-ingestion .
#
# This service does NOT do OCR itself. It orchestrates the Python docTR sidecar
# over HTTP (OCR_SIDECAR_URL, src/main.rs:25), so none of docTR's system
# libraries belong in this image — that is ocr-sidecar/Dockerfile's problem.

# ---------------------------------------------------------------------------
# Stage 1 — build
# ---------------------------------------------------------------------------
# See services/verification/Dockerfile for why this floats on Rust 1.x instead
# of pinning a patch release.
FROM rust:1-bookworm AS builder

# Native build inputs, each traced to a specific Cargo.lock entry:
#   protobuf-compiler  prost-build 0.11+ needs protoc on PATH (tonic-build 0.10).
#   cmake              aws-lc-sys 0.43.0 (via aws-sdk-s3 1.140.0 -> aws-lc-rs
#                      1.17.3) builds AWS-LC through cmake. bindgen is ABSENT
#                      from the lock, so the pre-generated bindings are in use
#                      and libclang is NOT required.
#   pkg-config/libssl  openssl-sys 0.9.117 with openssl-src absent = links the
#                      system OpenSSL. Reaches here twice over: reqwest 0.12.28
#                      with default features (default-tls -> native-tls) and
#                      sentry 0.49.1's default transport.
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
      protobuf-compiler \
      cmake \
      pkg-config \
      libssl-dev; \
    rm -rf /var/lib/apt/lists/*; \
    protoc --version; \
    cmake --version

# Mirror the repo tree so build.rs's "../../proto/ingestion.proto" resolves
# exactly as it does on a laptop. Cargo runs build scripts with CWD = crate root.
WORKDIR /build/services/ingestion
COPY proto/ /build/proto/

# Dependency layer. This one matters: the aws-sdk-s3 + tonic + image + calamine
# graph is several hundred crates and AWS-LC is a C build. Recompiling it on
# every source edit is the difference between a 30-second and a 10-minute loop.
COPY services/ingestion/Cargo.toml services/ingestion/Cargo.lock ./
COPY services/ingestion/build.rs ./
RUN set -eux; \
    mkdir -p src; \
    echo 'fn main() {}' > src/main.rs; \
    cargo build --release --locked; \
    rm -rf src

COPY services/ingestion/src/ ./src/

# touch defeats cargo's mtime fingerprint from the placeholder build; without it
# cargo can decide the crate is up to date and ship the empty binary.
#
# Note the feature choice: the `enhance` feature is intentionally NOT enabled.
# Cargo.toml documents its stages (deskew/denoise/contrast/binarize) as no-op
# placeholders, gated precisely so they cannot silently degrade OCR output. An
# image built with --all-features would enable them.
RUN set -eux; \
    touch src/main.rs; \
    cargo build --release --locked; \
    test -x target/release/ai-auditor-ingestion

# ---------------------------------------------------------------------------
# Stage 2 — runtime
# ---------------------------------------------------------------------------
FROM debian:bookworm-slim

ENV DEBIAN_FRONTEND=noninteractive

# libssl3 is a runtime requirement: openssl-sys links the system OpenSSL
# dynamically, so without it the container dies on exec with a loader error.
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates libssl3; \
    rm -rf /var/lib/apt/lists/*; \
    useradd --system --uid 10003 --home /nonexistent --shell /usr/sbin/nologin app

WORKDIR /app

COPY --from=builder /build/services/ingestion/target/release/ai-auditor-ingestion \
                    /usr/local/bin/ingestion

USER app

# Defaults match the clap definitions at src/main.rs:22,25,28 so the binary runs
# with no environment at all; compose sets the S3/NATS values explicitly.
ENV GRPC_ADDR="[::]:50051" \
    OCR_SIDECAR_URL=http://ocr-sidecar:8000 \
    NATS_URL=nats://nats:4222 \
    RUST_LOG=info

EXPOSE 50051

ENTRYPOINT ["/usr/local/bin/ingestion"]
