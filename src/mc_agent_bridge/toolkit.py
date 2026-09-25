"""The Harness-neutral operation surface: what the bridge can do right now.

The mod is the source of truth for capabilities. Its ``CAPS`` reply (and the
``hello`` frame) lists tokens such as ``state``, ``chat`` or ``snapshot``; the
toolkit maps each operation onto those tokens and hides - or explains -
operations the connected mod cannot serve. That keeps client-vantage tools out
of a server-vantage session, and makes unmerged mod work (per-player context,
chat-time context bundles) appear only once the mod advertises it.

Nothing here talks to the game. These are pure functions over a capability set,
shared by the daemon, the CLI and the MCP front-end.
"""

from __future__ import annotations

from collections.abc import Iterable
from dataclasses import dataclass

SERVER_VANTAGE = "server"
CLIENT_VANTAGE = "client"

#: The capability tokens the released mod advertises (``InterfaceConstants`` in
#: mc-agent-interface-mod). These fallbacks are used only before the mod has
#: answered; a live CAPS reply always wins.
DEFAULT_SERVER_CAPABILITIES = frozenset(
    {"state", "entities", "command", "wait", "mark", "snapshot", "events:game"}
)
DEFAULT_CLIENT_CAPABILITIES = frozenset(
    {
        "state",
        "entities",
        "command",
        "chat",
        "record",
        "wait",
        "screen",
        "mark",
        "connect",
        "world",
        "lan",
        "events:chat",
        "events:game",
    }
)

#: Mod issues the two adaptive operations depend on. The toolkit sends the
#: canonical request line only when the connected mod advertises the
#: capability; until then the operation is reported as unavailable with this
#: dependency instead of being attempted and failing at the game.
INTERFACE_MOD_PLAYER = "https://github.com/guajun/mc-agent-interface-mod/issues/1"
INTERFACE_MOD_PLAYER_LABEL = "mc-agent-interface-mod#1 (Expose server-side context for a named player)"
INTERFACE_MOD_CONTEXT = "https://github.com/guajun/mc-agent-interface-mod/issues/2"
INTERFACE_MOD_CONTEXT_LABEL = "mc-agent-interface-mod#2 (Capture and cache player context with chat events)"

#: Alias groups for capability tokens. Most names are their own only token;
#: the two not-yet-merged APIs are still free to pick a spelling, so the
#: adapter accepts the names the mod issues use. This is the single place to
#: change once they are merged.
CAPABILITY_ALIASES: dict[str, tuple[str, ...]] = {
    "player_context": ("player_context", "playercontext", "player", "context:player"),
    "context_bundle": (
        "context_bundle",
        "contextbundle",
        "context",
        "chat_context",
        "context:chat",
    ),
}


def capability_group(name: str) -> tuple[str, ...]:
    return CAPABILITY_ALIASES.get(name, (name,))


def normalize_capabilities(values: object) -> frozenset[str] | None:
    """Lower-case, de-duplicate and trim a capability list.

    ``None`` (or a non-iterable/string-only shape the mod should not send)
    means "unknown", which the toolkit treats as a legacy mod that cannot be
    filtered. An empty list means "advertises nothing" and filters everything.
    """
    if values is None:
        return None
    if isinstance(values, str):
        values = [values]
    if not isinstance(values, Iterable):
        return None
    tokens = {str(value).strip().lower() for value in values}
    tokens.discard("")
    return frozenset(tokens)


def has_capability(capabilities: frozenset[str] | None, name: str) -> bool:
    if capabilities is None:
        return True  # legacy mod: cannot know, so do not gate
    return any(alias in capabilities for alias in capability_group(name))


@dataclass(frozen=True)
class Operation:
    """One thing the toolkit exposes, and the mod support it needs."""

    name: str
    description: str
    #: Canonical capability names; every one must be advertised.
    requires: tuple[str, ...] = ()
    #: Where the gap comes from when ``requires`` is unmet, if it is not the
    #: released mod - i.e. a still-open mod issue.
    dependency: str | None = None
    dependency_label: str | None = None


