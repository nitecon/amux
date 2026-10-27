#!/usr/bin/env python3
"""Real-process HTTP/storage comparison of a pinned gateway and `amux server`.

All credentials are synthetic. Profiles, SQLite databases, logs and reports stay
under this checkout's target/test-fixtures. Production WhatsApp uses only a loopback fake Graph provider.
Exit zero means the reported probes agree, not that every API happy path is proven.
Use --require-positive-coverage to fail while any declared operation lacks a
successful behavioral fixture. No amux task CLI output is parsed.
"""
from __future__ import annotations

import argparse
import base64
import contextlib
import hashlib
import hmac
import http.client
import http.server
import json
import os
from pathlib import Path
import re
import signal
import shutil
import socket
import sqlite3
import struct
import subprocess
import sys
import tempfile
import time
import threading
import uuid

ROOT = Path(__file__).resolve().parents[1]
TOKEN = "isolated-amux-gateway-compatibility-token"
MISSING = "00000000-0000-4000-8000-000000000099"
ORIGINS = [
    {"session_id": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "instance_id": "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", "provider": "codex", "os": "linux"},
    {"session_id": "cccccccc-cccc-4ccc-8ccc-cccccccccccc", "instance_id": "dddddddd-dddd-4ddd-8ddd-dddddddddddd", "provider": "claude", "os": "windows"},
]
GENERATIONS = ["11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"]
FIXED_UUIDS = {MISSING, *GENERATIONS, "33333333-3333-4333-8333-333333333333", "44444444-4444-4444-8444-444444444444", *(str(uuid.UUID(int=100 + i)) for i in range(4)), *(o[k] for o in ORIGINS for k in ("session_id", "instance_id"))}
UUID_RE = re.compile(r"\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b")
HEADERS = ("content-type", "content-disposition", "allow", "www-authenticate")


def file_sha256(path):
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def require(condition, detail):
    if not condition:
        raise AssertionError(detail)


def origin_headers(index):
    o = ORIGINS[index]
    return {"X-Agent-Id": "compat-agent-" + str(index), "X-Agent-Session-Id": o["session_id"], "X-Agent-Instance-Id": o["instance_id"], "X-Agent-Provider": o["provider"], "X-Agent-OS": o["os"]}


def synthetic_environment(profile, port, ui_auth="on"):
    # An allow-list prevents inherited plugin tokens, gateway URLs, provider
    # credentials, database URLs, proxies or project settings from leaking in.
    keep = ("PATH", "PATHEXT", "SystemRoot", "SYSTEMROOT", "WINDIR", "COMSPEC", "LD_LIBRARY_PATH", "DYLD_LIBRARY_PATH")
    env = {k: os.environ[k] for k in keep if k in os.environ}
    for name, relative in {"HOME": "home", "USERPROFILE": "home", "XDG_CONFIG_HOME": "config", "XDG_DATA_HOME": "data", "XDG_STATE_HOME": "state", "XDG_CACHE_HOME": "cache", "TEMP": "tmp", "TMP": "tmp", "TMPDIR": "tmp"}.items():
        path = profile / relative
        path.mkdir(exist_ok=True)
        env[name] = str(path)
    env.update(GATEWAY_API_KEY=TOKEN, GATEWAY_HOST="127.0.0.1", GATEWAY_PORT=str(port), DATABASE_BACKEND="sqlite", DATABASE_PATH=str(profile / "gateway.db"), GATEWAY_UI_AUTH=ui_auth, DEFAULT_CHANNEL="discord", RUST_LOG="warn")
    # Original gateway polls its updater. Keep that request on a closed loopback
    # proxy; fixture requests themselves use direct HTTPConnection sockets.
    env.update(HTTP_PROXY="http://127.0.0.1:9", HTTPS_PROXY="http://127.0.0.1:9", ALL_PROXY="http://127.0.0.1:9", NO_PROXY="127.0.0.1,localhost")
    (profile / ".env").write_text("# Isolated compatibility fixture; do not search ancestor .env files.\n")
    return env


class Server:
    def __init__(self, executable, candidate, profile, ui_auth="on", startup_timeout=60, extra_env=None):
        self.executable, self.candidate, self.profile = executable, candidate, profile
        profile.mkdir(parents=True, exist_ok=True)
        with socket.socket() as reservation:
            reservation.bind(("127.0.0.1", 0))
            self.port = reservation.getsockname()[1]
        self.env = synthetic_environment(profile, self.port, ui_auth)
        self.env.update(extra_env or {})
        self.startup_timeout = startup_timeout
        self.process = self.log = None
        self.log_path = profile / ("candidate.log" if candidate else "reference.log")

    @property
    def database(self):
        return self.profile / "gateway.db"

    def __enter__(self):
        self.log = self.log_path.open("ab")
        command = [str(self.executable)] + (["server"] if self.candidate else [])
        try:
            self.process = subprocess.Popen(command, cwd=self.profile, env=self.env, stdin=subprocess.DEVNULL, stdout=self.log, stderr=self.log)
            deadline = time.monotonic() + self.startup_timeout
            while time.monotonic() < deadline:
                require(self.process.poll() is None, f"server exited ({self.process.returncode}); log: {self.log_path}")
                try:
                    if self.request("GET", "/v1/tasks/capabilities")[0] == 200:
                        return self
                except (OSError, http.client.HTTPException):
                    pass
                time.sleep(0.05)
            raise AssertionError(f"server did not start; log: {self.log_path}")
        except BaseException:
            self.__exit__(None, None, None)
            raise

    def __exit__(self, *_):
        if self.process is not None and self.process.poll() is None:
            self.process.terminate()
            try:
                self.process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait(timeout=10)
        if self.log:
            self.log.close()

    def request(self, method, path, body=None, headers=None, token=TOKEN):
        request_headers = {"Content-Type": "application/json"}
        if token is not None:
            request_headers["Authorization"] = "Bearer " + token
        request_headers.update(headers or {})
        data = json.dumps(body, separators=(",", ":")).encode() if body is not None and not isinstance(body, bytes) else body
        connection = http.client.HTTPConnection("127.0.0.1", self.port, timeout=15)
        try:
            connection.request(method, path, body=data, headers=request_headers)
            response = connection.getresponse()
            return response.status, {k.lower(): v for k, v in response.getheaders()}, response.read()
        finally:
            connection.close()


