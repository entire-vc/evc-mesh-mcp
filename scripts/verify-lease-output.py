#!/usr/bin/env python3
"""Live lease-output check. Creates and cancels one owned canary per target.

Credentials are read only from a named env-file carrier; capabilities and raw
responses stay in memory. --expect-leak is the pre-deploy negative control.
"""
import argparse
import json
import os
from pathlib import Path
import queue
import select
import shlex
import subprocess
import threading
import urllib.parse
import urllib.request
import uuid


def require(condition, label):
    if not condition:
        raise RuntimeError(label)


def request_json(url, key, method="GET", body=None):
    headers = {"X-Agent-Key": key, "Content-Type": "application/json",
               "Accept": "application/json, text/event-stream"}
    data = None if body is None else json.dumps(body).encode()
    with urllib.request.urlopen(urllib.request.Request(url, data, headers, method=method), timeout=45) as response:
        raw = response.read().decode()
    if not raw:
        return {}
    if raw.startswith("event:") or raw.startswith("data:"):
        raw = next(line[5:].strip() for line in raw.splitlines() if line.startswith("data:"))
    return json.loads(raw)


class Client:
    def __init__(self, target, key, api, binary):
        self.target, self.key, self.seq = target, key, 0
        self.process = None
        self.stream = None
        self.messages = queue.Queue()
        if target == "stdio":
            env = dict(os.environ, MESH_AGENT_KEY=key, MESH_API_URL=api, MESH_MCP_PROFILE="full")
            self.process = subprocess.Popen([binary, "--transport", "stdio"], env=env,
                                            stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                            stderr=subprocess.DEVNULL, text=True)
        elif target.endswith("/sse"):
            self.stream = urllib.request.urlopen(urllib.request.Request(target, headers={"X-Agent-Key": key}), timeout=45)
            threading.Thread(target=self.read_sse, daemon=True).start()
            endpoint = self.messages.get(timeout=45)
            require(isinstance(endpoint, str), "SSE endpoint missing")
            parsed = urllib.parse.urlparse(endpoint)
            require(not parsed.netloc or parsed.netloc == urllib.parse.urlparse(target).netloc, "SSE endpoint host changed")
            if parsed.netloc:
                self.endpoint = endpoint
            elif endpoint.startswith("/message"):
                self.endpoint = target[:-4] + endpoint
            else:
                self.endpoint = urllib.parse.urljoin(target, endpoint)
        self.rpc("initialize", {"protocolVersion": "2025-03-26", "capabilities": {},
                                "clientInfo": {"name": "lease-output-check", "version": "1"}})

    def read_sse(self):
        event = None
        try:
            for raw in self.stream:
                line = raw.decode().strip()
                if line.startswith("event:"):
                    event = line[6:].strip()
                elif line.startswith("data:"):
                    data = line[5:].strip()
                    self.messages.put(data if event == "endpoint" else json.loads(data))
        except Exception:
            self.messages.put(None)

    def rpc(self, method, params):
        self.seq += 1
        payload = {"jsonrpc": "2.0", "id": self.seq, "method": method, "params": params}
        if self.process:
            self.process.stdin.write(json.dumps(payload) + "\n")
            self.process.stdin.flush()
            require(select.select([self.process.stdout], [], [], 45)[0], "stdio timeout")
            result = json.loads(self.process.stdout.readline())
        elif self.stream:
            request_json(self.endpoint, self.key, "POST", payload)
            result = self.messages.get(timeout=45)
            require(isinstance(result, dict), "SSE response missing")
        else:
            result = request_json(self.target, self.key, "POST", payload)
        require("error" not in result, "RPC failed")
        return result["result"]

    def tool(self, name, args):
        result = self.rpc("tools/call", {"name": name, "arguments": args})
        require(not result.get("isError"), "tool failed: " + name)
        text = next(item["text"] for item in result["content"] if item.get("type") == "text")
        return result, json.loads(text)

    def close(self):
        if self.stream:
            self.stream.close()
        if self.process:
            self.process.stdin.close()
            try:
                self.process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                self.process.terminate()
                self.process.wait(timeout=5)


def contains_capability(value):
    if isinstance(value, dict):
        return any(key in {"checkout_token", "lease_token", "fencing_token"}
                   or contains_capability(child) for key, child in value.items())
    if isinstance(value, list):
        return any(contains_capability(child) for child in value)
    return False


