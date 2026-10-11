# tests/test_consumer_crash.py
#
# SOURCE-INVARIANT GUARD — no NATS in CI, so the behavioural equivalent (an
# ack failure killing a consumer, a publish failure losing a book-wide link
# pass) cannot run as a test. This asserts on the TEXT of main.py, the same
# trade internal/pipeline/verify_worker_test.go makes (a jetstream.Msg cannot
# be built outside a live connection).
#
# The bugs it pins, found in the same sweep on 2026-09-22:
#
# 1. The else-branch acked the extraction event BEFORE publishing the
#    follow-up link.requested — a publish failure left the event already
#    acked, so nothing retried the book-wide link pass (rule 5: a NATS
#    message is acked only after its work is durable, and the follow-up
#    publish is part of that work; the coordinator's own link.requested fires
#    BEFORE LLM classification, so the post-classification trigger is the
#    only one).
# 2. An ack/nak/publish failure propagated out of the async-for, gather
#    failed, and main() waited on the stop event forever — a zombie process
#    with a dead pipeline whose only trace was a GC-time "Task exception was
#    never retrieved" (the fail-silent outcome the ack-before-work class
#    exists to prevent, one layer up).
#
# Non-vacuity: run against the pre-fix tree (git archive HEAD into a scratch
# tree, copy this file in) — the assertions below fail there, because
# _ack_with_retry, asyncio.wait in main() and sys.exit(1) do not exist in
# pre-fix main.py, while test_consume_loop_exists passes on both trees (the
# pre-fix code has the same async-for shape). That is the intended split: one
# inertness guard, three assertions that test the fix.

from pathlib import Path

MAIN_PY = Path(__file__).resolve().parent.parent / "main.py"


def _source() -> str:
    return MAIN_PY.read_text(encoding="utf-8")


def test_consume_loop_exists():
    # inertness gate: the loop this file guards must exist, or the assertions
    # below cover nothing after a rename or removal.
    src = _source()
    assert "async for msg in sub.messages" in src, (
        "the consume loop's async-for is gone from main.py — this file's "
        "assertions now cover nothing"
    )


def test_followup_publish_happens_before_ack():
    src = _source()
    publish_pos = src.find("await js.publish(")
    ack_positions = [
        i for i in range(len(src)) if src.startswith("await _ack_with_retry(msg)", i)
    ]
    assert publish_pos != -1, (
        "the follow-up link.requested publish is gone from main.py — the "
        "book-wide link pass would never run after a classification"
    )
    assert len(ack_positions) >= 2, (
        "expected _ack_with_retry on both the undecodable path and the "
        "success path — an unguarded ack reintroduces the silent-death class"
    )
    assert publish_pos < ack_positions[-1], (
        "the success path acks BEFORE publishing the follow-up — a publish "
        "failure then loses the book-wide link pass with the event already "
        "acked and nothing to retry it (rule 5)"
    )


def test_ack_failures_retry_before_giving_up():
    src = _source()
    assert "async def _ack_with_retry" in src, (
        "_ack_with_retry is gone — a bare ack failure kills the consumer "
        "silently and main() never learns"
    )
    assert "range(1, 4)" in src, (
        "the ack retry loop is gone — a single transient transport failure "
        "would crash the process instead of recovering"
    )
    assert "RuntimeError" in src, (
        "the retry-exhaustion raise is gone — an unrecoverable ack failure "
        "would be swallowed instead of re-raised"
    )


def test_main_learns_when_the_consumer_dies():
    src = _source()
    assert "asyncio.wait" in src and "FIRST_COMPLETED" in src, (
        "main() waits only on the stop event — a consumer crash leaves a "
        "zombie process whose only trace is a GC-time 'Task exception was "
        "never retrieved'"
    )
    assert "import sys" in src and "sys.exit(1)" in src, (
        "main() never exits on a consumer crash — the fail-loud requirement "
        "(a dead pipeline must end the process visibly) is unmet"
    )
    assert "consumer task crashed" in src, (
        "the consumer-crash log line is gone — the death would be invisible "
        "even to an operator tailing the log"
    )