class Normalize:
    """Normalize only declared nondeterminism, keeping relationships per trace."""
    def __init__(self):
        self.ids = {}
        self.derived_keys = {}

    def identifier(self, value):
        if value.lower() in FIXED_UUIDS:
            return value.lower()
        return self.ids.setdefault(value, f"<uuid-{len(self.ids) + 1}>")

    def value(self, value, key=""):
        if isinstance(value, dict):
            if value.get("link_type") == "task_generated_from_spec":
                address = value.get("source_child_address", "")
                match = re.fullmatch(r"manifest\.items\[(.+)\]", address)
                version = value.get("source_version_id")
                require(match and version, "spec task link lost its version/item identity")
                expected = "spec-task-" + hashlib.sha256((version + ":" + match[1]).encode()).hexdigest()
                require(value.get("idempotency_key") == expected, "spec task idempotency hash changed")
                self.derived_keys[expected] = "<spec-task:" + self.identifier(version) + ":" + match[1] + ">"
            return {k: self.value(v, k) for k, v in sorted(value.items())}
        if isinstance(value, list):
            return [self.value(v, key) for v in value]
        if isinstance(value, str):
            if value in self.derived_keys:
                return self.derived_keys[value]
            if key == "password_hash" and value.startswith("$argon2"):
                return "<salted-argon2-password-hash>"
            if key.endswith("_json"):
                try:
                    return self.value(json.loads(value))
                except (ValueError, TypeError):
                    pass
            return UUID_RE.sub(lambda match: self.identifier(match.group()), value)
        if isinstance(value, int) and not isinstance(value, bool) and 1_500_000_000_000 < value < 4_000_000_000_000:
            return "<epoch-ms>"
        if isinstance(value, bytes):
            return {"base64": base64.b64encode(value).decode()}
        return value

    def response(self, response):
        status, headers, body = response
        selected = {k: headers.get(k) for k in HEADERS}
        if selected["allow"]:
            selected["allow"] = sorted(part.strip() for part in selected["allow"].split(","))
        try:
            payload = json.loads(body)
        except (ValueError, UnicodeDecodeError):
            try:
                payload = body.decode()
            except UnicodeDecodeError:
                payload = {"base64": base64.b64encode(body).decode()}
        return {"status": status, "headers": selected, "body": self.value(payload)}


def materialize(path):
    return re.sub(r"\{([^}]+)\}", lambda m: "compat-missing-project" if m[1] == "ident" else "compat-missing-skill" if m[1] == "name" else MISSING, path)


def matrix(server, operations, plugin_operations):
    result = []
    normalize = Normalize()
    for op in operations:
        method, path = op["method"], materialize(op["path"])
        for label, token in (("missing-bearer", None), ("wrong-bearer", "invalid-fixture-token")):
            response = server.request(method, path, body=b"{", token=token)
            require(response[0] == 401, f"{label} {method} {path}: expected401, got{response[0]}")
            result.append({"probe": label, "method": method, "route": op["path"], "response": normalize.response(response)})
        headers = {}
        body = None if method in ("GET", "DELETE") else b"{"
        if op["handler"] == "routes::upload_skill":
            headers = {"Content-Type": "application/octet-stream", "X-Kind": "invalid-compat-kind"}
        elif "multipart" in op["path"]:
            headers = {"Content-Type": "multipart/form-data"}
            body = b"missing-boundary"
        response = server.request(method, path, body=body, headers=headers)
        result.append({"probe": "authorized-malformed-or-missing-resource", "method": method, "route": op["path"], "response": normalize.response(response)})
    for path in sorted({op["path"] for op in operations}):
        response = server.request("TRACE", materialize(path))
        result.append({"probe": "method-fallback", "method": "TRACE", "route": path, "response": normalize.response(response)})
    for op in operations:
        if op["method"] == "GET":
            result.append({"probe": "implicit-head", "method": "HEAD", "route": op["path"], "response": normalize.response(server.request("HEAD", materialize(op["path"])))})
    for op in plugin_operations:
        response = server.request(op["method"], materialize(op["path"]), token=None)
        result.append({"probe": "unconfigured-plugin-transport", "method": op["method"], "route": op["path"], "response": normalize.response(response)})
    spoof = {"X-Gateway-UI": "1", "X-Gateway-User": "1:forged:Forged", "X-Gateway-User-Role": "admin"}
    for label, path, token, headers in [
        ("ui-header-cannot-bypass-auth", "/v1/projects", None, spoof),
        ("bearer-strips-spoofed-user", "/v1/users/me", TOKEN, spoof),
        ("unknown-route-no-bearer", "/v1/compat-does-not-exist", None, {}),
        ("unknown-route-authorized", "/v1/compat-does-not-exist", TOKEN, {}),
    ]:
        result.append({"probe": label, "method": "GET", "route": path, "response": normalize.response(server.request("GET", path, token=token, headers=headers))})
    return result


