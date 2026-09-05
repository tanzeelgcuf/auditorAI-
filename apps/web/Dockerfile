# apps/web/Dockerfile
#
# BUILD CONTEXT IS THE REPOSITORY ROOT, for consistency with the three service
# images (the two Rust ones *require* it to reach proto/):
#   docker build -f apps/web/Dockerfile -t ai-auditor-web .
#
# Nothing outside apps/web is copied here. apps/web has its own
# package-lock.json and there is no root package.json, so this is a standalone
# npm project, not an npm workspace — which is why .next/standalone lands at its
# own root rather than nested under apps/web/.
#
# THE API URL IS BAKED IN AT BUILD TIME, NOT READ AT RUNTIME. All five call
# sites are client-side (lib/api.ts:4, lib/hooks.ts:223, admin/settings,
# books/[bookId]/documents, books/[bookId]/reports/[reportId]), and Next inlines
# NEXT_PUBLIC_* into the bundle during `next build`. Passing it as a compose
# `environment:` entry does nothing; it has to be a build arg. Each call site
# falls back to http://localhost:8080, so a build with no arg produces a bundle
# that talks to the host's port 8080.

# ---------------------------------------------------------------------------
# Stage 1 — dependencies
# ---------------------------------------------------------------------------
# next 14.2.35 requires Node >= 18.17; 20 is the current LTS. Floating on the
# major and pinning through package-lock.json, same reasoning as the Rust images.
FROM node:20-bookworm-slim AS deps

WORKDIR /app

# npm ci (not install) so the lock file is authoritative and the build is
# reproducible; it fails loudly if package.json and the lock have drifted.
COPY apps/web/package.json apps/web/package-lock.json ./
RUN npm ci

# ---------------------------------------------------------------------------
# Stage 2 — build
# ---------------------------------------------------------------------------
FROM node:20-bookworm-slim AS builder

WORKDIR /app

COPY --from=deps /app/node_modules ./node_modules
COPY apps/web/ ./

# NOTE: `next build` also runs ESLint (.eslintrc.json + eslint-config-next are
# installed by npm ci) and the TypeScript type-checker. A lint or type error
# fails the image build. That is the right default — but it means the first
# build may surface pre-existing errors that `next dev` never enforced.
ARG NEXT_PUBLIC_API_URL=http://localhost:8080
ENV NEXT_PUBLIC_API_URL=$NEXT_PUBLIC_API_URL
ENV NEXT_TELEMETRY_DISABLED=1

RUN set -eux; \
    npm run build; \
    test -f .next/standalone/server.js; \
    test -d .next/static

# ---------------------------------------------------------------------------
# Stage 3 — runtime
# ---------------------------------------------------------------------------
FROM node:20-bookworm-slim

ENV NODE_ENV=production \
    NEXT_TELEMETRY_DISABLED=1

WORKDIR /app

# Two copies, both required:
#   .next/standalone  server.js plus the traced node_modules subset
#   .next/static      standalone deliberately EXCLUDES the static chunks
#
# There is intentionally no `COPY public/`: apps/web has no public/ directory,
# and COPY of a missing path fails the build. app/favicon.ico is handled by the
# app router and ends up inside .next.
#
# Runs as the `node` user (uid 1000) that the base image already provides,
# rather than creating another one — the three service images build on
# debian-slim, which ships no unprivileged user.
COPY --from=builder --chown=node:node /app/.next/standalone ./
COPY --from=builder --chown=node:node /app/.next/static ./.next/static

USER node

# server.js reads PORT and HOSTNAME; HOSTNAME must be 0.0.0.0 (per Next's
# documented standalone example) or the listener is not reachable from outside
# the container's network namespace.
ENV PORT=3000 \
    HOSTNAME=0.0.0.0

EXPOSE 3000

CMD ["node", "server.js"]
