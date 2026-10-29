#!/usr/bin/env python3
"""Credential-free native Codex acceptance (run on an isolated CI runner).

Pinned protocol references (rust-v0.162.1):
  codex-rs/app-server/tests/suite/v2/thread_revert.rs
  codex-rs/app-server/tests/common/config.rs
  codex-rs/core/tests/common/responses.rs

Uses the real app-server/history engine, but a deterministic localhost Responses
server, not a model or account. Never reads a pre-existing Codex home. This tests
sequential native writers; it does not claim simultaneous native-writer safety.
"""
import argparse
import http.server
import json
import os
from pathlib import Path
import queue
import signal
import subprocess
import tempfile
import threading
import time
import traceback


class MockResponses(http.server.ThreadingHTTPServer):
    def __init__(self):
        super().__init__(("127.0.0.1", 0), ResponsesHandler)
        self.requests = []
        self.lock = threading.Lock()
        self.worker = threading.Thread(target=self.serve_forever, daemon=True)
        self.worker.start()

    def close(self):
        self.shutdown()
        self.server_close()
        self.worker.join(timeout=5)


class ResponsesHandler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_args):
        pass

    def do_POST(self):
        if self.path != "/v1/responses":
            self.send_error(404)
            return
        if self.headers.get("Authorization"):
            self.send_error(400, "Acceptance must not send credentials")
            return
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        with self.server.lock:
            self.server.requests.append(body)
            number = len(self.server.requests)
        response_id = f"acceptance-response-{number}"
        events = [
            {"type": "response.created", "response": {"id": response_id}},
            {"type": "response.output_item.done", "item": {
                "type": "message", "role": "assistant", "id": f"acceptance-message-{number}",
                "content": [{"type": "output_text", "text": "Synthetic acceptance acknowledged."}]}},
            {"type": "response.completed", "response": {"id": response_id, "usage": {
                "input_tokens": 0, "input_tokens_details": None,
                "output_tokens": 0, "output_tokens_details": None, "total_tokens": 0}}},
        ]
        data = "".join(f"event: {event['type']}\ndata: {json.dumps(event)}\n\n" for event in events).encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


class AppServer:
    def __init__(self, binary, env, cwd, logs, label, launcher=None):
        self.incoming = queue.Queue()
        self.pending = []
        self.next_id = 0
        self.label = label
        self.log = (logs / f"{label}-rpc.jsonl").open("w", encoding="utf-8")
        self.stderr = (logs / f"{label}-stderr.log").open("w", encoding="utf-8")
        self.process = subprocess.Popen(
            launcher or [str(binary), "app-server", "--listen", "stdio://"], cwd=cwd, env=env,
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=self.stderr,
            text=True, bufsize=1, start_new_session=True,
        )
        self.reader = threading.Thread(target=self.read_stdout, daemon=True)
        self.reader.start()
        try:
            result = self.request("initialize", {
                "clientInfo": {"name": "codexm_acceptance", "version": "1"},
                "capabilities": {"experimentalApi": True},
            })
            assert result["codexHome"] == env["CODEX_HOME"], result
            self.send({"method": "initialized"})
        except BaseException:
            self.close()
            raise

    def read_stdout(self):
        try:
            for line in self.process.stdout:
                self.log.write(line)
                self.log.flush()
                if line.startswith("codexm:") or line.startswith("session sync:"):
                    continue
                self.incoming.put(json.loads(line))
        except BaseException as error:
            self.incoming.put(error)
        finally:
            self.incoming.put(EOFError("app-server stdout closed"))

    def send(self, message):
        self.process.stdin.write(json.dumps(message) + "\n")
        self.process.stdin.flush()

    def wait(self, predicate, timeout=90):
        deadline = time.monotonic() + timeout
        while True:
            for index, message in enumerate(self.pending):
                if predicate(message):
                    return self.pending.pop(index)
            try:
                message = self.incoming.get(timeout=max(0.01, deadline - time.monotonic()))
            except queue.Empty:
                raise TimeoutError("app-server did not produce expected response; inspect RPC/stderr logs")
            if isinstance(message, BaseException):
                raise message
            # Never authorize a tool, command, network, or permission request.
            if "id" in message and "method" in message:
                self.send({"id": message["id"], "error": {"code": -32601, "message": "No tools allowed in acceptance"}})
                raise AssertionError(f"Unexpected server request: {message['method']}")
            if predicate(message):
                return message
            self.pending.append(message)
            if time.monotonic() >= deadline:
                raise TimeoutError("app-server response deadline")

    def request(self, method, params):
        self.next_id += 1
        request_id = self.next_id
        self.send({"id": request_id, "method": method, "params": params})
        message = self.wait(lambda item: item.get("id") == request_id)
        if "error" in message:
            raise AssertionError(f"{method}: {message['error']}")
        result = message["result"]
        if method in ("thread/start", "thread/fork", "thread/revert", "thread/resume"):
            thread = result.get("thread", {})
            print(json.dumps({"stage": self.label, "method": method, "threadId": thread.get("id"),
                              "path": thread.get("path"), "historyMode": thread.get("historyMode"),
                              "turnCount": len(thread.get("turns", []))}), flush=True)
        return result

    def turn(self, thread_id, text):
        result = self.request("turn/start", {"threadId": thread_id, "input": [
            {"type": "text", "text": text, "text_elements": []}]})
        turn_id = result["turn"]["id"]
        event = self.wait(lambda item: item.get("method") == "turn/completed"
                          and item["params"]["threadId"] == thread_id
                          and item["params"]["turn"]["id"] == turn_id)
        assert event["params"]["turn"]["status"] == "completed", event
        return turn_id

    def items(self, thread_id):
        items, cursor = [], None
        while True:
            params = {"threadId": thread_id, "limit": 100, "sortDirection": "asc"}
            if cursor is not None:
                params["cursor"] = cursor
            page = self.request("thread/items/list", params)
            items.extend(page["data"])
            print(json.dumps({"stage": self.label, "method": "thread/items/list", "threadId": thread_id,
                              "pageCount": len(page["data"]), "hasNext": bool(page.get("nextCursor")),
                              "items": synthetic_item_summary(page["data"])}), flush=True)
            cursor = page.get("nextCursor")
            if not cursor:
                return items

    def close(self):
        forced = False
        if self.process.poll() is None:
            self.process.stdin.close()  # Upstream test helper uses EOF + drain, too.
            try:
                self.process.wait(timeout=20)
            except subprocess.TimeoutExpired:
                forced = True
                # Clean up only this test-owned process group, including a wrapper's child.
                os.killpg(self.process.pid, signal.SIGTERM)
                try:
                    self.process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    os.killpg(self.process.pid, signal.SIGKILL)
                    self.process.wait(timeout=10)
        self.reader.join(timeout=5)
        self.log.close()
        self.stderr.close()
        assert not forced, "app-server failed orderly EOF shutdown; acceptance cannot continue"
        assert self.process.returncode == 0, self.process.returncode

    def __enter__(self):
        return self

    def __exit__(self, *_args):
        self.close()


