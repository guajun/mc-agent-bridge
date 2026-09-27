from __future__ import annotations

import os
import tempfile
import unittest

from mc_agent_bridge.discovery import (
    DEFAULT_CLIENT_PORT,
    ENV_PORT_FILE,
    ENV_SERVER_DIR,
    VANTAGE_CLIENT,
    VANTAGE_SERVER,
    resolve_port,
)


def write_port_file(directory: str, name: str, content: str) -> str:
    path = os.path.join(directory, name)
    parent = os.path.dirname(path)
    if parent:
        os.makedirs(parent, exist_ok=True)
    with open(path, "w", encoding="utf-8") as handle:
        handle.write(content)
    return path


class ServerVantageDiscoveryTests(unittest.TestCase):
    def test_default_finds_mc_agent_server_port_file_relative_to_cwd(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            expected = write_port_file(directory, os.path.join("mc-agent-server", "port.txt"), "25591\n")
            cwd = os.getcwd()
            os.chdir(directory)
            try:
                resolution = resolve_port()
            finally:
                os.chdir(cwd)
            self.assertTrue(resolution.resolved)
            self.assertEqual(resolution.vantage, VANTAGE_SERVER)
            self.assertEqual(resolution.port, 25591)
            self.assertEqual(resolution.source, f"port-file:{expected}")

    def test_server_dir_accepts_the_game_dir_or_the_mod_dir(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            write_port_file(directory, os.path.join("mc-agent-server", "port.txt"), "25592")
            resolution = resolve_port(server_dir=directory)
            self.assertEqual(resolution.port, 25592)
            self.assertEqual(resolution.source, f"port-file:{os.path.join(directory, 'mc-agent-server', 'port.txt')}")

        with tempfile.TemporaryDirectory() as directory:
            direct = write_port_file(directory, "port.txt", "25593")
            resolution = resolve_port(server_dir=directory)
            self.assertEqual(resolution.port, 25593)
            self.assertEqual(resolution.source, f"port-file:{direct}")

    def test_env_server_dir_is_used(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            write_port_file(directory, os.path.join("mc-agent-server", "port.txt"), "25594")
            resolution = resolve_port(env={ENV_SERVER_DIR: directory})
            self.assertEqual(resolution.port, 25594)

    def test_explicit_port_beats_any_file(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            write_port_file(directory, os.path.join("mc-agent-server", "port.txt"), "25591")
            resolution = resolve_port(25600, server_dir=directory)
            self.assertEqual(resolution.port, 25600)
            self.assertEqual(resolution.source, "argument")
            self.assertEqual(resolution.checked, ())

    def test_explicit_port_file_beats_discovery(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            explicit = write_port_file(directory, "custom.txt", "25595")
            write_port_file(directory, os.path.join("mc-agent-server", "port.txt"), "25596")
            resolution = resolve_port(port_file=explicit, server_dir=directory)
            self.assertEqual(resolution.port, 25595)
            self.assertEqual(resolution.source, f"port-file:{explicit}")

    def test_port_file_env_var_is_an_explicit_override(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            explicit = write_port_file(directory, "env.txt", "25597")
            resolution = resolve_port(env={ENV_PORT_FILE: explicit})
            self.assertEqual(resolution.port, 25597)
            self.assertEqual(resolution.source, f"port-file:{explicit}")

    def test_missing_server_port_file_is_unresolved_not_a_client_fallback(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            resolution = resolve_port(server_dir=directory)
            self.assertFalse(resolution.resolved)
            self.assertIsNone(resolution.port, "server discovery must not fall back to 25580")
            self.assertEqual(resolution.source, "unresolved")
            error = resolution.error or ""
            # The error must be actionable: name every override the guide documents.
            self.assertIn("--server-dir", error)
            self.assertIn("--port-file", error)
            self.assertIn("--mod-port", error)
            self.assertIn("--vantage client", error)
            self.assertIn(os.path.join(directory, "mc-agent-server", "port.txt"), error)

    def test_invalid_port_file_is_reported_with_the_path(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            path = write_port_file(directory, os.path.join("mc-agent-server", "port.txt"), "not-a-port")
            resolution = resolve_port(server_dir=directory)
            self.assertFalse(resolution.resolved)
            self.assertIn(path, resolution.error or "")
            self.assertIn("not-a-port", resolution.error or "")

        with tempfile.TemporaryDirectory() as directory:
            write_port_file(directory, os.path.join("mc-agent-server", "port.txt"), "70000")
            resolution = resolve_port(server_dir=directory)
            self.assertFalse(resolution.resolved)
            self.assertIn("1-65535", resolution.error or "")

    def test_out_of_range_explicit_port_is_rejected(self) -> None:
        resolution = resolve_port(70000)
        self.assertFalse(resolution.resolved)
        self.assertIn("--mod-port", resolution.error or "")

    def test_unknown_vantage_is_a_programming_error(self) -> None:
        with self.assertRaises(ValueError):
            resolve_port(vantage="somewhere")


class ClientVantageDiscoveryTests(unittest.TestCase):
    def test_legacy_client_default_is_kept_for_the_explicit_override(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            cwd = os.getcwd()
            os.chdir(directory)
            try:
                resolution = resolve_port(vantage=VANTAGE_CLIENT)
            finally:
                os.chdir(cwd)
            self.assertTrue(resolution.resolved)
            self.assertEqual(resolution.port, DEFAULT_CLIENT_PORT)
            self.assertEqual(resolution.source, "default-client")

    def test_legacy_client_reads_port_txt_then_mc_agent_dir(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            write_port_file(directory, os.path.join("mc-agent", "port.txt"), "25601")
            cwd = os.getcwd()
            os.chdir(directory)
            try:
                resolution = resolve_port(vantage=VANTAGE_CLIENT)
            finally:
                os.chdir(cwd)
            self.assertEqual(resolution.port, 25601)
            self.assertIn("mc-agent", resolution.source)
