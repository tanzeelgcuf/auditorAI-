# mcp_client tests — persist_groups group-to-API mapping (offline, no network).
import asyncio
import sys, os
sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from mcp_client import MCPClient
from graph.schema import ReconciliationGroup
from uuid import UUID

BOOK = UUID("11111111-1111-1111-1111-111111111111")


class FakeMCP(MCPClient):
    """Stub that never hits the network; records create_entity_link calls."""
    def __init__(self):
        self.calls = []

    async def create_entity_link(self, invoice_ids, bank_ids, gl_ids, confidence, status):
        self.calls.append((invoice_ids, bank_ids, gl_ids, confidence, status))
        return {"id": "g", "status": status, "link_confidence": confidence}


def test_persist_groups_writes_valid_groups():
    m = FakeMCP()
    g = ReconciliationGroup(
        client_book_id=BOOK,
        invoice_entity_ids=[UUID("a"*32)],
        bank_entity_ids=[UUID("b"*32)],
        gl_entity_ids=[UUID("c"*32)],
        link_confidence=0.95, status="auto_linked",
    )
    n = asyncio.run(m.persist_groups([g], str(BOOK)))
    assert n == 1
    assert len(m.calls) == 1
    inv, bank, gl, conf, status = m.calls[0]
    assert status == "auto_linked"
    assert conf == 0.95
    assert inv == [str(UUID("a"*32))]


def test_persist_groups_skips_no_legs_and_keeps_bank_gl():
    m = FakeMCP()
    # bank+GL-only group is persistable (deposits/fees); multi-bank ok
    ok = ReconciliationGroup(
        client_book_id=BOOK,
        bank_entity_ids=[UUID("b"*32), UUID("d"*32)],
        gl_entity_ids=[UUID("c"*32)],
        link_confidence=0.9, status="needs_review",
    )
    # no legs at all — must skip
    bad = ReconciliationGroup(client_book_id=BOOK, link_confidence=0.5, status="needs_review")
    n = asyncio.run(m.persist_groups([ok, bad], str(BOOK)))
    assert n == 1
    assert len(m.calls) == 1
    inv, bank, gl, _, _ = m.calls[0]
    assert inv == []
    assert len(bank) == 2 and len(gl) == 1
    # Doubles as the negative control for the raise below: a SKIPPED group is not
    # a failed group, so this must not raise. If persist_groups ever raises on an
    # unpersistable group, every link pass over a book with one loose invoice
    # would fail permanently.


class FlakyMCP(MCPClient):
    """Stub whose create_entity_link fails for groups whose bank leg is `f`*32."""
    def __init__(self):
        self.calls = []

    async def create_entity_link(self, invoice_ids, bank_ids, gl_ids, confidence, status):
        self.calls.append((invoice_ids, bank_ids, gl_ids, confidence, status))
        if bank_ids and bank_ids[0] == str(UUID("f"*32)):
            raise RuntimeError("simulated 500 from create_entity_link")
        return {"id": "g", "status": status, "link_confidence": confidence}


def _group(bank_hex, conf=0.9):
    return ReconciliationGroup(
        client_book_id=BOOK,
        bank_entity_ids=[UUID(bank_hex * 32)],
        gl_entity_ids=[UUID("c"*32)],
        link_confidence=conf, status="needs_review",
    )


def test_persist_groups_raises_when_a_group_fails():
    """A failed write must reach the caller.

    Regression guard for the swallow that made main.py's retry logic vacuous:
    persist_groups caught every per-group exception, returned the success count,
    and the consumer acked — so linking ran, produced groups, and discarded them.
    """
    m = FlakyMCP()
    raised = None
    try:
        asyncio.run(m.persist_groups([_group("f")], str(BOOK)))
    except Exception as e:  # noqa: BLE001 - asserting on the type below
        raised = e
    assert raised is not None, "persist_groups swallowed a failed group write"
    assert isinstance(raised, RuntimeError)
    assert "1 of 1" in str(raised), str(raised)


def test_persist_groups_finishes_the_batch_before_raising():
    """Partial progress is kept, and the failure is still reported.

    Both halves matter. Only asserting the raise would also pass if the loop
    aborted on the first failure, which would strand the groups after it: a retry
    re-links them, but each retry pays for the whole link pass again.
    """
    m = FlakyMCP()
    groups = [_group("f"), _group("b"), _group("d")]
    raised = None
    try:
        asyncio.run(m.persist_groups(groups, str(BOOK)))
    except Exception as e:  # noqa: BLE001
        raised = e
    assert raised is not None
    # All three attempted, not just up to the failure.
    assert len(m.calls) == 3, m.calls
    assert "1 of 3" in str(raised), str(raised)