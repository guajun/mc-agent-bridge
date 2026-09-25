from __future__ import annotations

import asyncio
import contextlib
import json
import os
import random
import tempfile
import unittest
from pathlib import Path

from mc_agent_bridge.daemon import BridgeDaemon
from mc_agent_bridge.webhook import (
    ATTEMPT_HEADER,
    DEFAULT_EVENTS,
    ENV_BACKOFF,
    ENV_EVENTS,
    ENV_MAX_ATTEMPTS,
    ENV_MAX_BACKOFF,
    ENV_QUEUE,
    ENV_SECRET,
    ENV_TIMEOUT,
    ENV_URL,
    EVENT_ID_HEADER,
    EVENT_TYPE_HEADER,
    SIGNATURE_HEADER,
    TIMESTAMP_HEADER,
    WebhookConfig,
    WebhookConfigError,
    WebhookForwarder,
    build_event_body,
    extract_context_id,
    extract_sender,
    load_webhook_config,
    redact_url,
    sanitize_for_logs,
)

from .fake_mod import FakeMod
from .fake_webhook import FakeWebhookReceiver, verify_signature


async def wait_for(predicate, timeout: float = 5.0) -> None:
    deadline = asyncio.get_running_loop().time() + timeout
    while asyncio.get_running_loop().time() < deadline:
        if predicate():
            return
        await asyncio.sleep(0.02)
    raise AssertionError("condition not met in time")


def make_config(**overrides: object) -> WebhookConfig:
    values: dict[str, object] = {
        "url": "http://127.0.0.1:9/hook",
        "secret": "test-secret",
        "events": ("chat",),
        "backoff": 0.01,
        "max_backoff": 0.02,
        "max_attempts": 3,
        "timeout": 2.0,
    }
    values.update(overrides)
    return WebhookConfig(**values)  # type: ignore[arg-type]


class RedactionTests(unittest.TestCase):
    def test_redact_url_keeps_only_scheme_host_port(self) -> None:
        self.assertEqual(
            redact_url("https://user:pass@example.test:8443/hooks/token?token=abc#frag"),
            "https://example.test:8443",
        )
        self.assertEqual(redact_url("http://127.0.0.1:8080/hook"), "http://127.0.0.1:8080")
        self.assertEqual(redact_url("not a url"), "")

    def test_sanitize_for_logs_replaces_secret_and_full_url(self) -> None:
        config = WebhookConfig(url="https://example.test/hook?token=swordfish", secret="swordfish")

        text = sanitize_for_logs(
            "failed https://example.test/hook?token=swordfish with swordfish", config
        )

        self.assertNotIn("swordfish", text)
        self.assertIn("https://example.test", text)

    def test_config_repr_never_shows_the_secret(self) -> None:
        config = make_config(secret="do-not-print-me")
        self.assertNotIn("do-not-print-me", repr(config))


