"""Forward bridge events to a user-configured HTTP(S) webhook.

The forwarder is a standalone client of the daemon's loopback API: it
subscribes to the server-vantage event stream and POSTs selected events to one
URL. It is receiver-neutral - it contains no agent runtime, no Harness
knowledge and no model calls - and it only makes *outbound* requests, so the
mod and the local API keep their loopback bindings.

Delivery is best effort and in memory. The queue and retry state are lost when
the forwarder stops or the machine restarts; receivers should treat
``eventId`` as an idempotency key because retries (and a restarted forwarder
that replays nothing) never invent a new id for the same bridge event.

The request contract is documented in the README. In short: POST a JSON body,
sign ``"<timestamp>.<body>"`` with HMAC-SHA256 using the shared secret, and
send the signature, timestamp, event id, event type and attempt number in the
``X-MC-Agent-*`` headers.
"""

from __future__ import annotations

import asyncio
import contextlib
import dataclasses
import hashlib
import hmac
import json
import os
import random
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from collections.abc import Iterable, Mapping
from typing import Any, Callable

from . import __version__
from .local_api import LocalApiClient

#: Event categories forwarded unless the receiver asks for something else.
DEFAULT_EVENTS = ("chat", "game", "mark", "error")
DEFAULT_QUEUE_SIZE = 256
DEFAULT_MAX_ATTEMPTS = 5
DEFAULT_BACKOFF = 1.0
DEFAULT_MAX_BACKOFF = 30.0
DEFAULT_TIMEOUT = 10.0
DEFAULT_RECONNECT_DELAY = 2.0
#: Seconds a receiver should accept after the signing timestamp; kept here so
#: the README, the tests and the receivers all quote the same number.
REPLAY_WINDOW_SECONDS = 300

SIGNATURE_HEADER = "X-MC-Agent-Signature"
TIMESTAMP_HEADER = "X-MC-Agent-Timestamp"
EVENT_ID_HEADER = "X-MC-Agent-Event-Id"
EVENT_TYPE_HEADER = "X-MC-Agent-Event-Type"
ATTEMPT_HEADER = "X-MC-Agent-Attempt"

ENV_URL = "MC_AGENT_WEBHOOK_URL"
ENV_SECRET = "MC_AGENT_WEBHOOK_SECRET"
ENV_EVENTS = "MC_AGENT_WEBHOOK_EVENTS"
ENV_QUEUE = "MC_AGENT_WEBHOOK_QUEUE"
ENV_MAX_ATTEMPTS = "MC_AGENT_WEBHOOK_MAX_ATTEMPTS"
ENV_BACKOFF = "MC_AGENT_WEBHOOK_BACKOFF"
ENV_MAX_BACKOFF = "MC_AGENT_WEBHOOK_MAX_BACKOFF"
ENV_TIMEOUT = "MC_AGENT_WEBHOOK_TIMEOUT"
ENV_CONFIG = "MC_AGENT_WEBHOOK_CONFIG"

#: HTTP statuses worth retrying besides every ``5xx``: a timeout, a locked
#: resource and rate limiting are transient by convention.
RETRY_STATUSES = frozenset({408, 425, 429})


class WebhookConfigError(ValueError):
    """The forwarder is misconfigured; the message is safe to print."""


@dataclasses.dataclass(frozen=True)
class WebhookConfig:
    """Where and how to deliver events. The secret never appears in ``repr``."""

    url: str
    secret: str = dataclasses.field(default="", repr=False)
    events: tuple[str, ...] = DEFAULT_EVENTS
    queue_size: int = DEFAULT_QUEUE_SIZE
    max_attempts: int = DEFAULT_MAX_ATTEMPTS
    backoff: float = DEFAULT_BACKOFF
    max_backoff: float = DEFAULT_MAX_BACKOFF
    timeout: float = DEFAULT_TIMEOUT

    def __post_init__(self) -> None:
        parsed = urllib.parse.urlparse(self.url)
        if parsed.scheme not in ("http", "https") or not parsed.netloc:
            raise WebhookConfigError(
                "webhook url must be an absolute http(s) url, got "
                f"{redact_url(self.url) or '[redacted-url]'}"
            )
        if not self.secret:
            raise WebhookConfigError(
                "webhook forwarding requires a shared secret for HMAC-SHA256 signing"
            )
        if self.queue_size < 1:
            raise WebhookConfigError("webhook queue size must be at least 1")
        if self.max_attempts < 1:
            raise WebhookConfigError("webhook max attempts must be at least 1")
        if self.backoff < 0:
            raise WebhookConfigError("webhook backoff cannot be negative")
        if self.max_backoff < self.backoff:
            raise WebhookConfigError("webhook max backoff cannot be smaller than the base backoff")
        if self.timeout <= 0:
            raise WebhookConfigError("webhook timeout must be positive")

    @property
    def redacted_url(self) -> str:
        """The URL as it is safe to log: no userinfo, path or query string."""
        return redact_url(self.url)