class SocketPeer:
    def __init__(self, server):
        self.http = http.client.HTTPConnection("127.0.0.1", server.port, timeout=10)
        key = base64.b64encode(os.urandom(16)).decode()
        self.http.request("GET", "/v1/tasks/stream", headers={"Authorization": "Bearer " + TOKEN, "Connection": "Upgrade", "Upgrade": "websocket", "Sec-WebSocket-Version": "13", "Sec-WebSocket-Key": key})
        response = self.http.getresponse()
        require(response.status == 101, f"WebSocket upgrade returned{response.status}")
        expected = base64.b64encode(hashlib.sha1((key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode()).digest()).decode()
        require(response.getheader("Sec-WebSocket-Accept") == expected, "invalid WebSocket handshake")
        self.socket = self.http.sock

    def send_frame(self, payload, opcode=1):
        mask = os.urandom(4)
        size = len(payload)
        prefix = bytes([0x80 | opcode, 0x80 | size]) if size < 126 else bytes([0x80 | opcode, 0xFE]) + struct.pack("!H", size)
        self.socket.sendall(prefix + mask + bytes(byte ^ mask[i % 4] for i, byte in enumerate(payload)))

    def send(self, value):
        self.send_frame(json.dumps(value).encode())

    def read(self, size):
        result = b""
        while len(result) < size:
            chunk = self.socket.recv(size - len(result))
            require(chunk, "WebSocket closed early")
            result += chunk
        return result

    def receive(self, expected):
        while True:
            opcode, size = self.read(2)
            length = size & 0x7F
            if length == 126:
                length = struct.unpack("!H", self.read(2))[0]
            elif length == 127:
                length = struct.unpack("!Q", self.read(8))[0]
            require(not size & 0x80 and length < 4 * 1024 * 1024, "invalid/oversized server frame")
            payload = self.read(length)
            opcode &= 0x0F
            if opcode == 9:
                self.send_frame(payload, 10)
                continue
            require(opcode == 1, f"unexpected WebSocket opcode{opcode}")
            value = json.loads(payload)
            if value["type"] == expected:
                return value
            if value["type"] == "heartbeat":
                self.send({"type": "heartbeat"})
            else:
                require(value["type"] == "heartbeat_ack", f"expected {expected}, got{value}")

    def close(self):
        with contextlib.suppress(OSError):
            self.send_frame(b"", 8)
        self.socket.close()
        self.http.close()


class Flow:
    def __init__(self, server):
        self.server, self.normal = server, Normalize()
        self.trace, self.success = [], set()

    def hit(self, name, method, path, body=None, headers=None, expected=(200,), token=TOKEN):
        response = self.server.request(method, path, body=body, headers=headers, token=token)
        require(response[0] in expected, f"{name}: {method} {path} returned{response[0]} {response[2][:400]!r}")
        self.trace.append({"case": name, "method": method, "path": self.normal.value(path), "response": self.normal.response(response)})
        if 200 <= response[0] < 300:
            self.success.add((method, path.split("?", 1)[0]))
        try:
            return json.loads(response[2])
        except (ValueError, UnicodeDecodeError):
            return response[2]

    def run(self):
        h = self.hit
        for project in ("alpha", "beta"):
            h("register adhoc " + project, "POST", "/v1/projects", {"ident": project})
            h("backfill repo " + project, "POST", "/v1/projects", {"ident": project, "repo_url": f"https://github.com/compat-fixture/{project}.git"})
        h("project directory", "GET", "/v1/projects")
        h("plugin-required fresh repo", "POST", "/v1/projects", {"ident": "requires-plugin", "repo_url": "https://github.com/compat-fixture/new.git"}, expected=(400,))
        link = h("project link", "POST", "/v1/projects/alpha/links", {"label": "fixture", "url": "https://example.invalid/fixture"})
        h("project links", "GET", "/v1/projects/alpha/links")
        h("delete project link", "DELETE", f"/v1/projects/alpha/links/{link['id']}")
        presence = h("ordinary target heartbeat", "POST", "/v1/projects/alpha/task-targets", {"generation": GENERATIONS[0], "canonical_remote": "github.com/compat-fixture/alpha", "display_name": "Fixture A", "hostname": "FixtureHost"}, headers=origin_headers(0))
        require(presence["expires_at"] - presence["seen_at"] == 120000, "ordinary target TTL changed")
        h("target directory", "GET", "/v1/projects/alpha/task-targets")
        task = h("task create", "POST", "/v1/projects/alpha/tasks", {"title": "Durable task", "specification": "Preserve this context", "labels": ["fixture"]})
        task_path = "/v1/projects/alpha/tasks/" + task["id"]
        h("requester subscription", "PUT", task_path + "/requester", {"requester": presence["target"]})
        h("task claim", "PATCH", task_path, {"status": "in_progress"}, headers=origin_headers(0))
        before = database_snapshot(self.server.database)
        h("wrong owner refused", "PATCH", task_path, {"status": "done"}, headers=origin_headers(1), expected=(409,))
        require(before == database_snapshot(self.server.database), "rejected ownership mutation changed durable data")
        h("task comment", "POST", task_path + "/comments", {"content": "Comment retained across hosts"}, headers=origin_headers(0))
        h("task detail", "GET", task_path)
        h("task list", "GET", "/v1/projects/alpha/tasks")
        h("child task", "POST", task_path + "/subtasks", {"title": "Child fixture"}, headers=origin_headers(0))
        h("child task list", "GET", task_path + "/subtasks")
        h("task rank", "PATCH", task_path, {"rank": -4}, headers=origin_headers(0))
        delegated = h("cross-project delegate", "POST", "/v1/projects/alpha/tasks/delegate", {"target_project_ident": "beta", "title": "Delegated fixture", "specification": "Cross-project context"}, headers=origin_headers(0))
        h("delegated target detail", "GET", "/v1/projects/beta/tasks/" + delegated["target_task"]["id"])
        h("update delegated source and mirror", "PATCH", "/v1/projects/alpha/tasks/" + delegated["source_task"]["id"] + "/delegation", {"title": "Updated delegation", "specification": "Updated mirrored context"}, headers=origin_headers(0))
        h("requester clear", "PUT", task_path + "/requester", {"requester": None}, headers=origin_headers(0))
        h("task release", "PATCH", task_path, {"status": "todo"}, headers=origin_headers(0))
        h("task reclaim", "PATCH", task_path, {"status": "in_progress"}, headers=origin_headers(0))
        h("requester capability", "GET", "/v1/tasks/capabilities")
        # No channel plugin is configured. Human messages persist independently;
        # channel-only send/room/reply/action errors must remain explicit.
        message = h("human message persists", "POST", "/v1/projects/alpha/messages/human", {"content": "Durable human message", "author": "Fixture human"})
        message_path = f"/v1/projects/alpha/messages/{message['message_id']}"
        h("message threads", "GET", "/v1/projects/alpha/messages")
        h("message thread", "GET", message_path + "/thread")
        h("unread messages", "GET", "/v1/projects/alpha/messages/unread", headers={"X-Agent-Id": "fixture-reader"})
        h("confirm message", "POST", message_path + "/confirm", {}, headers={"X-Agent-Id": "fixture-reader"})
        h("resolve message", "POST", message_path + "/resolve", {})
        h("reopen message", "POST", message_path + "/reopen", {})
        h("bulk resolve messages", "POST", "/v1/projects/alpha/messages/resolve", {})
        h("plugin-required reply", "POST", message_path + "/reply", {"content": "No external reply"}, expected=(503,))
        h("plugin-required action", "POST", message_path + "/action", {"action": "working"}, expected=(503,))
        h("plugin-required send", "POST", "/v1/projects/alpha/messages", {"content": "No external send"}, expected=(503,))
        h("plugin-required room", "POST", "/v1/projects/alpha/room", {}, expected=(503,))
        memory = h("legacy memory push", "POST", "/v1/projects/alpha/memories/push", {"memories": [{"content": "Fixture durable memory", "memory_type": "project", "tags": ["fixture"]}]})
        require(memory["results"][0]["action"] == "created", "memory was not created")
        h("memory capability", "GET", "/v1/memories/capabilities")
        h("memory list", "GET", "/v1/projects/alpha/memories")
        h("memory pull", "POST", "/v1/projects/alpha/memories/pull", {})
        h("memory projects", "GET", "/v1/memories/projects")
        pattern = h("pattern create", "POST", "/v1/patterns", {"title": "Fixture pattern", "body": "Use fixture boundaries", "version": "latest", "state": "active", "labels": ["fixture"]})
        pattern_path = "/v1/patterns/" + pattern["id"]
        h("patterns list", "GET", "/v1/patterns")
        h("pattern read", "GET", pattern_path)
        h("pattern comment", "POST", pattern_path + "/comments", {"content": "Fixture comment"})
        h("pattern comments", "GET", pattern_path + "/comments")
        h("pattern update", "PATCH", pattern_path, {"summary": "Updated fixture summary"})
        h("skill JSON create", "POST", "/v1/skills/fixture-command", {"kind": "command", "content": "# Fixture command\nRead local fixture data.\n"})
        h("skills list", "GET", "/v1/skills")
        h("skill content", "GET", "/v1/skills/fixture-command/content")
        h("skill download", "GET", "/v1/skills/fixture-command")
        h("skill raw upload", "PUT", "/v1/skills/fixture-command", b"# Updated fixture\n", headers={"X-Kind": "command", "Content-Type": "text/markdown"})
        self.extra_lifecycle(task_path, pattern_path)
        self.users()
        self.artifacts()
        self.specs_and_reviews()
        self.dispatch()
        self.websocket()
        h("events", "GET", "/v1/task-events")
        h("execution runs", "GET", "/v1/execution/runs")
        h("stream consumers", "GET", "/v1/task-events/consumers")
        return {"trace": self.trace, "successful_operations": sorted(self.success), "sentinel_task_id": task["id"]}

    def extra_lifecycle(self, task_path, pattern_path):
        h = self.hit
        h("repo mapping", "PATCH", "/v1/projects/alpha/repo", {"provider": "github", "namespace": "compat-fixture", "repo_name": "alpha"})
        other = h("second same-project target", "POST", "/v1/projects/alpha/task-targets", {"generation": GENERATIONS[1], "canonical_remote": "github.com/compat-fixture/alpha"}, headers=origin_headers(1))
        mine = {"origin": ORIGINS[0], "generation": GENERATIONS[0]}
        h("human assignment", "POST", task_path + "/assign", {"target": other["target"]})
        h("owner reassignment", "POST", task_path + "/reassign", {"target": mine}, headers=origin_headers(1))
        h("claim reassigned task", "PATCH", task_path, {"status": "in_progress"}, headers=origin_headers(0))
        h("park for user review", "PATCH", task_path, {"status": "user_review"}, headers=origin_headers(0))
        h("human resumes parked task", "PATCH", task_path, {"status": "todo"})
        h("claim resumed task", "PATCH", task_path, {"status": "in_progress"}, headers=origin_headers(0))
        disposable = h("disposable task", "POST", "/v1/projects/alpha/tasks", {"title": "Disposable fixture"})
        h("reorder task", "POST", "/v1/projects/alpha/tasks/reorder?status=todo", {"order": [disposable["id"]]})
        h("delete task", "DELETE", "/v1/projects/alpha/tasks/" + disposable["id"])
        cancelled = h("cancellable task", "POST", "/v1/projects/alpha/tasks", {"title": "Cancellation fixture"})
        path = "/v1/projects/alpha/tasks/" + cancelled["id"]
        report = h("cancel task", "POST", path + "/cancellation", {"reason": "Fixture cancellation"}, headers=origin_headers(0))
        require(report["task"]["status"] == "cancelled" and not report["cancellation"]["executor_stop_confirmed"], "cancellation falsely confirms execution stop")
        h("read cancellation report", "GET", path + "/cancellation")
        h("cancelled task is terminal", "PATCH", path, {"status": "done"}, headers=origin_headers(0), expected=(409,))
        h("archive project", "POST", "/v1/projects/beta/archive", {})
        h("restore project", "POST", "/v1/projects/beta/restore", {})
        h("memory push alias", "POST", "/v1/projects/alpha/memories", {"memories": [{"content": "Alias durable memory", "memory_type": "project"}]})
        h("delete pattern", "DELETE", pattern_path)
        h("named skill root", "POST", "/v1/skills", {"name": "fixture-root-skill", "kind": "command", "content": "# Root skill fixture"})
        boundary = "compat-fixture-boundary"
        parts = [("kind", "command"), ("content", "# Multipart fixture")]
        payload = "".join(f"--{boundary}\r\nContent-Disposition: form-data; name=\"{name}\"\r\n\r\n{value}\r\n" for name, value in parts) + f"--{boundary}--\r\n"
        h("multipart skill", "POST", "/v1/skills/fixture-multipart/multipart", payload.encode(), headers={"Content-Type": "multipart/form-data; boundary=" + boundary})
        h("delete skill", "DELETE", "/v1/skills/fixture-root-skill", expected=(204,))

    def users(self):
        h = self.hit
        admin = h("create admin user", "POST", "/v1/users", {"username": "compat-admin", "password": "fixture-password-123", "role": "admin"})
        member = h("create member user", "POST", "/v1/users", {"username": "compat-member", "password": "fixture-password-123", "role": "member"})
        h("user list", "GET", "/v1/users")
        h("update user", "PATCH", "/v1/users/" + str(member["id"]), {"display_name": "Compatibility Member"})
        with contextlib.closing(sqlite3.connect(self.server.database)) as db:
            user_id, epoch = db.execute("SELECT id,session_epoch FROM users WHERE username='compat-member'").fetchone()
        expiry = int(time.time() * 1000) + 300000
        signed = f"gateway-ui-session:{user_id}:{expiry}:{epoch}"
        signature = hmac.new(TOKEN.encode(), signed.encode(), hashlib.sha256).hexdigest()
        headers = {"X-Gateway-UI": "1", "Cookie": f"gw_session={user_id}.{expiry}.{signature}"}
        h("session current user", "GET", "/v1/users/me", headers=headers, token=None)
        h("session cannot administer", "GET", "/v1/users", headers=headers, token=None, expected=(403,))
        h("change own password", "POST", "/v1/users/me/password", {"current_password": "fixture-password-123", "new_password": "fixture-new-password-456"}, headers=headers, token=None)
        h("delete member", "DELETE", "/v1/users/" + str(member["id"]))
        h("last administrator protected", "DELETE", "/v1/users/" + str(admin["id"]), expected=(400,))

    def artifacts(self):
        h = self.hit
        def headers(key):
            return {"X-Agent-Id": "fixture-agent", "X-Agent-System": "codex", "Idempotency-Key": key}
        artifact = h("artifact create", "POST", "/v1/projects/alpha/artifacts", {"kind": "spec", "title": "Fixture artifact", "labels": ["fixture"]}, headers=headers("artifact-create"), expected=(201,))
        aid = artifact["data"]["artifact_id"]
        path = "/v1/projects/alpha/artifacts/" + aid
        h("artifact create replay", "POST", "/v1/projects/alpha/artifacts", {"kind": "spec", "title": "Fixture artifact", "labels": ["fixture"]}, headers=headers("artifact-create"), expected=(200, 201))
        h("artifacts list", "GET", "/v1/projects/alpha/artifacts")
        h("artifact detail", "GET", path)
        version = h("artifact version", "POST", path + "/versions", {"body": "Fixture artifact body", "body_format": "markdown", "version_state": "draft"}, headers=headers("artifact-version"), expected=(201,))
        vid = version["data"]["artifact_version_id"]
        h("artifact versions", "GET", path + "/versions")
        h("artifact version detail", "GET", path + "/versions/" + vid)
        h("artifact comments", "GET", path + "/comments")
        h("artifact links", "GET", "/v1/projects/alpha/artifacts/links")
        h("artifact workflow runs", "GET", path + "/workflow-runs")
        second = h("second artifact version", "POST", path + "/versions", {"body": "Fixture artifact body\nMore", "parent_version_id": vid}, headers=headers("artifact-version-2"), expected=(201,))
        second_id = second["data"]["artifact_version_id"]
        h("artifact version diff", "GET", path + "/versions/" + second_id + "/diff?base_version_id=" + vid)
        comment = h("artifact comment", "POST", path + "/comments", {"target_kind": "artifact", "target_id": aid, "body": "Fixture comment"}, headers=headers("artifact-comment"), expected=(201,))
        comment_path = path + "/comments/" + comment["data"]["comment_id"]
        h("resolve artifact comment", "POST", comment_path + "/resolve", {"resolution_note": "Fixture resolved"}, headers=headers("artifact-comment-resolve"))
        h("reopen artifact comment", "POST", comment_path + "/reopen", {"note_body": "Fixture reopened"}, headers=headers("artifact-comment-reopen"))
        contribution = h("artifact contribution", "POST", path + "/contributions", {"target_kind": "artifact", "target_id": aid, "contribution_kind": "note", "role": "author", "body": "Fixture contribution"}, headers=headers("artifact-contribution"), expected=(201,))
        h("artifact contribution list", "GET", path + "/contributions")
        link = h("artifact link", "POST", "/v1/projects/alpha/artifacts/links", {"link_type": "doc_referenced_by_spec", "source_kind": "artifact", "source_id": aid, "target_kind": "artifact_version", "target_id": vid, "target_version_id": vid}, headers=headers("artifact-link"), expected=(201,))
        run = h("artifact workflow start", "POST", path + "/workflow-runs", {"workflow_kind": "spec_iteration", "phase": "fixture", "is_resumable": False}, headers=headers("artifact-run"), expected=(201,))
        run_path = path + "/workflow-runs/" + run["data"]["workflow_run_id"]
        h("artifact workflow get", "GET", run_path)
        h("artifact workflow complete", "PATCH", run_path, {"state": "succeeded", "generated_version_ids": [second_id], "generated_contribution_ids": [contribution["data"]["contribution_id"]], "generated_link_ids": [link["data"]["link_id"]]}, headers=headers("artifact-run-complete"))

    def specs_and_reviews(self):
        h = self.hit
        def headers(key, origin=False):
            return {**(origin_headers(0) if origin else {}), "X-Agent-Id": "fixture-agent", "X-Agent-System": "codex", "Idempotency-Key": key}
        manifest = {"spec_version": 1, "phases": [{"id": "phase-1", "name": "Fixture phase", "tasks": [{"id": item, "team": "backend", "title": "Fixture " + item, "depends_on": [], "labels": ["fixture"], "touch_surface": ["fixture.rs"], "acceptance_criteria": ["durable result"], "validation_plan": ["fixture check"], "spec_file": item + ".md", "status": "todo"} for item in ("T001", "T002")]}]}
        files = {item + ".md": "# " + item + "\nFixture handoff" for item in ("T001", "T002")}
        spec = h("spec import", "POST", "/v1/projects/alpha/specs", {"title": "Fixture spec", "body": "# Fixture specification", "manifest": manifest, "file_bodies": files}, headers=headers("spec-create"), expected=(201,))
        sid = spec["data"]["artifact"]["artifact_id"]
        first = spec["data"]["version"]["artifact_version_id"]
        path = "/v1/projects/alpha/specs/" + sid
        version = h("spec new version", "POST", path + "/versions", {"parent_version_id": first, "body": "# Updated fixture specification", "manifest": manifest, "file_bodies": files}, headers=headers("spec-version"), expected=(201,))
        h("accept spec version", "POST", path + "/accept", {"version_id": version["data"]["artifact_version_id"]}, headers=headers("spec-accept"))
        h("spec manifest", "GET", path + "/manifest")
        h("spec manifest item", "GET", path + "/manifest/phase-1:T001")
        generated = h("generate spec task", "POST", path + "/generate-tasks", {"confirmed": True, "manifest_item_ids": ["phase-1:T001"], "reporter": "fixture-generator"}, headers=headers("spec-generate", True), expected=(200, 201))
        require(generated["data"], "empty generated spec result")
        existing = h("existing task for spec link", "POST", "/v1/projects/alpha/tasks", {"title": "Existing spec task"})
        h("link existing spec task", "POST", path + "/link-task", {"manifest_item_id": "phase-1:T002", "task_id": existing["id"]}, headers=headers("spec-link"), expected=(201,))
        review = h("design review create", "POST", "/v1/projects/alpha/design-reviews", {"title": "Fixture design", "body": "# Fixture design body", "body_format": "markdown"}, headers=headers("review-create"), expected=(201,))
        rid = review["data"]["artifact"]["artifact_id"]
        rvid = review["data"]["version"]["artifact_version_id"]
        path = "/v1/projects/alpha/design-reviews/" + rid
        round_result = h("design review round", "POST", path + "/rounds", {"round_id": "compat-round", "source_artifact_version_id": rvid}, headers=headers("review-round"), expected=(201,))
        run_id = round_result["data"]["workflow_run"]["workflow_run_id"]
        round_path = path + "/rounds/" + run_id
        first = h("design first pass", "POST", round_path + "/contributions", {"phase": "pass_1", "body": "Fixture first review"}, headers=headers("review-pass-1"), expected=(201,))
        first_id = first["data"]["contribution_id"]
        second = h("design second pass", "POST", round_path + "/contributions", {"phase": "pass_2", "read_set": {"versions": [rvid], "contributions": [first_id]}, "body": "Fixture second review"}, headers=headers("review-pass-2"), expected=(201,))
        h("design contribution query", "GET", path + "/contributions?round_id=compat-round&phase=pass_2&role=reviewer&read_set_contains=" + first_id)
        h("design synthesis", "POST", round_path + "/synthesis", {"read_set": {"contributions": [second["data"]["contribution_id"]]}, "body": "# Fixture synthesis", "create_version": True, "version_label": "synthesis"}, headers=headers("review-synthesis"), expected=(201,))
        h("design state decision", "POST", path + "/state", {"review_state": "accepted", "note": "Fixture accepted"}, headers=headers("review-state"), expected=(201,))

    def dispatch(self):
        h = self.hit
        keys = ["a1" * 32, "b2" * 32]
        session_keys = ["c3" * 32, "d4" * 32]
        for index, project in enumerate(("alpha", "beta")):
            with contextlib.closing(sqlite3.connect(self.server.database)) as db:
                db.execute("INSERT INTO task_machines(project_ident,instance_id,os,key_hash) VALUES(?,?,?,?)", (project, ORIGINS[index]["instance_id"], ORIGINS[index]["os"], hashlib.sha256(keys[index].encode()).hexdigest()))
                db.commit()
        def headers(index):
            return {**origin_headers(index), "X-Gateway-Machine-Key": keys[index], "X-Gateway-Session-Key": session_keys[index]}
        registrations = [h("dispatch session " + project, "POST", f"/v1/projects/{project}/task-sessions", {"generation": GENERATIONS[index]}, headers=headers(index)) for index, project in enumerate(("alpha", "beta"))]
        for registration in registrations:
            require(registration["expires_at"] - registration["seen_at"] == 120000, "dispatch session TTL changed")
        h("dispatch sessions directory", "GET", "/v1/projects/alpha/task-sessions", headers=headers(0))
        body = {"request_id": "33333333-3333-4333-8333-333333333333", "generation": GENERATIONS[0], "target_project_ident": "beta", "target": registrations[1]["target"], "title": "Fixture dispatch", "specification": "Preserve session fenced dispatch"}
        result = h("credential-bound dispatch", "POST", "/v1/projects/alpha/task-dispatches", body, headers=headers(0))
        h("dispatch request replay", "POST", "/v1/projects/alpha/task-dispatches", body, headers=headers(0))
        dispatch = result["dispatch"]
        path = "/v1/projects/beta/task-dispatches/" + dispatch["task_id"] + "/actions"
        claim = None
        for number, operation in enumerate(("receive", "claim", "start", "succeeded")):
            action = {"request_id": str(uuid.UUID(int=100 + number)), "generation": GENERATIONS[1], "operation": operation, "claim_id": claim}
            if operation in ("start", "succeeded"):
                action["execution_id"] = "44444444-4444-4444-8444-444444444444"
            if operation == "succeeded":
                action["result"] = "Fixture completed successfully"
            result = h("dispatch " + operation, "POST", path, action, headers=headers(1))
            dispatch = result["dispatch"]
            claim = dispatch.get("claim_id")
        require(dispatch["state"] == "succeeded", "dispatch did not finish")
        h("dispatch recovery", "GET", "/v1/projects/beta/task-dispatches/recovery?generation=" + GENERATIONS[1], headers=headers(1))

    def websocket(self):
        latest = self.hit("pre-stream event cursor", "GET", "/v1/task-events")
        after = latest[0]["id"] if latest else 0
        peer = SocketPeer(self.server)
        try:
            peer.send({"type": "subscribe", "protocol_version": 1, "consumer_id": "compat-fixture", "after_event_id": after})
            self.trace.append({"case": "WebSocket subscribed", "frame": self.normal.value(peer.receive("subscribed"))})
            self.success.add(("GET", "/v1/tasks/stream"))
            self.hit("streamed task", "POST", "/v1/projects/alpha/tasks", {"title": "WebSocket fixture"})
            event = peer.receive("event")
            require(event["event"]["kind"] == "task_created", "unexpected stream event kind")
            self.trace.append({"case": "WebSocket event", "frame": self.normal.value(event)})
            peer.send({"type": "ack", "event_id": event["event"]["id"], "status": "received"})
            self.trace.append({"case": "WebSocket durable receipt", "frame": self.normal.value(peer.receive("recorded"))})
        finally:
            peer.close()
        deadline = time.monotonic() + 10
        while time.monotonic() < deadline:
            rows = json.loads(self.server.request("GET", "/v1/task-events/consumers")[2])
            if any(r["consumer_id"] == "compat-fixture" and not r["connected"] for r in rows):
                return
            time.sleep(0.02)
        raise AssertionError("stream consumer stayed connected after close")


def database_snapshot(path):
    with contextlib.closing(sqlite3.connect(path)) as db:
        require(db.execute("PRAGMA integrity_check").fetchone()[0] == "ok", "SQLite integrity check failed")
        require(not db.execute("PRAGMA foreign_key_check").fetchall(), "SQLite foreign keys failed")
        schema = db.execute("SELECT type,name,tbl_name,sql FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY type,name").fetchall()
        tables = {}
        for (name,) in db.execute("SELECT name FROM sqlite_master WHERE type='table' ORDER BY name"):
            escaped = '"' + name.replace('"', '""') + '"'
            cursor = db.execute("SELECT * FROM " + escaped)
            columns = [column[0] for column in cursor.description]
            rows = [{column: value for column, value in zip(columns, row)} for row in cursor.fetchall()]
            def order(row):
                return repr(sorted(row.items()))
            tables[name] = sorted(rows, key=order)
        return {"schema": schema, "tables": tables}


def compare(name, reference, candidate):
    if reference == candidate:
        return
    if isinstance(reference, list) and isinstance(candidate, list):
        for index, (left, right) in enumerate(zip(reference, candidate)):
            if left != right:
                raise AssertionError(f"{name} differs at item{index}:\nreference={json.dumps(left, default=str)[:2500]}\ncandidate={json.dumps(right, default=str)[:2500]}")
    raise AssertionError(f"{name} differs: {str(reference)[:1200]} != {str(candidate)[:1200]}")


def operation_key(operation):
    return operation["method"] + " " + operation["path"]


def matching_operation(method, path, operations):
    matches = []
    for operation in operations:
        if method != operation["method"]:
            continue
        pattern = re.sub(r"\{[^}]+\}", "[^/]+", operation["path"])
        if re.fullmatch(pattern, path):
            matches.append(operation)
    # Prefer literal routes (for example /tasks/delegate over /tasks/{id}).
    return min(matches, key=lambda operation: operation["path"].count("{")) if matches else None


def switching(reference, candidate, profile, task_id, timeout):
    stages = []
    for index, use_candidate in enumerate((False, True, False, True)):
        before = database_snapshot(profile / "gateway.db")
        with Server(candidate if use_candidate else reference, use_candidate, profile, startup_timeout=timeout) as server:
            compare("persisted data/schema before and after startup", before, database_snapshot(server.database))
            path = "/v1/projects/alpha/tasks/" + task_id
            task = json.loads(server.request("GET", path)[2])
            require(task["owner_origin"] == ORIGINS[0] and task["specification"] == "Preserve this context", "switch lost owner/context")
            protected = database_snapshot(server.database)
            response = server.request("PATCH", path, {"status": "done"}, origin_headers(1))
            require(response[0] == 409, "switch permitted a foreign claim completion")
            compare("failed switch write atomicity", protected, database_snapshot(server.database))
            note = f"switch-stage-{index}"
            response = server.request("POST", path + "/comments", {"content": note}, origin_headers(0))
            require(response[0] == 200, "switch could not append owner comment")
            detail = json.loads(server.request("GET", path)[2])
            require(detail["comments"][-1]["content"] == note, "switch comment not durable")
            after = database_snapshot(server.database)
            old_events = before["tables"].get("task_events", [])
            new_events = after["tables"].get("task_events", [])
            require(all(row in new_events for row in old_events) and len(new_events) > len(old_events), "switch lost or failed to append task events")
            stages.append({"backend": "candidate" if use_candidate else "reference", "stage": index, "schema_entries": len(after["schema"]), "tables": len(after["tables"]), "events_before": len(old_events), "events_after": len(new_events), "owner_preserved": True, "foreign_write_refused_without_changes": True})
    return stages


class GraphProvider:
    """Loopback provider exercising unmodified production WhatsApp HTTP code."""
    def __enter__(self):
        self.requests = []
        owner = self
        class Handler(http.server.BaseHTTPRequestHandler):
            def do_POST(self):
                body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                owner.requests.append({"path": self.path, "authorization": self.headers.get("Authorization"), "body": body})
                data = json.dumps({"messages": [{"id": "fixture-external-" + str(len(owner.requests))}]}).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)
            def log_message(self, *_):
                pass
        self.http = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.http.serve_forever, daemon=True)
        self.thread.start()
        return self

    def __exit__(self, *_):
        self.http.shutdown()
        self.http.server_close()
        self.thread.join(timeout=5)


