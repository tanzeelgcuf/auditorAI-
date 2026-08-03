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
    # bank+GL-only group is persistable (deposits/fees — doc 09); multi-bank ok
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