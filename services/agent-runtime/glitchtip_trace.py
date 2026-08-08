"""Thin GlitchTip (Sentry-compatible) error reporting for the extraction pipeline.

GlitchTip is Sentry-compatible, so we use sentry-sdk (the idiomatic client).
When GLITCHTIP_DSN is unset this is a strict no-op — the pipeline needs no
Sentry server to run. Mirrors langfuse_trace.py's guard pattern.
"""
import os

import sentry_sdk

_USE = bool(os.getenv("GLITCHTIP_DSN"))


def init_glitchtip():
    """Initialize Sentry/GlitchTip from GLITCHTIP_DSN (no-op if unset)."""
    if not _USE:
        return
    sentry_sdk.init(
        dsn=os.getenv("GLITCHTIP_DSN"),
        environment=os.getenv("APP_ENV", "dev"),
        traces_sample_rate=0.0,  # errors only; tracing stays in OTel/Langfuse
    )