def redact_url(url: str) -> str:
    """Strip everything from a webhook URL except scheme and host.

    Receiver URLs routinely carry a bearer token in the path or query string
    (for example a hosted chat gateway's per-channel URL), so those parts are
    treated as secrets too.
    """
    parsed = urllib.parse.urlparse(url or "")
    if not parsed.scheme or not parsed.netloc:
        return ""
    host = parsed.hostname or ""
    if not host:
        return ""
    if ":" in host and not host.startswith("["):
        host = f"[{host}]"
    return f"{parsed.scheme}://{host}" + (f":{parsed.port}" if parsed.port else "")


def sanitize_for_logs(text: str, config: WebhookConfig) -> str:
    """Remove the shared secret and the full receiver URL from a log line."""
    # Replace the whole URL first: once the secret is replaced inside it, the
    # URL no longer matches and its path/query would survive in the log.
    if config.url:
        text = text.replace(config.url, config.redacted_url or "[redacted-url]")
    if config.secret:
        text = text.replace(config.secret, "[redacted]")
    return text


def sign(secret: str, timestamp: int | str, body: bytes) -> str:
    """The receiver-neutral signature: ``sha256=hex(HMAC(key=secret, msg=t.body))``."""
    message = f"{timestamp}.".encode("utf-8") + body
    digest = hmac.new(secret.encode("utf-8"), message, hashlib.sha256).hexdigest()
    return f"sha256={digest}"


def extract_sender(event: Mapping[str, Any]) -> Any:
    """Best effort sender identity from the raw server event."""
    for key in ("sender", "player", "playerName", "name", "from"):
        value = event.get(key)
        if isinstance(value, str) and value.strip():
            return value.strip()
        if isinstance(value, Mapping):
            nested = value.get("name") or value.get("id")
            if nested not in (None, ""):
                return nested
    return None


def extract_context_id(event: Mapping[str, Any]) -> Any:
    """The stable chat context reference, under any of the spellings the mod may use."""
    for key in ("context_id", "contextId"):
        value = event.get(key)
        if value not in (None, "", {}):
            return value
    context = event.get("context")
    if isinstance(context, Mapping):
        for key in ("id", "context_id", "contextId"):
            value = context.get(key)
            if value not in (None, ""):
                return value
        return None
    if context not in (None, ""):
        return context
    return None


def build_event_body(event: Mapping[str, Any]) -> dict[str, Any]:
    """Turn one buffered bridge event into the JSON body a receiver gets.

    The original event is retained verbatim under ``data``; the top-level fields
    are the stable receiver-facing view. ``eventId`` is derived from the
    daemon's stream id and sequence number when the daemon supplies them, so the
    id does not change across retries (or across a forwarder restart).
    """
    event_id = str(event.get("eventId") or "").strip()
    if not event_id:
        stream_id = str(event.get("streamId") or "").strip()
        sequence = event.get("seq")
        suffix = str(sequence) if sequence is not None else uuid.uuid4().hex
        event_id = f"{stream_id or 'mc-bridge'}:{suffix}"
    category = str(event.get("category") or "other")
    return {
        "eventId": event_id,
        "sequence": event.get("seq"),
        "streamId": event.get("streamId"),
        "event": category,
        "type": event.get("type") or category,
        "category": category,
        "timestamp": event.get("receivedAt") or int(time.time() * 1000),
        "tick": event.get("tick"),
        "sender": extract_sender(event),
        "context_id": extract_context_id(event),
        "data": dict(event),
    }


# --------------------------------------------------------------------------- config


def _read_config_file(path: str | None) -> dict[str, Any]:
    if not path:
        return {}
    if not os.path.isfile(path):
        raise WebhookConfigError(f"webhook config file not found: {path}")
    try:
        with open(path, "r", encoding="utf-8") as handle:
            loaded = json.load(handle)
    except (OSError, ValueError) as error:
        # Never echo file contents: the file holds the shared secret.
        raise WebhookConfigError(f"cannot read webhook config file {path}: {error}") from error
    if not isinstance(loaded, dict):
        raise WebhookConfigError(f"webhook config file {path} must contain a JSON object")
    return loaded