def synthetic_item_summary(items):
    # Log only structure and this harness's synthetic markers, not instruction bodies.
    import re
    return {"types": [item.get("type", item.get("item", {}).get("type")) for item in items],
            "markers": sorted(set(re.findall(r"codexm-acceptance-[a-z-]+-d895a1", json.dumps(items))))}


def profile_diagnostics(profiles, stage):
    for name, home in profiles.items():
        for path in sorted(home.glob("sessions/**/*.jsonl")):
            records = [json.loads(line) for line in path.read_text().splitlines() if line.strip()]
            metadata = next((record["payload"] for record in records if record.get("type") == "session_meta"), {})
            print(json.dumps({"stage": stage, "profile": name, "relativePath": str(path.relative_to(home)),
                              "bytes": path.stat().st_size, "lines": len(records),
                              "meta": {key: metadata.get(key) for key in ("id", "history_mode", "history_base", "cwd")},
                              "ordinals": [records[0].get("ordinal"), records[-1].get("ordinal")],
                              "synthetic": synthetic_item_summary(records)}), flush=True)


def assert_markers(value, present, absent=()):
    encoded = json.dumps(value)
    for marker in present:
        assert marker in encoded, f"Inherited marker missing: {marker}"
    for marker in absent:
        assert marker not in encoded, f"Reverted marker unexpectedly replayed: {marker}"


