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
