from __future__ import annotations

import unittest

from mc_agent_bridge.cli import build_parser


class ParserTests(unittest.TestCase):
    def setUp(self) -> None:
        self.parser = build_parser()

    def test_api_port_is_accepted_before_or_after_the_subcommand(self) -> None:
        self.assertEqual(self.parser.parse_args(["--api-port", "8799", "call", "status"]).api_port, 8799)
        self.assertEqual(self.parser.parse_args(["call", "--api-port", "8799", "status"]).api_port, 8799)
        self.assertEqual(self.parser.parse_args(["run", "--api-port", "8799"]).api_port, 8799)

    def test_subcommand_defaults_do_not_clobber_top_level_options(self) -> None:
        args = self.parser.parse_args(["--api-host", "127.0.0.2", "--api-port", "8799", "call", "status"])
        self.assertEqual(args.api_host, "127.0.0.2")
        self.assertEqual(args.api_port, 8799)

    def test_defaults(self) -> None:
        args = self.parser.parse_args(["call", "status"])
        self.assertEqual(args.api_host, "127.0.0.1")
        self.assertEqual(args.api_port, 8765)
        self.assertEqual(args.timeout, 60.0)

    def test_run_passes_daemon_options(self) -> None:
        args = self.parser.parse_args(
            ["run", "--mod-port", "25580", "--port-file", "port.txt", "--buffer", "10"]
        )
        self.assertEqual(args.mod_port, 25580)
        self.assertEqual(args.port_file, "port.txt")
        self.assertEqual(args.buffer, 10)

    def test_watch_and_mcp_parse(self) -> None:
        self.assertEqual(self.parser.parse_args(["watch", "--events", "chat"]).events, "chat")
        self.assertEqual(self.parser.parse_args(["mcp"]).transport, "stdio")

    def test_run_accepts_server_first_discovery_options(self) -> None:
        args = self.parser.parse_args(
            ["run", "--server-dir", "labs/my-lab", "--port-file", "labs/my-lab/mc-agent-server/port.txt"]
        )
        self.assertEqual(args.server_dir, "labs/my-lab")
        self.assertEqual(args.port_file, "labs/my-lab/mc-agent-server/port.txt")
        self.assertEqual(args.vantage, "server")

    def test_client_vantage_is_an_explicit_override(self) -> None:
        args = self.parser.parse_args(["run", "--vantage", "client"])
        self.assertEqual(args.vantage, "client")
        self.assertEqual(self.parser.parse_args(["mcp", "--vantage", "client"]).vantage, "client")

    def test_discover_parses_without_api_options(self) -> None:
        args = self.parser.parse_args(["discover", "--mod-port", "25581"])
        self.assertEqual(args.mod_port, 25581)
        self.assertEqual(args.vantage, "server")
        self.assertEqual(args.func.__name__, "_cmd_discover")