def run_acceptance(args, logs):
    with tempfile.TemporaryDirectory(prefix="codexm-native-acceptance-") as temporary:
        base = Path(temporary).resolve()
        project = base / "project"
        project.mkdir()
        for name in ("home", "manager", "profiles", "tmp", "runtime", "unused-codex"):
            (base / name).mkdir(mode=0o700)
        # Allowlist rather than inherit CI secrets, proxy settings, or auth variables.
        env = {"PATH": os.environ.get("PATH", "/usr/bin:/bin"), "LANG": "C.UTF-8",
               "HOME": str(base / "home"), "CODEX_HOME": str(base / "unused-codex"),
               "CODEXM_HOME": str(base / "manager"), "CODEXM_PROFILES_HOME": str(base / "profiles"),
               "TMPDIR": str(base / "tmp"), "XDG_RUNTIME_DIR": str(base / "runtime"),
               "CODEXM_CODEX_BIN": str(args.codex)}
        command_log = (logs / "codexm.log").open("w", encoding="utf-8")

        def codexm(*arguments, expected_error=None):
            result = subprocess.run([str(args.codexm), *arguments], cwd=project, env=env,
                                    text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=60)
            command_log.write(json.dumps(arguments) + "\n" + result.stdout + "\n")
            command_log.flush()
            if expected_error is None:
                assert result.returncode == 0, f"codexm {arguments}: {result.stdout}"
            else:
                assert result.returncode != 0 and expected_error in result.stdout, result.stdout

        version = subprocess.run([str(args.codex), "--version"], cwd=project, env=env,
                                 text=True, capture_output=True, check=True, timeout=15).stdout.strip()
        assert args.expected_version in version, f"Expected pinned Codex {args.expected_version}, got {version}"
        codexm("add", "first")
        codexm("add", "second")
        configuration = json.loads((base / "manager" / "config.json").read_text())
        profiles = {name: Path(value["codex_home"]).resolve()
                    for name, value in configuration["profiles"].items()}
        assert all(home.is_relative_to(base) for home in profiles.values())
        server = MockResponses()
        try:
            config = f'''model = "mock-model"
model_provider = "mock_provider"
approval_policy = "never"
sandbox_mode = "read-only"
[model_providers.mock_provider]
name = "Local synthetic acceptance only"
base_url = "http://127.0.0.1:{server.server_port}/v1"
wire_api = "responses"
requires_openai_auth = false
request_max_retries = 0
stream_max_retries = 0
'''
            for home in profiles.values():
                home.mkdir(mode=0o700, parents=True, exist_ok=True)
                (home / "config.toml").write_text(config)
            markers = {name: f"codexm-acceptance-{name}-d895a1" for name in
                       ("parent-one", "parent-two", "child-keep", "child-discard", "profile-two", "profile-one")}
            first_env = dict(env, CODEX_HOME=str(profiles["first"]))
            second_env = dict(env, CODEX_HOME=str(profiles["second"]))
            with AppServer(args.codex, first_env, project, logs, "first-create") as app:
                parent = app.request("thread/start", {
                    "cwd": str(project), "historyMode": "paginated", "model": "mock-model",
                    "modelProvider": "mock_provider", "approvalPolicy": "never", "sandbox": "read-only",
                    "baseInstructions": "Synthetic history acceptance. Reply briefly; do not use tools.",
                })["thread"]
                app.turn(parent["id"], markers["parent-one"])
                app.turn(parent["id"], markers["parent-two"])
                child = app.request("thread/fork", {"threadId": parent["id"], "cwd": str(project),
                                                     "excludeTurns": True})["thread"]
                app.turn(child["id"], markers["child-keep"])
                discarded_turn = app.turn(child["id"], markers["child-discard"])
                reverted = app.request("thread/revert", {"threadId": child["id"],
                                                          "beforeTurnId": discarded_turn})["thread"]
                assert reverted["id"] == child["id"]
                assert reverted["path"] != child["path"], "Revert did not create a physical rollout"
                assert "_" in Path(reverted["path"]).name, "Revert fixture lacks distinct physical rollout ID"
                expected = [markers[name] for name in ("parent-one", "parent-two", "child-keep")]
                assert_markers(app.items(child["id"]), expected, [markers["child-discard"]])
            original_child_path = Path(child["path"])
            original_child_bytes = original_child_path.read_bytes()
            assert markers["child-discard"].encode() in original_child_bytes
            # Only codexm copies histories. No native databases or credentials are copied.
            codexm("session", "init", "--project", str(project), "--profile", "first", "--import-existing")
            codexm("session", "sync", "--project", str(project), "--profile", "second")
            assert not list(profiles["second"].glob("*.sqlite*")), "Sync copied native database state"
            assert not (profiles["second"] / "auth.json").exists()
            profile_diagnostics(profiles, "before-second-resume")
            with AppServer(args.codex, second_env, project, logs, "second-resume") as app:
                app.request("thread/resume", {"threadId": child["id"], "excludeTurns": True})
                turns = app.request("thread/turns/list", {"threadId": child["id"], "limit": 100})
                print(json.dumps({"stage": "second-resume", "turnCount": len(turns["data"]),
                                  "turnIds": [turn["id"] for turn in turns["data"]],
                                  "hasNext": bool(turns.get("nextCursor"))}), flush=True)
                before_items = app.items(child["id"])
                ui_error = None
                try:
                    assert_markers(before_items, expected, [markers["child-discard"]])
                except AssertionError as error:
                    # Keep this failure: a diagnostic model replay must not waive UI history.
                    ui_error = error
                app.turn(child["id"], markers["profile-two"])
                replay_input = server.requests[-1]["input"]
                print(json.dumps({"stage": "second-resume-model-replay", "synthetic": synthetic_item_summary(replay_input),
                                  "inheritedPresent": {marker: marker in json.dumps(replay_input) for marker in expected},
                                  "discardedPresent": markers["child-discard"] in json.dumps(replay_input)}), flush=True)
                if ui_error is not None:
                    after_items = app.items(child["id"])
                    after_turns = app.request("thread/turns/list", {"threadId": child["id"], "limit": 100})
                    print(json.dumps({"stage": "second-after-diagnostic-continuation",
                                      "itemCount": len(after_items), "synthetic": synthetic_item_summary(after_items),
                                      "turnCount": len(after_turns["data"]),
                                      "turnIds": [turn["id"] for turn in after_turns["data"]],
                                      "originalUiGate": "failed"}), flush=True)
                    try:
                        assert_markers(replay_input, expected + [markers["profile-two"]], [markers["child-discard"]])
                        print("Diagnostic native model-context replay passed; UI-history gate still failed.", flush=True)
                    except AssertionError as replay_error:
                        print(f"Diagnostic native model-context replay also failed: {replay_error}", flush=True)
                    raise ui_error
                assert_markers(replay_input, expected + [markers["profile-two"]], [markers["child-discard"]])
            codexm("session", "sync", "--project", str(project), "--profile", "second")
            codexm("session", "sync", "--project", str(project), "--profile", "first")
            assert original_child_path.read_bytes() == original_child_bytes, "Source tail changed during round trip"
            with AppServer(args.codex, first_env, project, logs, "first-resume") as app:
                app.request("thread/resume", {"threadId": child["id"], "excludeTurns": True})
                expected.append(markers["profile-two"])
                assert_markers(app.items(child["id"]), expected, [markers["child-discard"]])
                app.turn(child["id"], markers["profile-one"])
                assert_markers(server.requests[-1]["input"], expected + [markers["profile-one"]],
                               [markers["child-discard"]])
            codexm("session", "sync", "--project", str(project), "--profile", "first")
            codexm("session", "sync", "--project", str(project), "--profile", "second")
            codexm("session", "status", "--project", str(project), "--profile", "second")
            # Hold the actual native child under codexm's cooperating-writer lease.
            launcher = [str(args.codexm), "run", "--unmanaged", "--project", str(project),
                        "second", "--", "app-server", "--listen", "stdio://"]
            with AppServer(args.codex, second_env, project, logs, "leased-native", launcher=launcher) as app:
                app.request("thread/resume", {"threadId": child["id"], "excludeTurns": True})
                codexm("session", "sync", "--project", str(project), "--profile", "first",
                       expected_error="another codexm run")
                codexm("session", "sync", "--project", str(project), "--profile", "second",
                       expected_error="another codexm run")
            codexm("session", "sync", "--project", str(project), "--profile", "second")
            assert all(not (home / "auth.json").exists() for home in profiles.values())
            return {"status": "passed", "codexVersion": version, "parentThread": parent["id"],
                    "childThread": child["id"], "mockResponses": len(server.requests),
                    "verified": ["native paginated start", "native fork", "native revert with physical ID",
                                 "profile-one to profile-two cold resume", "inherited history replay",
                                 "discarded turn absent", "native continuation", "profile-two to profile-one resume",
                                 "original source tail preserved", "cooperating sync excluded during native run"],
                    "limitations": ["local deterministic provider, not a real model/account",
                                    "sequential writers, not simultaneous native processes"]}
        finally:
            server.close()
            command_log.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--codex", type=Path, required=True)
    parser.add_argument("--codexm", type=Path, required=True)
    parser.add_argument("--artifacts", type=Path, required=True)
    parser.add_argument("--expected-version", default="0.162.1")
    args = parser.parse_args()
    args.codex = args.codex.resolve(strict=True)
    args.codexm = args.codexm.resolve(strict=True)
    args.artifacts.mkdir(parents=True, exist_ok=True)
    try:
        result = run_acceptance(args, args.artifacts)
    except BaseException as error:
        result = {"status": "failed", "error": str(error)}
        (args.artifacts / "summary.json").write_text(json.dumps(result, indent=2) + "\n")
        traceback.print_exc()
        raise SystemExit(1)
    (args.artifacts / "summary.json").write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps(result, indent=2))


if __name__ == "__main__":
    main()