class ConfigTests(unittest.TestCase):
    def _write(self, data: dict[str, object]) -> str:
        handle = tempfile.NamedTemporaryFile("w", suffix=".json", delete=False, encoding="utf-8")
        with handle:
            json.dump(data, handle)
        self.addCleanup(os.unlink, handle.name)
        return handle.name

    def test_disabled_by_default(self) -> None:
        with self.assertRaises(WebhookConfigError) as caught:
            load_webhook_config(env={})
        message = str(caught.exception)
        self.assertIn("off by default", message)
        self.assertIn(ENV_URL, message)
        self.assertIn(ENV_SECRET, message)

    def test_url_and_secret_are_both_required(self) -> None:
        with self.assertRaises(WebhookConfigError) as caught:
            load_webhook_config(env={ENV_URL: "https://example.test/hook"})
        self.assertIn(ENV_SECRET, str(caught.exception))

        with self.assertRaises(WebhookConfigError) as caught:
            load_webhook_config(env={ENV_SECRET: "secret"})
        self.assertIn(ENV_URL, str(caught.exception))

    def test_env_builds_a_config_and_keeps_the_secret_out_of_repr(self) -> None:
        config = load_webhook_config(
            env={ENV_URL: "https://example.test/hook", ENV_SECRET: "shh"}
        )

        self.assertEqual(config.url, "https://example.test/hook")
        self.assertEqual(config.secret, "shh")
        self.assertEqual(config.events, DEFAULT_EVENTS)
        self.assertNotIn("shh", repr(config))

    def test_file_config_with_env_overrides(self) -> None:
        path = self._write(
            {
                "url": "https://file.example/hook",
                "secret": "file-secret",
                "events": ["game"],
                "queue_size": 7,
            }
        )

        config = load_webhook_config(
            env={ENV_URL: "https://env.example/hook", ENV_SECRET: "env-secret"},
            config_path=path,
        )

        self.assertEqual(config.url, "https://env.example/hook")
        self.assertEqual(config.secret, "env-secret")
        self.assertEqual(config.events, ("game",))
        self.assertEqual(config.queue_size, 7)

    def test_env_and_explicit_arguments_win_over_the_file(self) -> None:
        path = self._write({"url": "https://file.example/hook", "secret": "file-secret"})
        config = load_webhook_config(
            env={ENV_URL: "https://env.example/hook", ENV_SECRET: "env-secret", ENV_QUEUE: "4"},
            config_path=path,
            events="chat,mark",
            queue_size=9,
            max_attempts=2,
        )

        self.assertEqual(config.events, ("chat", "mark"))
        self.assertEqual(config.queue_size, 9)
        self.assertEqual(config.max_attempts, 2)

    def test_env_tuning_values(self) -> None:
        config = load_webhook_config(
            env={
                ENV_URL: "https://example.test/hook",
                ENV_SECRET: "s",
                ENV_EVENTS: "chat, game",
                ENV_QUEUE: "5",
                ENV_MAX_ATTEMPTS: "2",
                ENV_BACKOFF: "0.25",
                ENV_MAX_BACKOFF: "2.5",
                ENV_TIMEOUT: "4",
            }
        )

        self.assertEqual(config.events, ("chat", "game"))
        self.assertEqual(config.queue_size, 5)
        self.assertEqual(config.max_attempts, 2)
        self.assertEqual(config.backoff, 0.25)
        self.assertEqual(config.max_backoff, 2.5)
        self.assertEqual(config.timeout, 4.0)

    def test_invalid_numbers_and_schemes_are_rejected(self) -> None:
        with self.assertRaises(WebhookConfigError):
            load_webhook_config(
                env={ENV_URL: "https://example.test/hook", ENV_SECRET: "s", ENV_QUEUE: "many"}
            )
        with self.assertRaises(WebhookConfigError):
            WebhookConfig(url="ftp://user:swordfish@example.test/hook", secret="s")

    def test_a_bad_scheme_error_does_not_echo_credentials(self) -> None:
        with self.assertRaises(WebhookConfigError) as caught:
            WebhookConfig(url="gopher://user:swordfish@example.test/hook", secret="s")
        self.assertNotIn("swordfish", str(caught.exception))

    def test_missing_config_file_is_reported(self) -> None:
        missing = str(Path(tempfile.gettempdir()) / "mc-agent-bridge-no-such-config.json")
        with self.assertRaises(WebhookConfigError):
            load_webhook_config(env={}, config_path=missing)


class EnvelopeTests(unittest.TestCase):
    def test_body_keeps_the_original_event_and_receiver_fields(self) -> None:
        event = {
            "type": "chat",
            "text": "hello",
            "sender": "Alice",
            "tick": 4211,
            "seq": 7,
            "streamId": "abc123",
            "category": "chat",
            "receivedAt": 1_700_000_000_000,
            "contextId": "ctx-42",
        }

        body = build_event_body(event)

        self.assertEqual(body["eventId"], "abc123:7")
        self.assertEqual(body["sequence"], 7)
        self.assertEqual(body["streamId"], "abc123")
        self.assertEqual(body["event"], "chat")
        self.assertEqual(body["type"], "chat")
        self.assertEqual(body["timestamp"], 1_700_000_000_000)
        self.assertEqual(body["tick"], 4211)
        self.assertEqual(body["sender"], "Alice")
        self.assertEqual(body["context_id"], "ctx-42")
        self.assertEqual(body["data"], event)

    def test_sender_and_context_variants(self) -> None:
        self.assertEqual(extract_sender({"player": {"name": "Bob"}}), "Bob")
        self.assertEqual(extract_sender({"name": "Carol"}), "Carol")
        self.assertEqual(extract_context_id({"context_id": "c-1"}), "c-1")
        self.assertEqual(extract_context_id({"context": {"id": "c-2"}}), "c-2")
        self.assertIsNone(extract_sender({"type": "chat"}))
        self.assertIsNone(extract_context_id({"type": "chat"}))

    def test_legacy_event_without_a_stream_id_gets_a_stable_fallback(self) -> None:
        body = build_event_body({"type": "chat", "seq": 3, "category": "chat"})
        self.assertEqual(body["eventId"], "mc-bridge:3")


