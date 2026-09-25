"""Endpoint discovery for the server-vantage mod (and the legacy client vantage).

The mod's server entrypoint writes the port it bound to into
``<gameDir>/mc-agent-server/port.txt`` (the directory is ``mcagent.serverDir``,
default ``mc-agent-server``). The client entrypoint writes
``<gameDir>/mc-agent/port.txt``. The toolkit targets the server vantage by
default because it is the authoritative one and the only one that snapshots.

Discovery never guesses a client port when the server port file is missing:
:func:`resolve_port` returns an unresolved result carrying an actionable error,
and the daemon reports it instead of silently connecting somewhere else.
"""

from __future__ import annotations

import os
from collections.abc import Mapping
from dataclasses import dataclass

VANTAGE_SERVER = "server"
VANTAGE_CLIENT = "client"
VANTAGES = (VANTAGE_SERVER, VANTAGE_CLIENT)

PORT_FILE_NAME = "port.txt"
#: Mirrors ``mcagent.serverDir`` / ``mcagent.dir`` in the interface mod.
SERVER_DIR_NAME = "mc-agent-server"
CLIENT_DIR_NAME = "mc-agent"
#: Mirrors ``mcagent.serverPort`` / ``mcagent.port``.
DEFAULT_SERVER_PORT = 25581
DEFAULT_CLIENT_PORT = 25580

ENV_PORT_FILE = "MC_AGENT_PORT_FILE"
ENV_SERVER_DIR = "MC_AGENT_SERVER_DIR"


@dataclass(frozen=True)
class PortResolution:
    """Where to dial the interface mod, and how that answer was reached."""

    vantage: str
    port: int | None
    source: str
    checked: tuple[str, ...] = ()
    error: str | None = None

    @property
    def resolved(self) -> bool:
        return self.port is not None

    def as_dict(self) -> dict[str, object]:
        return {
            "vantage": self.vantage,
            "port": self.port,
            "source": self.source,
            "checked": list(self.checked),
            "error": self.error,
            "resolved": self.resolved,
        }


def _read_port(path: str) -> tuple[int | None, str | None]:
    """Return ``(port, problem)`` for one candidate file.

    ``(None, None)`` means the file does not exist; a non-empty problem string
    means the file exists but is not a usable TCP port.
    """
    try:
        with open(path, "r", encoding="utf-8") as handle:
            text = handle.read().strip()
    except OSError as error:
        if isinstance(error, FileNotFoundError) or error.errno == 2:
            return None, None
        return None, f"cannot be read ({error})"
    try:
        port = int(text)
    except ValueError:
        return None, f"does not contain a port number (found {text!r})"
    if not 0 < port < 65536:
        return None, f"contains {port}, which is not a valid TCP port (1-65535)"
    return port, None


def _server_error(root: str, checked: list[str], detail: str | None = None) -> PortResolution:
    lines = [
        "cannot find the server-vantage port file"
        + (f": {detail}" if detail else f": no readable {os.path.join(root, SERVER_DIR_NAME, PORT_FILE_NAME)}"),
        "The mod writes it when its server entrypoint starts. Fix one of:",
        f"  --server-dir <game-dir>   where {SERVER_DIR_NAME}/ lives (or set {ENV_SERVER_DIR})",
        "  --port-file <path>        the exact port.txt to read (or set MC_AGENT_PORT_FILE)",
        f"  --mod-port <port>         the port itself ({DEFAULT_SERVER_PORT} by default, or the next free one)",
        "Client-vantage discovery is off by default; pass --vantage client only for an explicit legacy setup.",
    ]
    return PortResolution(
        VANTAGE_SERVER,
        None,
        "unresolved",
        tuple(checked),
        "\n".join(lines),
    )


def _resolve_server(server_dir: str | None) -> PortResolution:
    explicit_dir = server_dir is not None
    root = os.path.abspath(server_dir) if explicit_dir else os.getcwd()
    checked = [os.path.join(root, SERVER_DIR_NAME, PORT_FILE_NAME)]
    # Only when a directory was named explicitly do we also try <dir>/port.txt,
    # so that ``--server-dir ./mc-agent-server`` works. With the default we
    # look *only* at mc-agent-server/port.txt: the bare ./port.txt next to a
    # directory is the legacy client location and must not be picked up here.
    if explicit_dir:
        checked.append(os.path.join(root, PORT_FILE_NAME))
    for path in checked:
        port, problem = _read_port(path)
        if port is not None:
            return PortResolution(VANTAGE_SERVER, port, f"port-file:{path}", tuple(checked))
        if problem is not None:
            return _server_error(root, checked, f"'{path}' {problem}")
    return _server_error(root, checked)


def _resolve_client() -> PortResolution:
    root = os.getcwd()
    checked = [
        os.path.join(root, PORT_FILE_NAME),
        os.path.join(root, CLIENT_DIR_NAME, PORT_FILE_NAME),
    ]
    for path in checked:
        port, problem = _read_port(path)
        if port is not None:
            return PortResolution(VANTAGE_CLIENT, port, f"port-file:{path}", tuple(checked))
        if problem is not None:
            return PortResolution(
                VANTAGE_CLIENT,
                None,
                "unresolved",
                tuple(checked),
                f"the client-vantage port file '{path}' {problem}",
            )
    # The legacy default is intentionally kept for explicit --vantage client
    # users only; the server vantage never guesses.
    return PortResolution(VANTAGE_CLIENT, DEFAULT_CLIENT_PORT, "default-client", tuple(checked))


def resolve_port(
    explicit_port: int | None = None,
    port_file: str | None = None,
    vantage: str = VANTAGE_SERVER,
    server_dir: str | None = None,
    env: Mapping[str, str] | None = None,
) -> PortResolution:
    """Resolve the mod endpoint for one vantage.

    Priority: explicit port, then an explicit port file (argument or
    ``MC_AGENT_PORT_FILE``), then vantage-specific discovery. The server
    vantage fails with a hint rather than falling back to the client port.
    """
    if vantage not in VANTAGES:
        raise ValueError(f"unknown vantage {vantage!r}; expected one of {', '.join(VANTAGES)}")
    environment = os.environ if env is None else env

    if explicit_port is not None:
        port = int(explicit_port)
        if not 0 < port < 65536:
            return PortResolution(
                vantage,
                None,
                "argument",
                error=f"--mod-port {port} is not a valid TCP port (1-65535)",
            )
        return PortResolution(vantage, port, "argument")

    explicit_file = port_file or environment.get(ENV_PORT_FILE) or None
    if explicit_file:
        port, problem = _read_port(explicit_file)
        if port is not None:
            return PortResolution(vantage, port, f"port-file:{explicit_file}", (explicit_file,))
        detail = problem or "does not exist"
        return PortResolution(
            vantage,
            None,
            "port-file",
            (explicit_file,),
            f"the port file '{explicit_file}' {detail}",
        )

    if vantage == VANTAGE_SERVER:
        chosen_dir = server_dir or environment.get(ENV_SERVER_DIR) or None
        return _resolve_server(chosen_dir)
    return _resolve_client()