def _parse_events(value: Any, default: tuple[str, ...]) -> tuple[str, ...]:
    if value in (None, ""):
        return default
    if isinstance(value, str):
        raw: Iterable[Any] = value.split(",")
    elif isinstance(value, Iterable):
        raw = value
    else:
        raise WebhookConfigError("webhook events must be a comma separated string or a list")
    events = tuple(dict.fromkeys(str(item).strip() for item in raw if str(item).strip()))
    return events or default


def _number(value: Any, default: float, name: str, cast: Callable[[Any], Any]) -> float:
    if value in (None, ""):
        return default
    try:
        number = cast(value)
    except (TypeError, ValueError) as error:
        raise WebhookConfigError(f"webhook {name} must be a number, got {value!r}") from error
    if number < 0:
        raise WebhookConfigError(f"webhook {name} cannot be negative")
    return number


def load_webhook_config(
    *,
    env: Mapping[str, str] | None = None,
    config_path: str | None = None,
    events: str | None = None,
    queue_size: int | None = None,
    max_attempts: int | None = None,
    backoff: float | None = None,
    max_backoff: float | None = None,
    delivery_timeout: float | None = None,
) -> WebhookConfig:
    """Build a config from file plus environment, with explicit flags last.

    Precedence per field: function argument (a CLI flag) > environment variable
    > JSON config file > built-in default. URL and secret have no CLI flag, so
    they cannot end up in shell history.
    """
    environment: Mapping[str, str] = os.environ if env is None else env
    config_file = config_path or environment.get(ENV_CONFIG)
    file_config = _read_config_file(config_file)

    url = str(environment.get(ENV_URL) or file_config.get("url") or "").strip()
    secret = str(environment.get(ENV_SECRET) or file_config.get("secret") or "")
    if not url and not secret:
        raise WebhookConfigError(
            "webhook forwarding is off by default: set both "
            f"{ENV_URL} and {ENV_SECRET} (or url and secret in the config file)"
        )
    if not url:
        raise WebhookConfigError(
            f"webhook forwarding needs a receiver URL: set {ENV_URL} or url in the config file"
        )
    if not secret:
        raise WebhookConfigError(
            f"webhook forwarding needs a shared secret: set {ENV_SECRET} or secret in the config file"
        )

    chosen_events = events if events is not None else environment.get(ENV_EVENTS)
    if chosen_events in (None, ""):
        chosen_events = file_config.get("events")

    return WebhookConfig(
        url=url,
        secret=secret,
        events=_parse_events(chosen_events, DEFAULT_EVENTS),
        queue_size=int(
            _number(
                queue_size if queue_size is not None else environment.get(ENV_QUEUE) or file_config.get("queue_size"),
                DEFAULT_QUEUE_SIZE,
                "queue size",
                int,
            )
        ),
        max_attempts=int(
            _number(
                max_attempts
                if max_attempts is not None
                else environment.get(ENV_MAX_ATTEMPTS) or file_config.get("max_attempts"),
                DEFAULT_MAX_ATTEMPTS,
                "max attempts",
                int,
            )
        ),
        backoff=float(
            _number(
                backoff if backoff is not None else environment.get(ENV_BACKOFF) or file_config.get("backoff"),
                DEFAULT_BACKOFF,
                "backoff",
                float,
            )
        ),
        max_backoff=float(
            _number(
                max_backoff
                if max_backoff is not None
                else environment.get(ENV_MAX_BACKOFF) or file_config.get("max_backoff"),
                DEFAULT_MAX_BACKOFF,
                "max backoff",
                float,
            )
        ),
        timeout=float(
            _number(
                delivery_timeout
                if delivery_timeout is not None
                else environment.get(ENV_TIMEOUT) or file_config.get("timeout"),
                DEFAULT_TIMEOUT,
                "timeout",
                float,
            )
        ),
    )


# -------------------------------------------------------------------------- forwarder


class _TransientDeliveryError(Exception):
    """A transport failure that is worth retrying."""


@dataclasses.dataclass
class ForwarderStats:
    delivered: int = 0
    retries: int = 0
    failed: int = 0
    dropped: int = 0
    last_event_id: str | None = None
    last_delivered_at: int | None = None
    last_error: str | None = None


def _print_log(message: str) -> None:
    print(message, flush=True)