class BackoffTests(unittest.TestCase):
    def test_backoff_grows_and_respects_the_cap(self) -> None:
        forwarder = WebhookForwarder(
            make_config(backoff=1.0, max_backoff=3.0), rng=random.Random(1)
        )

        first = forwarder._backoff_for(1)
        second = forwarder._backoff_for(2)
        third = forwarder._backoff_for(3)
        tenth = forwarder._backoff_for(10)

        self.assertGreaterEqual(first, 0.8)
        self.assertLessEqual(first, 1.25)
        self.assertGreaterEqual(second, 1.6)
        self.assertLessEqual(second, 2.5)
        self.assertLess(first, second)
        for delay in (third, tenth):
            self.assertGreater(delay, 0.0)
            self.assertLessEqual(delay, 3.0)


class ForwarderTests(unittest.IsolatedAsyncioTestCase):
    """The forwarder end to end: fake mod -> daemon -> fake webhook receiver."""

    async def asyncSetUp(self) -> None:
        self.mod = FakeMod()
        await self.mod.start()
        self.daemon = BridgeDaemon(mod_port=self.mod.port, api_port=0, reconnect_delay=0.05)
        self.task = asyncio.create_task(self.daemon.run())
        await wait_for(lambda: self.daemon.server.port != 0)
        await wait_for(lambda: self.daemon.connected)
        self.receivers: list[FakeWebhookReceiver] = []
        self.forwarders: list[tuple[WebhookForwarder, asyncio.Task[None]]] = []
        self.logs: list[str] = []

    async def asyncTearDown(self) -> None:
        for forwarder, task in self.forwarders:
            await forwarder.stop()
            with contextlib.suppress(asyncio.CancelledError, TimeoutError):
                await asyncio.wait_for(task, timeout=3)
        for receiver in self.receivers:
            await asyncio.to_thread(receiver.stop)
        await self.daemon.stop()
        with contextlib.suppress(asyncio.CancelledError, TimeoutError):
            await asyncio.wait_for(self.task, timeout=3)
        await self.mod.stop()

    def receiver(self, statuses: list[int] | None = None, delay: float = 0.0) -> FakeWebhookReceiver:
        receiver = FakeWebhookReceiver(statuses=statuses, delay=delay).start()
        self.receivers.append(receiver)
        return receiver

    async def forward(self, config: WebhookConfig) -> WebhookForwarder:
        forwarder = WebhookForwarder(
            config,
            api_port=self.daemon.server.port,
            reconnect_delay=0.05,
            logger=self.logs.append,
        )
        task = asyncio.create_task(forwarder.run())
        self.forwarders.append((forwarder, task))
        await wait_for(lambda: forwarder.status()["connected"])
        return forwarder

    def headers(self, record: dict[str, object]) -> dict[str, str]:
        return record["headers"]  # type: ignore[return-value]

    async def test_delivery_body_signature_and_identity(self) -> None:
        receiver = self.receiver()
        secret = "shared-secret-123"
        await self.forward(make_config(url=receiver.url, secret=secret))

        await self.mod.push(
            {
                "type": "chat",
                "text": "hello webhook",
                "sender": "Alice",
                "tick": 4211,
                "contextId": "ctx-42",
            }
        )
        await wait_for(lambda: len(receiver.snapshot()) == 1)

        record = receiver.snapshot()[0]
        self.assertEqual(record["method"], "POST")
        self.assertEqual(record["path"], "/hook")
        body = json.loads(record["body"].decode("utf-8"))
        headers = self.headers(record)

        self.assertEqual(body["event"], "chat")
        self.assertEqual(body["type"], "chat")
        self.assertEqual(body["data"]["text"], "hello webhook")
        self.assertEqual(body["sender"], "Alice")
        self.assertEqual(body["context_id"], "ctx-42")
        self.assertEqual(body["tick"], 4211)
        self.assertEqual(body["streamId"], self.daemon.stream_id)
        self.assertEqual(body["eventId"], f"{self.daemon.stream_id}:{body['sequence']}")
        self.assertIsInstance(body["timestamp"], int)

        self.assertEqual(headers[EVENT_ID_HEADER.lower()], body["eventId"])
        self.assertEqual(headers[EVENT_TYPE_HEADER.lower()], "chat")
        self.assertEqual(headers[ATTEMPT_HEADER.lower()], "1")
        self.assertTrue(
            verify_signature(
                secret,
                headers[TIMESTAMP_HEADER.lower()],
                record["body"],
                headers[SIGNATURE_HEADER.lower()],
            )
        )

    async def test_retries_reuse_the_event_id_and_resign_each_attempt(self) -> None:
        receiver = self.receiver(statuses=[500, 503, 200])
        secret = "retry-secret"
        forwarder = await self.forward(make_config(url=receiver.url, secret=secret))

        await self.mod.push({"type": "chat", "text": "retry me", "sender": "Bob"})
        await wait_for(lambda: forwarder.status()["delivered"] == 1)

        records = receiver.snapshot()
        self.assertEqual(len(records), 3)
        event_ids = {self.headers(record)[EVENT_ID_HEADER.lower()] for record in records}
        self.assertEqual(len(event_ids), 1)
        bodies = [json.loads(record["body"].decode("utf-8")) for record in records]
        self.assertEqual({body["eventId"] for body in bodies}, event_ids)
        self.assertEqual(
            [self.headers(record)[ATTEMPT_HEADER.lower()] for record in records],
            ["1", "2", "3"],
        )
        for record in records:
            headers = self.headers(record)
            self.assertTrue(
                verify_signature(
                    secret,
                    headers[TIMESTAMP_HEADER.lower()],
                    record["body"],
                    headers[SIGNATURE_HEADER.lower()],
                )
            )

        status = forwarder.status()
        self.assertEqual(status["retries"], 2)
        self.assertEqual(status["failed"], 0)
        self.assertEqual(status["lastEventId"], event_ids.pop())

    async def test_permanent_4xx_is_not_retried_and_logs_stay_secret_free(self) -> None:
        receiver = self.receiver(statuses=[404])
        secret = "never-print-me"
        forwarder = await self.forward(make_config(url=receiver.url, secret=secret))

        await self.mod.push({"type": "chat", "text": "nope", "sender": "Bob"})
        await wait_for(lambda: forwarder.status()["failed"] == 1)

        self.assertEqual(len(receiver.snapshot()), 1)
        self.assertEqual(forwarder.status()["retries"], 0)
        log_text = "\n".join(self.logs)
        self.assertIn("404", log_text)
        self.assertNotIn(secret, log_text)
        self.assertNotIn("/hook", log_text)
        self.assertNotIn(secret, json.dumps(forwarder.status()))

    async def test_queue_overflow_is_counted_and_logged(self) -> None:
        receiver = self.receiver(delay=0.3)
        config = make_config(
            url=receiver.url,
            secret="overflow-secret",
            queue_size=1,
            max_attempts=1,
        )
        forwarder = await self.forward(config)

        for index in range(4):
            await self.mod.push({"type": "chat", "text": f"m{index}", "sender": "P"})

        await wait_for(lambda: forwarder.status()["dropped"] >= 1)
        self.assertIn("queue full", "\n".join(self.logs))
        self.assertGreaterEqual(forwarder.status()["queued"], 0)

    async def test_only_configured_categories_are_forwarded(self) -> None:
        receiver = self.receiver()
        await self.forward(make_config(url=receiver.url, secret="filter-secret", events=("chat",)))

        await self.mod.push({"type": "mark", "text": "not me"})
        await asyncio.sleep(0.15)
        self.assertEqual(receiver.snapshot(), [])

        await self.mod.push({"type": "chat", "text": "me", "sender": "A"})
        await wait_for(lambda: len(receiver.snapshot()) == 1)
        body = json.loads(receiver.snapshot()[0]["body"].decode("utf-8"))
        self.assertEqual(body["event"], "chat")

    async def test_status_redacts_the_receiver_url(self) -> None:
        receiver = self.receiver()
        secret = "status-secret"
        forwarder = await self.forward(
            make_config(url=f"{receiver.url}?token={secret}", secret=secret)
        )

        status = forwarder.status()
        self.assertEqual(status["url"], redact_url(receiver.url))
        self.assertNotIn(secret, json.dumps(status))