OPERATIONS: tuple[Operation, ...] = (
    Operation("status", "Bridge health: mod connection, resolved port/vantage, buffered events."),
    Operation("capabilities", "The connected mod's capabilities and the filtered toolkit surface."),
    Operation("state", "World/server state and the online player list.", ("state",)),
    Operation(
        "player",
        "Server-side context for one player: identity, position, rotation, view target.",
        ("player_context",),
        INTERFACE_MOD_PLAYER,
        INTERFACE_MOD_PLAYER_LABEL,
    ),
    Operation("entities", "Entities the instance ticks, in tick order.", ("entities",)),
    Operation("command", "Run a command as the mod's command source.", ("command",)),
    Operation("command_output", "Run a command and collect the answer it produced.", ("command",)),
    Operation("chat", "Send a chat message as the client-vantage player.", ("chat",)),
    Operation("record_start", "Start per-tick entity sampling (client vantage).", ("record",)),
    Operation("record_stop", "Stop the running sampling session (client vantage).", ("record",)),
    Operation("wait", "Block until the game advanced N ticks.", ("wait",)),
    Operation("screen", "Current client GUI screen (client vantage only).", ("screen",)),
    Operation("mark", "Annotate the event stream with a text marker.", ("mark",)),
    Operation("connect", "Join a server (client vantage only).", ("connect",)),
    Operation("world", "Open a single-player save (client vantage only).", ("world",)),
    Operation("lan", "Publish the single-player world to the LAN (client vantage only).", ("lan",)),
    Operation("events", "Replay buffered events since a cursor."),
    Operation(
        "context",
        "Retrieve a chat-time player context bundle by its context id.",
        ("context_bundle",),
        INTERFACE_MOD_CONTEXT,
        INTERFACE_MOD_CONTEXT_LABEL,
    ),
    Operation("save", "World-save metadata reported by STATE: level, world directory, players.", ("state",)),
    Operation("snapshot", "Write the entity set in tick order to the instance's disk.", ("snapshot",)),
    Operation("snapshots", "List the snapshots already on the connected instance.", ("snapshot",)),
    Operation("fork", "Freeze, snapshot, copy the world files, resume.", ("snapshot", "command")),
    Operation("restore", "Summon a snapshot's entities in the recorded order.", ("command",)),
    Operation("order", "Compare a fresh snapshot's order hash with a saved fork.", ("snapshot",)),
    Operation("stop", "Shut the daemon down."),
)

OPERATION_BY_NAME: dict[str, Operation] = {operation.name: operation for operation in OPERATIONS}


class UnsupportedCapability(RuntimeError):
    """The connected mod does not advertise a capability an operation needs.

    Subclasses :class:`RuntimeError` so the local API, CLI and MCP layers keep
    their existing error path and the message reaches the caller verbatim.
    """

    def __init__(
        self,
        operation: Operation,
        missing: Iterable[str],
        instance: str | None = None,
        vantage: str | None = None,
    ) -> None:
        self.operation = operation
        self.missing = tuple(missing)
        self.instance = instance
        self.vantage = vantage
        super().__init__(self._message())

    def _message(self) -> str:
        where = f"the connected {self.instance}-vantage mod" if self.instance else "the connected mod"
        advertised = ", ".join(repr(name) for name in self.missing)
        if self.operation.dependency:
            return (
                f"toolkit operation {self.operation.name!r} is not available: requires "
                f"{self.operation.dependency_label or self.operation.dependency}; {where} does not "
                f"advertise {advertised}. The toolkit has no fallback for an unmerged mod API, so "
                f"nothing was sent to the game."
            )
        return (
            f"toolkit operation {self.operation.name!r} is not available: {where} does not advertise "
            f"{advertised}. Run `mc-bridge call capabilities` for the supported surface; use "
            f"`--vantage client` only for an explicit legacy client endpoint."
        )


def missing_capabilities(
    operation: Operation, capabilities: frozenset[str] | None
) -> list[str]:
    if capabilities is None:
        return []
    return [name for name in operation.requires if not has_capability(capabilities, name)]


def operation_support(
    operation: Operation,
    capabilities: frozenset[str] | None,
    instance: str | None = None,
) -> dict[str, object]:
    missing = missing_capabilities(operation, capabilities)
    support: dict[str, object] = {
        "name": operation.name,
        "description": operation.description,
        "requires": list(operation.requires),
        "supported": not missing,
    }
    if missing:
        where = f"the connected {instance}-vantage mod" if instance else "the connected mod"
        support["missing"] = missing
        support["reason"] = f"{where} does not advertise {', '.join(missing)}"
    if operation.dependency:
        support["dependency"] = operation.dependency
        support["dependencyLabel"] = operation.dependency_label
    return support


def surface(
    instance: str | None = None,
    capabilities: object = None,
    vantage: str | None = None,
) -> dict[str, object]:
    """Build the filtered operation surface for one mod connection."""
    normalized = normalize_capabilities(capabilities)
    operations = [operation_support(operation, normalized, instance) for operation in OPERATIONS]
    result: dict[str, object] = {
        "instance": instance,
        "vantage": vantage,
        "modCapabilities": sorted(normalized) if normalized is not None else None,
        "supported": [entry["name"] for entry in operations if entry["supported"]],
        "unsupported": [entry["name"] for entry in operations if not entry["supported"]],
        "operations": operations,
    }
    if normalized is None:
        result["note"] = "the mod did not advertise capabilities; the bridge is not filtering its surface"
    return result


def static_supported(vantage: str) -> frozenset[str]:
    """The default surface before/without a live CAPS reply, for one vantage."""
    capabilities = (
        DEFAULT_CLIENT_CAPABILITIES if vantage == CLIENT_VANTAGE else DEFAULT_SERVER_CAPABILITIES
    )
    return frozenset(
        operation.name
        for operation in OPERATIONS
        if not missing_capabilities(operation, capabilities)
    )
