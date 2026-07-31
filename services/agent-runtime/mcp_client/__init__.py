# services/agent-runtime/mcp_client/__init__.py
# MCP client — calls services/api's internal MCP tools.

import os
import structlog
from typing import Any, Dict, List, Optional
import httpx

logger = structlog.get_logger()


class MCPClient:
    """HTTP client for services/api's MCP tool server.

    Tools exposed (per docs 05 §3):
    - get_pending_entities(client_book_id)
    - create_entity_link(invoice_id, bank_id, gl_id, confidence)
    - flag_for_review(entity_link_id, reason)
    - get_book_tolerance(client_book_id)
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
        invoice_id: str,
        bank_id: str,
        gl_id: str,
        confidence: float,
    ) -> Dict[str, Any]:
        return await self._post("create_entity_link", {
            "invoice_id": invoice_id,
            "bank_id": bank_id,
            "gl_id": gl_id,
            "confidence": confidence,
        })

    async def flag_for_review(self, entity_link_id: str, reason: str) -> Dict[str, Any]:
        return await self._post("flag_for_review", {
            "entity_link_id": entity_link_id,
            "reason": reason,
        })

    async def get_book_tolerance(self, client_book_id: str) -> Dict[str, Any]:
        return await self._post("get_book_tolerance", {"client_book_id": client_book_id})

    async def aclose(self):
        await self.client.aclose()