def check(args, key, target):
    rest = lambda path, method="GET", body=None: request_json(args.api + path, key, method, body)
    me = rest("/api/v1/agents/me")
    client = Client(target, key, args.api, args.binary)
    task_id = None
    try:
        # Narrow duplicate/read checks before creating the disposable fixture.
        client.tool("get_my_tasks", {"project_id": args.project, "status_category": "in_progress", "limit": 200})
        title = "lease-output-canary-" + uuid.uuid4().hex
        client.tool("list_tasks", {"project_id": args.project, "search": title})
        _, task = client.tool("create_task", {"project_id": args.project, "title": title,
            "description": "Временный canary #3364942c; не брать в работу. Будет cancelled после проверки MCP.",
            "status_slug": "in_progress", "priority": "none", "assignee_id": me["id"],
            "assignee_type": "agent", "labels": ["no-intake", "owner-lock", "canary"]})
        task_id = task["id"]
        checkout_wire, checkout = client.tool("checkout_task", {"task_id": task_id, "ttl_minutes": 10})
        baseline = rest("/api/v1/tasks/" + task_id)
        capability = baseline.get("checkout_token")
        require(bool(capability), "REST positive control has no capability")
        require(baseline.get("checked_out_by") == me["id"], "canary holder mismatch")
        metadata = ["checked_out_by", "checkout_generation", "checkout_expires", "checkout_acquired_at"]
        for name in metadata:
            require(baseline.get(name) is not None, "REST metadata missing: " + name)
        # Legacy MCP checkouts carry no session_id. Preserve its presence or
        # absence exactly; do not introduce a new fencing protocol here.
        metadata.append("checkout_session_id")
        reads = [
            ("get_task", {"task_id": task_id}),
            ("get_task", {"task_id": task_id, "full": True}),
            ("get_task_context", {"task_id": task_id}),
            ("list_tasks", {"project_id": args.project, "search": title}),
            ("list_tasks", {"project_id": args.project, "search": title, "full": True}),
            ("get_my_tasks", {"project_id": args.project, "status_category": "in_progress", "limit": 200}),
            ("get_my_tasks", {"project_id": args.project, "status_category": "in_progress", "limit": 200, "full": True}),
        ]
        leaks = 0
        for name, params in reads:
            wire, output = client.tool(name, params)
            leaked = contains_capability(output) or capability in json.dumps(wire)
            leaks += int(leaked)
            if not args.expect_leak:
                require(not leaked, "read capability leaked: " + name)
            if name == "get_task" and params.get("full"):
                for field in metadata:
                    require(output["task"].get(field) == baseline.get(field), "MCP metadata changed: " + field)
            if name == "get_my_tasks":
                require(any(task["id"] == task_id for task in output["tasks"]), "canary absent from assigned tasks")
        extended_wire, extended = client.tool("extend_checkout", {"task_id": task_id, "ttl_minutes": 20})
        after = rest("/api/v1/tasks/" + task_id)
        require(after.get("checkout_token") == capability, "extend rotated capability")
        require(after["checkout_generation"] == baseline["checkout_generation"], "extend changed generation")
        require(after["checkout_expires"] > baseline["checkout_expires"], "extend did not advance expiry")
        if not args.expect_leak:
            require(capability not in json.dumps(checkout_wire), "checkout wire leaked capability")
            require(capability not in json.dumps(extended_wire), "extend wire leaked capability")
            require(not contains_capability(checkout) and not contains_capability(extended), "mutation capability field leaked")
        _, released = client.tool("release_task", {"task_id": task_id})
        require(released.get("released") is True, "release failed")
        final = rest("/api/v1/tasks/" + task_id)
        require(not final.get("checkout_token") and not final.get("checked_out_by"), "release did not clear lease")
        require(final["checkout_generation"] == baseline["checkout_generation"], "release reset generation")
        require(leaks > 0 if args.expect_leak else leaks == 0, "negative/positive control mismatch")
        print(json.dumps({"target": target, "task_id": task_id, "read_cases": len(reads),
              "read_leaks": leaks, "REST_positive_control": True, "metadata": "PASS",
              "checkout_extend_release": "PASS", "expected_red": args.expect_leak}))
    finally:
        if task_id:
            # Only this newly created, owned canary can be touched in cleanup.
            state = rest("/api/v1/tasks/" + task_id)
            if state.get("checkout_token") and state.get("checked_out_by") == me["id"]:
                rest("/api/v1/tasks/" + task_id + "/checkout", "DELETE", {"checkout_token": state["checkout_token"]})
            client.tool("move_task", {"task_id": task_id, "status_slug": "cancelled",
                                     "comment": "Canary #3364942c: live MCP проверка завершена, fixture убран."})
            cancelled = rest("/api/v1/tasks/" + task_id)
            require(cancelled.get("status_id") != state.get("status_id"), "canary cleanup failed")
            print(json.dumps({"task_id": task_id, "cleanup": "cancelled"}))
        client.close()


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--credential-file", default=str(Path.home() / ".config/agents/linus.env"))
    parser.add_argument("--api", default="https://mesh.entire.host")
    parser.add_argument("--project", default="c6e35032-36d5-4045-b30d-6cf9e35c3dee")
    parser.add_argument("--binary", default=str(Path.home() / "bin/mesh-mcp"))
    parser.add_argument("--target", action="append", required=True, help="stdio, streamable HTTP URL, or SSE URL ending /sse")
    parser.add_argument("--expect-leak", action="store_true")
    arguments = parser.parse_args()
    carrier = {}
    for line in Path(arguments.credential_file).read_text().splitlines():
        if line.startswith("export "):
            line = line[7:]
        if line.startswith("MESH_AGENT_KEY="):
            carrier["MESH_AGENT_KEY"] = shlex.split(line.split("=", 1)[1])[0]
    try:
        for destination in arguments.target:
            check(arguments, carrier["MESH_AGENT_KEY"], destination)
    except Exception as error:
        # Never print response bodies, capability values, headers or tracebacks.
        print("FAIL: " + (str(error) if type(error) is RuntimeError else type(error).__name__))
        raise SystemExit(1)