class WebhookForwarder:
    """Subscribe to the bridge event stream and deliver it to one webhook URL."""

    def __init__(
        self,
        config: WebhookConfig,
        *,
        api_host: str = "127.0.0.1",
        api_port: int = 8765,
        reconnect_delay: float = DEFAULT_RECONNECT_DELAY,
        logger: Callable[[str], None] | None = None,
        rng: random.Random | None = None,
    ) -> None:
        self.config = config
        self.api_host = api_host
        self.api_port = api_port
        self.reconnect_delay = reconnect_delay
        self.stats = ForwarderStats()
        self._logger = logger or _print_log
        self._rng = rng or random.Random()
        self._queue: asyncio.Queue[dict[str, Any]] = asyncio.Queue(maxsize=config.queue_size)
        self._stop = asyncio.Event()
        self._running = False
        self._connected = False
        self._client: LocalApiClient | None = None
        self._started_at: float | None = None

    # ------------------------------------------------------------------ lifecycle

    async def run(self) -> None:
        self._running = True
        self._started_at = time.time()
        self._log(
            f"webhook forwarder started: url={self.config.redacted_url} "
            f"events={','.join(self.config.events)}"
        )
        try:
            while self._running:
                await self._run_connection()
                if self._running:
                    await self._sleep_or_stop(self.reconnect_delay)
        finally:
            self._running = False
            self._connected = False
            self._log(
                "webhook forwarder stopped: delivered={d} retries={r} failed={f} dropped={p}".format(
                    d=self.stats.delivered,
                    r=self.stats.retries,
                    f=self.stats.failed,
                    p=self.stats.dropped,
                )
            )

    async def stop(self) -> None:
        self._running = False
        self._stop.set()
        client = self._client
        if client is not None:
            await client.close()

    async def _run_connection(self) -> None:
        """One bridge API connection: subscribe, pump, deliver until it drops."""
        client = LocalApiClient(self.api_host, self.api_port)
        try:
            await client.connect(retry=False)
        except OSError as error:
            self._log(
                f"bridge API unavailable on {self.api_host}:{self.api_port} ({error}); retrying"
            )
            return
        self._client = client
        pump: asyncio.Task[None] | None = None
        worker: asyncio.Task[None] | None = None
        try:
            await client.call("subscribe", {"events": list(self.config.events)})
            self._connected = True
            self._log(f"connected to bridge API on {self.api_host}:{self.api_port}")
            source = await client.events()
            worker = asyncio.create_task(self._deliver_loop())
            pump = asyncio.create_task(self._pump(source))
            await client.wait_closed()
            self._log("bridge API connection closed; reconnecting")
        except (ConnectionError, OSError, RuntimeError) as error:
            self._log(f"bridge API connection failed: {error}")
        finally:
            self._connected = False
            self._client = None
            for task in (pump, worker):
                if task is not None:
                    task.cancel()
            for task in (pump, worker):
                if task is not None:
                    with contextlib.suppress(asyncio.CancelledError, Exception):
                        await task
            await client.close()

    async def _sleep_or_stop(self, seconds: float) -> None:
        try:
            await asyncio.wait_for(self._stop.wait(), timeout=seconds)
        except (asyncio.TimeoutError, TimeoutError):
            pass

    # --------------------------------------------------------------------- events

    async def _pump(self, source: asyncio.Queue[dict[str, Any]]) -> None:
        """Move events from the unbounded API client queue into the bounded one."""
        while True:
            message = await source.get()
            data = message.get("data")
            if isinstance(data, dict):
                self._enqueue(data)

    def _enqueue(self, event: dict[str, Any]) -> None:
        try:
            self._queue.put_nowait(event)
        except asyncio.QueueFull:
            # Keep the newest event: a slow receiver falls behind either way,
            # and the drop is reported instead of blocking the pump.
            with contextlib.suppress(asyncio.QueueEmpty):
                self._queue.get_nowait()
                self._queue.task_done()
            self._queue.put_nowait(event)
            self.stats.dropped += 1
            self._log(
                f"warning: webhook queue full; dropped {self.stats.dropped} oldest event(s)"
            )

    async def _deliver_loop(self) -> None:
        while True:
            event = await self._queue.get()
            try:
                await self._deliver(event)
            finally:
                self._queue.task_done()

    async def _deliver(self, event: dict[str, Any]) -> bool:
        """POST one event, retrying transient failures with bounded backoff.

        The serialized body is built once and reused for every attempt, which is
        what guarantees a retry carries the same ``eventId`` (and payload).
        Signing is re-done per attempt so the timestamp stays inside a receiver's
        replay window.
        """
        body = build_event_body(event)
        payload = json.dumps(body, ensure_ascii=False).encode("utf-8")
        event_id = str(body["eventId"])
        event_type = str(body.get("type") or body.get("event") or "")
        last_error = "delivery failed"
        for attempt in range(1, self.config.max_attempts + 1):
            timestamp = int(time.time())
            headers = {
                "Content-Type": "application/json; charset=utf-8",
                "User-Agent": f"mc-agent-bridge/{__version__}",
                SIGNATURE_HEADER: sign(self.config.secret, timestamp, payload),
                TIMESTAMP_HEADER: str(timestamp),
                EVENT_ID_HEADER: event_id,
                EVENT_TYPE_HEADER: event_type,
                ATTEMPT_HEADER: str(attempt),
            }
            try:
                status = await asyncio.to_thread(self._post, payload, headers)
            except _TransientDeliveryError as error:
                last_error = str(error)
            else:
                if 200 <= status < 400:
                    self.stats.delivered += 1
                    self.stats.last_event_id = event_id
                    self.stats.last_delivered_at = int(time.time() * 1000)
                    self._log(f"delivered event {event_id} to {self.config.redacted_url} (HTTP {status})")
                    return True
                if status >= 500 or status in RETRY_STATUSES:
                    last_error = f"HTTP {status}"
                else:
                    self.stats.failed += 1
                    self.stats.last_event_id = event_id
                    self.stats.last_error = self._sanitize(f"HTTP {status}")
                    self._log(
                        f"webhook rejected event {event_id} permanently: HTTP {status}; not retrying"
                    )
                    return False
            if attempt < self.config.max_attempts:
                self.stats.retries += 1
                self.stats.last_event_id = event_id
                self.stats.last_error = self._sanitize(last_error)
                delay = self._backoff_for(attempt)
                self._log(
                    f"webhook delivery of {event_id} failed ({self.stats.last_error}); "
                    f"retrying in {delay:.2f}s (attempt {attempt + 1}/{self.config.max_attempts})"
                )
                await asyncio.sleep(delay)

        self.stats.failed += 1
        self.stats.last_event_id = event_id
        self.stats.last_error = self._sanitize(last_error)
        self._log(
            f"webhook gave up on event {event_id} after {self.config.max_attempts} "
            f"attempts: {self.stats.last_error}"
        )
        return False

    def _post(self, payload: bytes, headers: dict[str, str]) -> int:
        request = urllib.request.Request(
            self.config.url, data=payload, headers=headers, method="POST"
        )
        try:
            with urllib.request.urlopen(request, timeout=self.config.timeout) as response:
                response.read()
                return int(response.status)
        except urllib.error.HTTPError as error:
            # A status code is a delivery result, not a transport failure; the
            # caller decides whether 5xx/429 is worth another attempt.
            with contextlib.suppress(Exception):
                error.read()
            with contextlib.suppress(Exception):
                error.close()
            return int(error.code)
        except (urllib.error.URLError, TimeoutError, OSError) as error:
            reason = getattr(error, "reason", error)
            raise _TransientDeliveryError(f"network error: {reason}") from error

    def _backoff_for(self, attempt: int) -> float:
        """Exponential backoff with a hard cap and mild jitter (attempt starts at 1)."""
        exponential = self.config.backoff * (2 ** (attempt - 1))
        jittered = exponential * self._rng.uniform(0.8, 1.25)
        return max(0.0, min(self.config.max_backoff, jittered))

    # --------------------------------------------------------------------- status

    def status(self) -> dict[str, Any]:
        """A log-safe snapshot: receiver URL is redacted, the secret never appears."""
        return {
            "running": self._running,
            "connected": self._connected,
            "url": self.config.redacted_url,
            "events": list(self.config.events),
            "queueSize": self.config.queue_size,
            "queued": self._queue.qsize(),
            "delivered": self.stats.delivered,
            "retries": self.stats.retries,
            "failed": self.stats.failed,
            "dropped": self.stats.dropped,
            "lastEventId": self.stats.last_event_id,
            "lastDeliveredAt": self.stats.last_delivered_at,
            "lastError": self.stats.last_error,
            "startedAt": self._started_at,
        }

    # ---------------------------------------------------------------------- log

    def _log(self, message: str) -> None:
        self._logger(f"[mc-agent-bridge] {self._sanitize(message)}")

    def _sanitize(self, text: str) -> str:
        return sanitize_for_logs(text, self.config)