def plugin_flow(binary, candidate, profile, startup_timeout):
    with GraphProvider() as provider:
        env = {"DEFAULT_CHANNEL": "whatsapp", "WHATSAPP_ACCESS_TOKEN": "fixture-whatsapp-token", "WHATSAPP_PHONE_NUMBER_ID": "fixture-phone", "WHATSAPP_WEBHOOK_VERIFY_TOKEN": "fixture-verify", "WHATSAPP_APP_SECRET": "fixture-secret", "WHATSAPP_GRAPH_BASE_URL": "http://127.0.0.1:" + str(provider.http.server_port), "WHATSAPP_GRAPH_API_VERSION": "v-fixture", "WHATSAPP_DEFAULT_RECIPIENT": "15550000001", "WHATSAPP_PROJECT_ROOMS": "plugin-alpha=15550000001,plugin-repo=15550000002"}
        with Server(binary, candidate, profile, startup_timeout=startup_timeout, extra_env=env) as server:
            flow = Flow(server)
            h = flow.hit
            h("plugin adhoc registration", "POST", "/v1/projects", {"ident": "plugin-alpha"})
            h("plugin eager repository room", "POST", "/v1/projects", {"ident": "plugin-repo", "repo_url": "https://github.com/compat-fixture/plugin-repo.git"})
            h("plugin explicit room", "POST", "/v1/projects/plugin-alpha/room", {})
            outbound = {"body": "Fixture body", "content": "Alias must lose", "subject": "Fixture subject", "hostname": "FixtureHost", "event_at": 1770000000000}
            sent = h("plugin structured send", "POST", "/v1/projects/plugin-alpha/messages", outbound, headers=origin_headers(0))
            path = "/v1/projects/plugin-alpha/messages/" + str(sent["message_id"])
            h("plugin structured reply", "POST", path + "/reply", outbound, headers=origin_headers(0))
            h("plugin structured action", "POST", path + "/action", {"message": "Fixture action", "hostname": "FixtureHost", "event_at": 1770000000000}, headers=origin_headers(0))
            human = h("plugin human delivery", "POST", "/v1/projects/plugin-alpha/messages/human", {"content": "Fixture human body", "author": "Fixture human"})
            require(human["delivered"] is True, "human message was not delivered")
            h("plugin human threaded delivery", "POST", "/v1/projects/plugin-alpha/messages/human", {"content": "Fixture human reply", "parent_message_id": sent["message_id"], "author": "Fixture human"})
            # Existing persisted human rows without an external id exercise the
            # production reply/action fallback to send_structured.
            with sqlite3.connect(server.database) as conn:
                conn.execute("UPDATE messages SET external_message_id=NULL WHERE id=?", (human["message_id"],))
            fallback = "/v1/projects/plugin-alpha/messages/" + str(human["message_id"])
            h("plugin reply fallback", "POST", fallback + "/reply", outbound, headers=origin_headers(0))
            h("plugin action fallback", "POST", fallback + "/action", outbound, headers=origin_headers(0))
            h("valid webhook challenge", "GET", "/webhooks/whatsapp?hub.mode=subscribe&hub.verify_token=fixture-verify&hub.challenge=fixture-challenge", token=None)
            h("invalid webhook challenge", "GET", "/webhooks/whatsapp?hub.mode=subscribe&hub.verify_token=wrong", expected=(403,), token=None)
            payload = {"entry": [{"changes": [{"field": "messages", "value": {"contacts": [{"wa_id": "15550000001", "profile": {"name": "Fixture sender"}}], "messages": [{"from": "15550000001", "id": "fixture-inbound-1", "timestamp": "1770000000", "type": "text", "text": {"body": "Fixture webhook inbound"}}]}}]}]}
            wire = json.dumps(payload, separators=(",", ":")).encode()
            signature = "sha256=" + hmac.new(b"fixture-secret", wire, hashlib.sha256).hexdigest()
            h("invalid webhook signature", "POST", "/webhooks/whatsapp", wire, headers={"X-Hub-Signature-256": "sha256=invalid"}, expected=(401,), token=None)
            h("valid webhook ingress", "POST", "/webhooks/whatsapp", wire, headers={"X-Hub-Signature-256": signature}, token=None)
            duplicate = h("webhook deduplication", "POST", "/webhooks/whatsapp", wire, headers={"X-Hub-Signature-256": signature}, token=None)
            require(duplicate["messages"] == 0, "duplicate webhook was not suppressed")
            deadline = time.monotonic() + 5
            while True:
                with sqlite3.connect(server.database) as conn:
                    rows = conn.execute("SELECT project_ident,source,content,external_message_id,author_kind FROM messages WHERE external_message_id='fixture-inbound-1'").fetchall()
                if rows or time.monotonic() >= deadline:
                    break
                time.sleep(0.02)
            require(len(rows) == 1 and rows[0][2] == "Fixture webhook inbound", "webhook input not durably persisted exactly once")
            require(len(provider.requests) == 7, "unexpected outbound Graph request count")
            for request in provider.requests:
                require(request["path"] == "/v-fixture/fixture-phone/messages", "wrong Graph endpoint")
                require(request["authorization"] == "Bearer fixture-whatsapp-token", "wrong Graph authorization")
            flow.trace.append({"case": "production Graph requests", "requests": provider.requests})
            flow.trace.append({"case": "durable webhook input", "rows": rows})
            return {"trace": flow.trace, "successful_operations": flow.success}


