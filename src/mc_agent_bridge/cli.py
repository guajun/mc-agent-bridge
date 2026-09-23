"""``mc-bridge`` command line: run the daemon, poke it, or expose it over MCP."""

from __future__ import annotations

import argparse
import asyncio
import json
import sys
from typing import Any

from .daemon import DEFAULT_API_PORT, DEFAULT_MOD_PORT, BridgeDaemon
from .local_api import LocalApiClient


def _print(value: Any) -> None:
    print(json.dumps(value, ensure_ascii=False, indent=2))


async def _one_shot(args: argparse.Namespace, method: str, params: dict[str, Any]) -> Any:
    client = LocalApiClient(args.api_host, args.api_port)
    try:
        try:
            await client.connect(retry=False)
        except OSError as error:
            raise SystemExit(
                f"no bridge daemon on {args.api_host}:{args.api_port} ({error}). "
                "Start one with: mc-bridge run"
            ) from error
        return await client.call(method, params, timeout=args.timeout)
    finally:
        await client.close()


def _cmd_run(args: argparse.Namespace) -> int:
    daemon = BridgeDaemon(
        host=args.mod_host,
        mod_port=args.mod_port,
        port_file=args.port_file,
        api_host=args.api_host,
        api_port=args.api_port,
        buffer_size=args.buffer,
        reconnect_delay=args.reconnect_delay,
    )
    try:
        asyncio.run(daemon.run())
    except KeyboardInterrupt:
        print("\n[mc-agent-bridge] stopped")
    return 0


def _cmd_call(args: argparse.Namespace) -> int:
    params = json.loads(args.params) if args.params else {}
    _print(asyncio.run(_one_shot(args, args.method, params)))
    return 0


def _cmd_watch(args: argparse.Namespace) -> int:
    async def run() -> None:
        client = LocalApiClient(args.api_host, args.api_port)
        await client.connect(retry=False)
        events = args.events.split(",") if args.events else ["*"]
        await client.call("subscribe", {"events": events})
        if args.since is not None:
            replay = await client.call("events", {"since": args.since, "limit": args.limit})
            for event in replay["events"]:
                print(json.dumps(event, ensure_ascii=False), flush=True)
        queue = await client.events()
        try:
            while True:
                message = await queue.get()
                print(json.dumps(message["data"], ensure_ascii=False), flush=True)
        finally:
            await client.close()

    try:
        asyncio.run(run())
    except KeyboardInterrupt:
        pass
    return 0


def _cmd_mcp(args: argparse.Namespace) -> int:
    from .mcp_server import main as mcp_main

    mcp_main(transport=args.transport)
    return 0


def _add_api_options(parser: argparse.ArgumentParser) -> None:
    """Loopback API options, accepted before or after the sub-command.

    The sub-parsers use SUPPRESS for their defaults so that omitting the option
    there cannot overwrite a value that was already given before the
    sub-command, e.g. ``mc-bridge --api-port 8799 call status``.
    """
    suppress = len(parser.prog.split()) > 1
    parser.add_argument(
        "--api-host",
        default=argparse.SUPPRESS if suppress else "127.0.0.1",
        help="loopback API host",
    )
    parser.add_argument(
        "--api-port",
        type=int,
        default=argparse.SUPPRESS if suppress else DEFAULT_API_PORT,
        help="loopback API port",
    )
    parser.add_argument(
        "--timeout",
        type=float,
        default=argparse.SUPPRESS if suppress else 60.0,
        help="request timeout in seconds",
    )


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="mc-bridge",
        description="Agent-agnostic bridge between an agent runtime and Minecraft.",
    )
    _add_api_options(parser)
    sub = parser.add_subparsers(dest="command", required=True)

    run = sub.add_parser("run", help="run the bridge daemon in the foreground")
    _add_api_options(run)
    run.add_argument("--mod-host", default="127.0.0.1")
    run.add_argument(
        "--mod-port",
        type=int,
        default=None,
        help=f"interface mod port (default: read port.txt, else {DEFAULT_MOD_PORT})",
    )
    run.add_argument("--port-file", default=None, help="explicit path to the mod's port.txt")
    run.add_argument("--buffer", type=int, default=1000, help="buffered events kept for replay")
    run.add_argument("--reconnect-delay", type=float, default=2.0)
    run.set_defaults(func=_cmd_run)

    call = sub.add_parser("call", help="call one bridge method")
    _add_api_options(call)
    call.add_argument("method")
    call.add_argument("params", nargs="?", help="JSON object of parameters")
    call.set_defaults(func=_cmd_call)

    watch = sub.add_parser("watch", help="stream events as JSON lines")
    _add_api_options(watch)
    watch.add_argument("--events", default="*", help="comma separated: chat,game,mark,sample,error,*")
    watch.add_argument("--since", type=int, default=None, help="replay buffered events after a cursor")
    watch.add_argument("--limit", type=int, default=200)
    watch.set_defaults(func=_cmd_watch)

    mcp = sub.add_parser("mcp", help="serve the bridge over MCP (stdio by default)")
    _add_api_options(mcp)
    mcp.add_argument("--transport", default="stdio")
    mcp.set_defaults(func=_cmd_mcp)

    return parser


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        return int(args.func(args) or 0)
    except KeyboardInterrupt:
        return 130
    except (RuntimeError, ValueError) as error:
        print(f"error: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())
