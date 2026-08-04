"""Ollama-compatible client adapter for the LangGraph extraction node.

The graph calls `client.messages.create(model, system, messages)` (Anthropic
shape). This adapter exposes the same surface over Ollama's OpenAI-compatible
endpoint, returning an Anthropic-shaped dict (`content: [{type:text, text}]`)
so `_get_response_text` and the rest of the graph need no changes.

Swapping the provider is a one-line change in main.py:
    client = make_llm_client()          # Ollama/OpenAI-compatible
    client = Anthropic(...) if ... else None   # original
"""
import os
from typing import Any, Dict

from openai import OpenAI

OLLAMA_URL = os.getenv("OLLAMA_URL", "http://localhost:11434/v1")
OLLAMA_MODEL = os.getenv("OLLAMA_MODEL", "llama3.2")


class OllamaMessagesAdapter:
    """Exposes `messages.create(...)` over an OpenAI-compatible client."""

    def __init__(self, base_url: str = None, model: str = None):
        # Explicit timeout: a stuck Ollama call (huge book prompt) must fail
        # fast and let the batch be retried, not hang the consumer forever.
        # Default OpenAI client timeout is unbounded-ish for generation.
        self._client = OpenAI(base_url=base_url or OLLAMA_URL, api_key="ollama", timeout=90.0)  # key ignored locally
        self._model = model or OLLAMA_MODEL

    @property
    def messages(self) -> "OllamaMessagesAdapter":
        return self

    def create(self, **kwargs: Any) -> Dict[str, Any]:
        system = kwargs.get("system", "")
        messages = list(kwargs.get("messages", []) or [])
        if system:
            messages = [{"role": "system", "content": system}] + messages

        resp = self._client.chat.completions.create(
            model=self._model,
            max_tokens=kwargs.get("max_tokens", 4000),
            temperature=kwargs.get("temperature", 0),
            messages=messages,
        )
        text = resp.choices[0].message.content or ""
        # Anthropic-shaped response so _get_response_text parses it.
        return {"content": [{"type": "text", "text": text}]}


def make_llm_client() -> Any:
    """Build the LLM client for the graph. Local Ollama by default."""
    return OllamaMessagesAdapter()