def middleware_off(executable, candidate, profile, timeout):
    normal = Normalize()
    with Server(executable, candidate, profile, "off", timeout) as server:
        return [normal.response(server.request("GET", path, token=token, headers=headers)) for path, token, headers in [
            ("/v1/projects", None, {}),
            ("/v1/projects", None, {"X-Gateway-UI": "1"}),
            ("/v1/projects", "wrong", {"X-Gateway-UI": "1"}),
            ("/v1/compat-does-not-exist", None, {"X-Gateway-UI": "1"}),
        ]]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--reference", required=True, type=Path, help="Original pinned gateway binary")
    parser.add_argument("--candidate", required=True, type=Path, help="amux binary supporting `server`")
    parser.add_argument("--inventory", type=Path, default=ROOT / "docs/server/GatewayApiContract.json")
    parser.add_argument("--startup-timeout", type=float, default=60)
    parser.add_argument("--require-positive-coverage", action="store_true", help="Fail if any declared operation lacks a successful fixture")
    args = parser.parse_args()
    reference_source, candidate_source = args.reference.resolve(strict=True), args.candidate.resolve(strict=True)
    inventory = json.loads(args.inventory.read_text())
    operations = inventory["api_boundary"]["operations"]
    plugin_operations = inventory["api_boundary"].get("included_plugin_transport_operations", [])
    fixture_root = ROOT / "target/test-fixtures"
    fixture_root.mkdir(parents=True, exist_ok=True)
    fixture = Path(tempfile.mkdtemp(prefix="gateway-compatibility-", dir=fixture_root))
    # Freeze the supplied executable bytes once. A concurrent Cargo rebuild
    # must not change which implementation later switching stages exercise.
    snapshots = fixture / "binaries"
    snapshots.mkdir()
    reference = snapshots / ("gateway.exe" if os.name == "nt" else "gateway")
    candidate = snapshots / ("amux.exe" if os.name == "nt" else "amux")
    shutil.copy2(reference_source, reference)
    shutil.copy2(candidate_source, candidate)
    report = {"reference_source": str(reference_source), "candidate_source": str(candidate_source), "reference_sha256": file_sha256(reference), "candidate_sha256": file_sha256(candidate), "baseline": inventory["baseline"], "reference": str(reference), "candidate": str(candidate), "fixture": str(fixture), "normalization": "Generated UUIDs, epoch milliseconds and salted password hashes; spec-task idempotency hashes are independently verified from version/item IDs before logical normalization. Per-trace relationships preserved. Expiry TTLs are asserted separately. Same-profile switching compares exact durable values without normalization.", "all_declared_positive_behaviors_proven": False}
    report_path = fixture / "report.json"
    print(f"Compatibility fixture: {fixture}", flush=True)
    try:
        matrices = []
        flows = []
        for label, binary, is_candidate in (("reference", reference, False), ("candidate", candidate, True)):
            with Server(binary, is_candidate, fixture / (label + "-matrix"), startup_timeout=args.startup_timeout) as server:
                matrices.append(matrix(server, operations, plugin_operations))
            with Server(binary, is_candidate, fixture / (label + "-flow"), startup_timeout=args.startup_timeout) as server:
                flows.append(Flow(server).run())
            print(label + " HTTP matrix and positive flows complete", flush=True)
        report.update(matrix_reference=matrices[0], matrix_candidate=matrices[1], positive_reference=flows[0]["trace"], positive_candidate=flows[1]["trace"])
        off_ref = middleware_off(reference, False, fixture / "reference-ui-off", args.startup_timeout)
        off_candidate = middleware_off(candidate, True, fixture / "candidate-ui-off", args.startup_timeout)
        plugin_ref = plugin_flow(reference, False, fixture / "reference-plugin", args.startup_timeout)
        plugin_candidate = plugin_flow(candidate, True, fixture / "candidate-plugin", args.startup_timeout)
        report.update(ui_auth_off_reference=off_ref, ui_auth_off_candidate=off_candidate, plugin_reference=plugin_ref["trace"], plugin_candidate=plugin_candidate["trace"])
        mismatches = []
        for name, left, right in (("HTTP method/auth/error matrix", matrices[0], matrices[1]), ("positive behavioral flow", flows[0]["trace"], flows[1]["trace"]), ("explicit ui-auth-off compatibility", off_ref, off_candidate), ("production WhatsApp plugin", plugin_ref["trace"], plugin_candidate["trace"])):
            try:
                compare(name, left, right)
            except AssertionError as error:
                mismatches.append(str(error))
        report["mismatches"] = mismatches
        require(not mismatches, "\n".join(mismatches))
        report["switching"] = switching(reference, candidate, fixture / "reference-flow", flows[0]["sentinel_task_id"], args.startup_timeout)
        covered = set()
        for method, path in set(tuple(item) for item in flows[0]["successful_operations"]) | plugin_ref["successful_operations"]:
            operation = matching_operation(method, path, operations)
            if operation:
                covered.add(operation_key(operation))
        declared = {operation_key(operation) for operation in operations}
        uncovered = sorted(declared - covered)
        report.update(status="passed-for-reported-scope", api_operations=len(operations), plugin_transport_operations=len(plugin_operations), matrix_probes=len(matrices[0]), behavioral_cases=len(flows[0]["trace"]), positive_http_success_cases=sum(1 for item in flows[0]["trace"] if "response" in item and 200 <= item["response"]["status"] < 300), positive_operations=len(covered), uncovered_positive_operations=uncovered, all_declared_positive_behaviors_proven=False, all_declared_operations_positive_covered=not uncovered, plugin_behavioral_cases=len(plugin_ref["trace"]), positive_plugin_transport_operations=2, matrix_reference=matrices[0], matrix_candidate=matrices[1], positive_reference=flows[0]["trace"], positive_candidate=flows[1]["trace"], ui_auth_off_reference=off_ref, ui_auth_off_candidate=off_candidate, remaining_coverage=["Production WhatsApp plugin sends/room creation and signed webhook ingress are compared using a loopback Graph provider. Real-provider delivery and Discord provider transport remain unverified.", "Uncovered positive operations listed explicitly need additional fixtures; auth/error parity alone is not a complete compatibility gate.", "The WebSocket fixture exercises subscribe/event/ack, but not all replay/receipt/fencing races; upstream task-stream/offline-machine smokes and native client contracts remain required."])
        report_path.write_text(json.dumps(report, indent=2, default=str) + "\n")
        print(f"PASS: {len(matrices[0])} real HTTP probes, {len(flows[0]['trace'])} behavioral cases, four persisted backend switches", flush=True)
        print(f"Positive operation coverage: {len(covered)}/{len(declared)}; {len(uncovered)} explicitly uncovered. Production WhatsApp plugin success and signed ingress compared against loopback Graph provider.", flush=True)
        print(f"Report: {report_path}", flush=True)
        return 2 if args.require_positive_coverage and uncovered else 0
    except BaseException as error:
        report.update(status="failed", error=f"{type(error).__name__}: {error}")
        report_path.write_text(json.dumps(report, indent=2, default=str) + "\n")
        print(f"FAIL: {error}\nReport: {report_path}", file=sys.stderr, flush=True)
        return 1


if __name__ == "__main__":
    def interrupted(signum, _frame):
        # Unwind Server contexts so an interrupted check does not orphan a
        # gateway process holding the isolated profile open.
        raise KeyboardInterrupt(f"signal {signum}")
    signal.signal(signal.SIGTERM, interrupted)
    sys.exit(main())
