#!/bin/sh
# services/api/docker-entrypoint.sh
#
# Why this exists: internal/documents/clamav.go:46 invokes
#   clamdscan --stream --no-summary <tmpfile>
# with no --config-file, so clamdscan reads /etc/clamav/clamd.conf and nothing
# else. In this deployment clamd is a separate container, so that file has to
# point at it over TCP. Rendering it here (instead of baking it in) keeps the
# daemon's hostname a deployment concern rather than an image rebuild.
#
# Failure mode this protects against: with the Debian default clamd.conf, which
# specifies a LocalSocket, clamdscan exits non-zero -> clamav.go treats that as
# "scan unavailable" -> documents.go:112 REJECTS the upload. Fail-closed is the
# right behaviour, but it would make every upload fail with no obvious cause.

set -eu

CLAMAV_HOST="${CLAMAV_HOST:-clamav}"
CLAMAV_PORT="${CLAMAV_PORT:-3310}"
CLAMD_CONF="/etc/clamav/clamd.conf"

if [ -w "$(dirname "$CLAMD_CONF")" ]; then
    cat > "$CLAMD_CONF" <<EOF
# Rendered by docker-entrypoint.sh — do not edit; changes are lost on restart.
# Client-side config only: this container runs clamdscan, never clamd.
TCPSocket ${CLAMAV_PORT}
TCPAddr ${CLAMAV_HOST}
EOF
else
    echo "warn: $(dirname "$CLAMD_CONF") is not writable; leaving clamd.conf as-is." >&2
    echo "warn: if it still specifies LocalSocket, every upload will be rejected" >&2
    echo "warn: by the fail-closed branch in internal/documents/clamav.go." >&2
fi

exec "$@"
