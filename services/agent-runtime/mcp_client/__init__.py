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
        self.client = httpx.AsyncClient(
            base_url=self.base_url,
            headers={"Authorization": f"Bearer {self.api_key}"} if self.api_key else {},
            timeout=30.0,
        )

    async def _post(self, tool: str, payload: Dict[str, Any]) -> Dict[str, Any]:
        resp = await self.client.post(f"/mcp/tools/{tool}", json=payload)
        resp.raise_for_status()
        return resp.json()

    async def get_pending_entities(self, client_book_id: str) -> List[Dict[str, Any]]:
        result = await self._post("get_pending_entities", {"client_book_id": client_book_id})
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
        written.

        Called after the LangGraph link node. Each group with ≥1 bank and ≥1 GL
        leg is written via create_entity_link; the API publishes
        verification.requested on creation, so the verify worker evaluates it.
        Groups without bank+gl legs are skipped (not persistable, doc 09).
        """
        written = 0
        for g in groups:
            inv = [str(i) for i in (getattr(g, "invoice_entity_ids", None) or [])]
            bank = [str(i) for i in (getattr(g, "bank_entity_ids", None) or [])]
            gl = [str(i) for i in (getattr(g, "gl_entity_ids", None) or [])]
            if not bank or not gl:
                logger.warning("skipping group without bank+gl legs", group=str(getattr(g, "id", "?")))
                continue
            try:
                resp = await self.create_entity_link(
                    invoice_ids=inv,
                    bank_ids=bank,
                    gl_ids=gl,
                    confidence=getattr(g, "link_confidence", 0.0),
                    status=getattr(g, "status", "needs_review"),
                )
                logger.info("group persisted", group=str(getattr(g, "id", "?")), status=resp.get("status", "?"))
                written += 1
            except Exception as e:
                logger.error("group persist failed", group=str(getattr(g, "id", "?")), error=str(e))
        return written

    async def aclose(self):
        await self.client.aclose()