"""Adapter boundary for the server-vantage player and context operations.

The toolkit exposes two operations whose mod APIs shipped in
mc-agent-interface-mod 0.6.0:

* ``player`` - per-player server-side context, including the view ray;
* ``context`` - chat-time context-bundle lookup.

The bridge talks to the mod over the line protocol, so the adapter knows the
canonical request lines and keeps the mapped shapes stable across mod reply
variants. It checks the connected mod's CAPS before sending anything: if the
capability is advertised the adapter sends the canonical line and normalizes
the reply; if it is not, the daemon raises
:class:`~mc_agent_bridge.toolkit.UnsupportedCapability` and no request reaches
the game.

Canonical request lines are ``PLAYER <uuid|name>`` and ``CONTEXT <id>``. Reply
normalization accepts a nested object (``player`` / ``context``), flat fields,
and the released mod's split shape (a ``player`` entity record plus a separate
top-level ``view``), keeps unknown fields out of the model-facing result, and
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


def _player_with_view(reply: dict[str, Any], player: dict[str, Any] | None) -> dict[str, Any] | None:
    """Keep the server-side view with the player it belongs to.

    The released mod answers ``PLAYER`` with the entity record under ``player``
    and the ray under a separate top-level ``view``; this toolkit's contract
    documents ``player.view``. Merge the two without overwriting a view the
    player object already carries, so one accessor works for both shapes.
    """
    if not isinstance(player, dict):
        return player
    view = reply.get("view")
    if not isinstance(view, dict) or isinstance(player.get("view"), dict):
        return player
    merged = dict(player)
    merged["view"] = view
    return merged


def player_context(reply: dict[str, Any], identifier: str | None = None) -> dict[str, Any]:
    """Normalize a mod ``PLAYER`` reply into a stable toolkit result."""
    player = _player_with_view(reply, _nested_or_flat(reply, "player", _PLAYER_FIELDS))
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
