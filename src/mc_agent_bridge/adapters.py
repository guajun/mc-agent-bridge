"""Adapter boundary for the not-yet-merged server-vantage mod operations.

The toolkit exposes two operations whose mod API is still open work:

* ``player`` - per-player server-side context (mc-agent-interface-mod#1);
* ``context`` - chat-time context-bundle lookup (mc-agent-interface-mod#2).

The bridge talks to the mod over the line protocol, so until those issues
merge the toolkit knows only the request line and reply shape it *expects*. It
checks the connected mod's CAPS before sending anything: if the capability is
advertised the adapter sends the canonical line and normalizes the reply; if it
is not, the daemon raises :class:`~mc_agent_bridge.toolkit.UnsupportedCapability`
and no request reaches the game.

When the mod issues merge, adjust only this file:

* canonical request lines are ``PLAYER <uuid|name>`` and ``CONTEXT <id>``;
* reply normalization accepts a nested object (``player`` / ``context``) or
  flat fields, keeps unknown fields out of the model-facing result, and
  preserves structured ``found``/``status``/``reason`` answers for unknown or
  expired lookups.
"""

from __future__ import annotations

from typing import Any

#: Field names copied from a flat reply when the mod does not nest its payload.
_PLAYER_FIELDS = (
    "uuid",
    "name",
    "dimension",
    "position",
    "pos",
    "rotation",
    "yaw",
    "pitch",
    "view",
    "viewTarget",
    "look",
)
_CONTEXT_FIELDS = (
    "id",
    "contextId",
    "context_id",
    "eventSeq",
    "seq",
    "timestamp",
    "tick",
    "sender",
    "uuid",
    "name",
    "dimension",
    "position",
    "pos",
    "rotation",
    "yaw",
    "pitch",
    "view",
    "viewTarget",
    "look",
    "schema",
    "protocol",
)


def player_line(identifier: str) -> str:
    """The canonical request line for a per-player context lookup."""
    return f"PLAYER {identifier}"


def context_line(context_id: str) -> str:
    """The canonical request line for a chat-time context bundle."""
    return f"CONTEXT {context_id}"


def _nested_or_flat(reply: dict[str, Any], key: str, fields: tuple[str, ...]) -> dict[str, Any] | None:
    nested = reply.get(key)
    if isinstance(nested, dict):
        return nested
    flat = {field: reply[field] for field in fields if field in reply}
    return flat or None


def player_context(reply: dict[str, Any], identifier: str | None = None) -> dict[str, Any]:
    """Normalize a mod ``PLAYER`` reply into a stable toolkit result."""
    player = _nested_or_flat(reply, "player", _PLAYER_FIELDS)
    found = reply.get("found")
    result: dict[str, Any] = {
        "type": "player_context",
        "player": player,
        "found": True if found is None else bool(found),
    }
    uuid = reply.get("uuid") or (player or {}).get("uuid")
    name = reply.get("name") or (player or {}).get("name")
    if identifier and not uuid and not name:
        result["requested"] = identifier
    if uuid:
        result["uuid"] = uuid
    if name:
        result["name"] = name
    for key in ("status", "reason", "message"):
        if key in reply:
            result[key] = reply[key]
    return result


def context_bundle(reply: dict[str, Any], context_id: str) -> dict[str, Any]:
    """Normalize a mod ``CONTEXT`` reply into a stable toolkit result."""
    bundle = _nested_or_flat(reply, "context", _CONTEXT_FIELDS)
    found = reply.get("found")
    status = str(reply.get("status") or "").lower()
    if found is None and status in ("not_found", "expired", "missing", "evicted"):
        found = False
    result: dict[str, Any] = {
        "type": "context_bundle",
        "id": context_id,
        "found": True if found is None else bool(found),
        "context": bundle,
    }
    for key in ("status", "expiresAt", "expired", "reason", "message", "cacheSize", "cacheLimit"):
        if key in reply:
            result[key] = reply[key]
    if not result["found"] and "status" not in result:
        result["status"] = "not_found"
    return result


def context_id(event: dict[str, Any]) -> str | None:
    """The stable chat-event reference to a stored context bundle, if any."""
    for key in ("contextId", "context_id"):
        value = event.get(key)
        if value not in (None, ""):
            return str(value)
    return None
