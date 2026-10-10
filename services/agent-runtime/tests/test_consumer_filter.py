# tests/test_consumer_filter.py
#
# SOURCE-INVARIANT GUARD — no NATS in CI and none in the environment that found
# the bug, so the behavioural equivalent ("the EXTRACTION consumer must never
# receive link.requested events") cannot run as a test. This asserts on the
# TEXT of main.py, the same trade internal/pipeline/verify_worker_test.go makes
# (a jetstream.Msg cannot be built outside a live connection).
#
# What this pins: filter_subject set EXPLICITLY on both subscriptions — a
# contract pin, not a bug fix. nats-py's subscribe() sets filter_subject from
# the subject when the config omits it, verified live 2026-09-20 (a consumer
# created with a config that omitted it was stored by the server as
# filter='entity.extraction.requested', and a link.requested marker published
# afterwards was NOT received). An earlier draft of this header claimed the
# missing filter caused link.requested events to be delivered to the
# EXTRACTION consumer — that claim was FALSIFIED by the same reproduction
# test and by the topology (no stream covers both subjects; ENTITY_EXTRACTION
# 404s). The "batch event missing required fields" lines in the 2026-09-20
# logs were the extraction handler correctly rejecting malformed events
# published to entity.extraction.requested itself — client_book_id present,
# batch_id absent, a payload no version of this repo's Go code publishes.
# The explicit pin is kept because requirements.txt bounds nats-py>=2.7.0 and
# the auto-set behavior is proven only for the installed version. The Go
# coordinator's FilterSubject IS load-bearing (coordinator.go:99-106).
#
# Non-vacuity, run via git archive HEAD into a scratch tree with this file
# copied in: the two filter-matching assertions and the ConsumerConfig
# assertion FAIL there (3 of 4 — the bug reproducing with its named message),
# while test_no_bare_subscribe_without_config_remains PASSES on both trees
# because the pre-fix call sites already passed config=. That is the intended
# split: one guard, three assertions that test the fix.

import re
from pathlib import Path

MAIN_PY = Path(__file__).resolve().parent.parent / "main.py"


def _source() -> str:
    return MAIN_PY.read_text(encoding="utf-8")


def _subscribe_line(src: str, subject: str) -> str:
    """The single js.subscribe call for `subject`, as a source line.

    The filter literal and the subscribe subject must sit on the SAME line —
    that adjacency is the invariant. A reformatted call that separates them
    goes red here rather than silently unpinning the filter.
    """
    lines = [
        line for line in src.splitlines()
        if "js.subscribe(" in line and f'"{subject}"' in line
    ]
    assert len(lines) == 1, (
        f"expected exactly one js.subscribe call for {subject}, found "
        f"{len(lines)} — the consumer was removed, renamed, or the call was "
        "split across lines and this guard can no longer see the filter"
    )
    return lines[0]


def test_extraction_consumer_filter_matches_its_subject():
    line = _subscribe_line(_source(), "entity.extraction.requested")
    assert line.count('"entity.extraction.requested"') >= 2, (
        "the EXTRACTION consumer's filter subject is not the same literal as "
        "its subscribe subject — a filter for any other value (or none) lets "
        "the consumer take every subject on the stream it binds to, and the "
        "link.requested event published by its own publish_link_after path is "
        "delivered back to it (observed live 2026-09-20: 'batch event missing "
        "required fields' on every batch completion)"
    )


def test_link_consumer_filter_matches_its_subject():
    line = _subscribe_line(_source(), "link.requested")
    assert line.count('"link.requested"') >= 2, (
        "the LINK consumer's filter subject is not the same literal as its "
        "subscribe subject — with no filter it takes every subject on the "
        "stream it binds to, including entity.extraction.requested, and every "
        "extraction batch would run the LLM twice"
    )


def test_no_bare_subscribe_without_config_remains():
    src = _source()
    found = 0
    for lineno, line in enumerate(src.splitlines(), 1):
        if "js.subscribe(" in line:
            found += 1
            assert "config=" in line, (
                f"main.py:{lineno}: js.subscribe without config= — a bare "
                "subscription carries no filter_subject and takes every "
                "subject on its stream (the 2026-09-20 cross-delivery bug)"
            )
    assert found >= 2, (
        f"expected both consumer subscriptions in main.py, found {found} "
        "js.subscribe call sites — the consumers were removed or renamed and "
        "this guard now covers nothing"
    )


def test_filter_subject_reaches_the_consumer_config():
    src = _source()
    # The helper must pass filter_subject through to ConsumerConfig — a helper
    # that drops the kwarg silently reintroduces the bug while the call sites
    # still look correct.
    assert re.search(r"ConsumerConfig\([^)]*filter_subject", src, re.DOTALL), (
        "ConsumerConfig is constructed without filter_subject — the call "
        "sites pass a subject to a helper that drops it"
    )
