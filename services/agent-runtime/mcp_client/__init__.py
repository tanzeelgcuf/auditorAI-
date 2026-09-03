# services/agent-runtime/mcp_client/__init__.py
# MCP client — calls services/api's internal MCP tools (doc 05 §3).

import os
import structlog
from typing import Any, Dict, List, Optional
import httpx

logger = structlog.get_logger()


class MCPClient:
    """HTTP client for services/api's MCP tool server.

    Tools exposed (per docs 05 §3):
    - get_pending_entities(client_book_id)
    - create_entity_link(invoice_ids, bank_ids, gl_ids, confidence, status)
    - flag_for_review(entity_link_id, reason)
    - get_book_tolerance(client_book_id)
    - persist_groups(groups, client_book_id) — write link output back to the API
    """

    def __init__(self, base_url: Optional[str] = None, api_key: Optional[str] = None):
        self.base_url = (base_url or os.getenv("API_MCP_URL", "http://api:8080")).rstrip("/")
        self.api_key = api_key or os.getenv("API_INTERNAL_KEY", "")
        # Internal MCP auth (doc 05 §3): shared secret on X-Internal-Key, not a
        # user JWT. The API's InternalAuth middleware resolves client_book_id ->
        # firm from the body.
        self.client = httpx.AsyncClient(
            base_url=self.base_url,
            headers={"X-Internal-Key": self.api_key} if self.api_key else {},
            timeout=30.0,
        )

    async def _post(self, tool: str, payload: Dict[str, Any]) -> Dict[str, Any]:
        resp = await self.client.post(f"/mcp/tools/{tool}", json=payload)
        resp.raise_for_status()
        return resp.json()

    async def get_pending_entities(self, client_book_id: str, batch_id: Optional[str] = None) -> List[Dict[str, Any]]:
        """Fetch pending entities. batch_id (a source document id) scopes the
        result to that document — extraction must not span the whole book."""
        payload: Dict[str, Any] = {"client_book_id": client_book_id}
        if batch_id:
            payload["batch_id"] = batch_id
        result = await self._post("get_pending_entities", payload)
        return result.get("entities", [])

    async def create_entity_link(
        self,
        invoice_ids: List[str],
        bank_ids: List[str],
        gl_ids: List[str],
        confidence: float,
        status: str = "needs_review",
    ) -> Dict[str, Any]:
        """Create one reconciliation group. Arrays (a group can have multiple
        entities per leg); bank and gl are required (doc 09)."""
        return await self._post("create_entity_link", {
            "invoice_ids": invoice_ids,
            "bank_ids": bank_ids,
            "gl_ids": gl_ids,
            "confidence": confidence,
            "status": status,
        })

    async def flag_for_review(self, entity_link_id: str, reason: str) -> Dict[str, Any]:
        return await self._post("flag_for_review", {
            "entity_link_id": entity_link_id,
            "reason": reason,
        })

    async def get_book_tolerance(self, client_book_id: str) -> Dict[str, Any]:
        return await self._post("get_book_tolerance", {"client_book_id": client_book_id})

    async def persist_groups(self, groups: List[Any], client_book_id: str) -> int:
        """Persist cross-linked ReconciliationGroups to the API. Returns count
        written. RAISES if any persistable group could not be written.

        Called after the LangGraph link node. Each group with >=1 bank and >=1 GL
        leg is written via create_entity_link; the API publishes
        verification.requested on creation, so the verify worker evaluates it.
        Groups without bank+gl legs are skipped (not persistable, doc 09) and are
        NOT counted as failures.

        WHY THIS RAISES NOW. Every per-group failure used to be caught here and
        logged, and the count of successes was returned regardless. That made the
        caller's retry logic in main.py vacuous: if all 50 groups failed, this
        returned 0, main.py logged "groups persisted written=0" and acked the
        event. The linking work was paid for and thrown away, with one ERROR line
        per group and nothing that retried or surfaced it. Moving the ack decision
        out of main.py accomplished nothing while this swallow was still here.

        WHY IT RAISES AFTER THE LOOP, NOT ON THE FIRST FAILURE. Retrying the whole
        link event is safe and cheap precisely because a retry does not redo the
        groups that succeeded: get_pending_entities filters with
        `AND id NOT IN (SELECT extracted_entity_id FROM reconciliation_group_members)`
        (services/api/internal/mcp/mcp.go:87), so an entity already in a group is
        excluded from the next pass. Finishing the loop therefore maximises forward
        progress per delivery, and the retry re-links only what is left.

        Note the guard being relied on is the pending-entity FILTER, not a
        constraint: reconciliation_group_members is UNIQUE on
        (reconciliation_group_id, extracted_entity_id), which does not stop the
        same entity joining a second group. If that filter is ever relaxed,
        re-running a link pass starts duplicating groups and this rationale no
        longer holds.
        """
        written = 0
        failed: List[str] = []
        for g in groups:
            gid = str(getattr(g, "id", "?"))
            inv = [str(i) for i in (getattr(g, "invoice_entity_ids", None) or [])]
            bank = [str(i) for i in (getattr(g, "bank_entity_ids", None) or [])]
            gl = [str(i) for i in (getattr(g, "gl_entity_ids", None) or [])]
            if not bank or not gl:
                logger.warning("skipping group without bank+gl legs", group=gid)
                continue
            try:
                resp = await self.create_entity_link(
                    invoice_ids=inv,
                    bank_ids=bank,
                    gl_ids=gl,
                    confidence=getattr(g, "link_confidence", 0.0),
                    status=getattr(g, "status", "needs_review"),
                )
                logger.info("group persisted", group=gid, status=resp.get("status", "?"))
                written += 1
            except Exception as e:
                logger.error("group persist failed", group=gid, error=str(e))
                failed.append(gid)
        if failed:
            raise RuntimeError(
                f"{len(failed)} of {len(failed) + written} persistable group(s) failed "
                f"to write for book {client_book_id}: {', '.join(failed[:10])}"
            )
        return written

    async def aclose(self):
        await self.client.aclose()