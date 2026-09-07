#!/usr/bin/env python3
"""Deterministic contract tests for configure-aggregator.py."""

from __future__ import annotations

import base64
import copy
import json
import stat
import subprocess
import sys
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

SCRIPT = Path(__file__).with_name("configure-aggregator.py")
BASE_CONFIG = {
    "sortCriteria": {"global": []},
    "formatter": {"id": "minimalisticgdrive"},
    "presets": [],
    "checkOwned": True,
}


class ApiState:
    def __init__(self) -> None:
        self.config = copy.deepcopy(BASE_CONFIG)
        self.gets = 0
        self.puts: list[dict] = []
        self.drift_on_get: int | None = None
        self.get_status = 200
        self.error_body = "withheld-error-secret"
        self.redirect_location: str | None = None
        self.get_paths: list[str] = []
        self.server_normalizes_proxy = False
        self.auth = ""


def handler_for(state: ApiState):
    class Handler(BaseHTTPRequestHandler):
        def reply(self, status: int, value: dict | str) -> None:
            raw = (
                json.dumps(value).encode()
                if isinstance(value, dict)
                else value.encode()
            )
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def authorised(self) -> bool:
            return self.headers.get("Authorization") == state.auth

        def do_GET(self) -> None:
            state.gets += 1
            state.get_paths.append(self.path)
            if not self.authorised():
                self.reply(401, state.error_body)
                return
            if state.redirect_location:
                self.send_response(302)
                self.send_header("Location", state.redirect_location)
                self.end_headers()
                return
            if state.get_status != 200:
                self.reply(state.get_status, state.error_body)
                return
            if state.drift_on_get == state.gets:
                state.config = {**state.config, "addonDescription": "concurrent-drift"}
            config = copy.deepcopy(state.config)
            config.update(
                {
                    "uuid": "11111111-1111-4111-8111-111111111111",
                    "trusted": False,
                    "accessKey": "api-managed-secret",
                }
            )
            self.reply(
                200,
                {"success": True, "data": {"userData": config}},
            )

        def do_PUT(self) -> None:
            if not self.authorised():
                self.reply(401, state.error_body)
                return
            size = int(self.headers.get("Content-Length", "0"))
            payload = json.loads(self.rfile.read(size))
            state.puts.append(copy.deepcopy(payload))
            state.config = copy.deepcopy(payload["config"])
            if state.server_normalizes_proxy:
                state.config["proxy"]["url"] = "server-ciphertext-url"
                state.config["proxy"]["credentials"] = "server-ciphertext-credentials"
            self.reply(200, {"success": True, "data": {"uuid": "withheld"}})

        def log_message(self, _format: str, *_args: object) -> None:
            pass

    return Handler


class ConfigureAggregatorTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.state = ApiState()
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), handler_for(self.state))
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

        self.uuid = "11111111-1111-4111-8111-111111111111"
        self.password = "credential-secret"
        token = base64.b64encode(
            f"{self.uuid}:{self.password}".encode()
        ).decode()
        self.state.auth = "Basic " + token
        self.base_url = f"http://127.0.0.1:{self.server.server_port}"
        self.credentials = self.write_json(
            "credentials.json",
            {
                "base_url": self.base_url,
                "uuid": self.uuid,
                "password": self.password,
            },
        )

    def tearDown(self) -> None:
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)
        self.temp.cleanup()

    def write_json(self, name: str, value: dict, mode: int = 0o600) -> Path:
        path = self.root / name
        path.write_text(json.dumps(value), encoding="utf-8")
        path.chmod(mode)
        return path

    def run_script(self, *args: object) -> subprocess.CompletedProcess[str]:
        return subprocess.run(
            [sys.executable, str(SCRIPT), *map(str, args)],
            text=True,
            capture_output=True,
            timeout=10,
            check=False,
        )

    def assert_values_withheld(self, result: subprocess.CompletedProcess[str]) -> None:
        output = result.stdout + result.stderr
        self.assertNotIn(self.password, output)
        self.assertNotIn(self.uuid, output)
        self.assertNotIn("api-managed-secret", output)
        self.assertNotIn(self.state.error_body, output)

    def test_snapshot_is_private_and_does_not_echo_values(self) -> None:
        self.state.config["addonDescription"] = "config-value-secret"
        output = self.root / "baseline.json"

        result = self.run_script(
            "snapshot", "--credentials", self.credentials, "--output", output
        )

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(stat.S_IMODE(output.stat().st_mode), 0o600)
        saved = json.loads(output.read_text(encoding="utf-8"))
        self.assertEqual(saved["addonDescription"], "config-value-secret")
        self.assertNotIn("uuid", saved)
        self.assertNotIn("trusted", saved)
        self.assertNotIn("accessKey", saved)
        self.assertNotIn("config-value-secret", result.stdout + result.stderr)
        self.assertEqual(self.state.get_paths, ["/api/v1/user?raw=true"])
        self.assert_values_withheld(result)

    def test_apply_writes_once_then_reruns_as_noop(self) -> None:
        baseline = self.write_json("baseline.json", BASE_CONFIG)
        desired_config = {**BASE_CONFIG, "addonName": "REL-03 gate"}
        desired = self.write_json("desired.json", desired_config)

        first = self.run_script(
            "apply",
            "--credentials",
            self.credentials,
            "--baseline",
            baseline,
            "--desired",
            desired,
        )
        second = self.run_script(
            "apply",
            "--credentials",
            self.credentials,
            "--baseline",
            baseline,
            "--desired",
            desired,
        )

        self.assertEqual(first.returncode, 0, first.stderr)
        self.assertEqual(second.returncode, 0, second.stderr)
        self.assertEqual(len(self.state.puts), 1)
        self.assertEqual(self.state.puts[0], {"config": desired_config})
        self.assertIn("Applied and verified", first.stdout)
        self.assertIn("No change", second.stdout)
        self.assertIn("Manual steps NOT performed", first.stdout)
        self.assertIn("Manual steps NOT performed", second.stdout)
        self.assert_values_withheld(first)
        self.assert_values_withheld(second)

    def test_explicit_server_normalized_paths_are_idempotent(self) -> None:
        baseline_config = {
            **BASE_CONFIG,
            "proxy": {
                "enabled": True,
                "id": "mediaflow",
                "url": "old-ciphertext-url",
                "credentials": "old-ciphertext-credentials",
            },
        }
        desired_config = {**baseline_config, "addonName": "wanted"}
        self.state.config = copy.deepcopy(baseline_config)
        self.state.server_normalizes_proxy = True
        baseline = self.write_json("baseline.json", baseline_config)
        desired = self.write_json("desired.json", desired_config)
        flags = (
            "--server-normalized-path",
            "/proxy/url",
            "--server-normalized-path",
            "/proxy/credentials",
        )

        first = self.run_script(
            "apply",
            "--credentials",
            self.credentials,
            "--baseline",
            baseline,
            "--desired",
            desired,
            *flags,
        )
        second = self.run_script(
            "apply",
            "--credentials",
            self.credentials,
            "--baseline",
            baseline,
            "--desired",
            desired,
            *flags,
        )

        self.assertEqual(first.returncode, 0, first.stderr)
        self.assertEqual(second.returncode, 0, second.stderr)
        self.assertEqual(len(self.state.puts), 1)
        self.assertIn("Applied and verified", first.stdout)
        self.assertIn("No change", second.stdout)
        self.assert_values_withheld(first)
        self.assert_values_withheld(second)

    def test_server_normalized_path_cannot_hide_a_desired_change(self) -> None:
        baseline_config = {
            **BASE_CONFIG,
            "proxy": {"url": "old", "credentials": "same"},
        }
        desired_config = {
            **BASE_CONFIG,
            "proxy": {"url": "new", "credentials": "same"},
        }
        baseline = self.write_json("baseline.json", baseline_config)
        desired = self.write_json("desired.json", desired_config)

        result = self.run_script(
            "apply",
            "--credentials",
            self.credentials,
            "--baseline",
            baseline,
            "--desired",
            desired,
            "--server-normalized-path",
            "/proxy/url",
        )

        self.assertEqual(result.returncode, 1)
        self.assertEqual(self.state.gets, 0)
        self.assertEqual(self.state.puts, [])
        self.assertIn("cannot be ignored", result.stderr)
        self.assert_values_withheld(result)

    def test_existing_drift_is_refused_without_put(self) -> None:
        baseline = self.write_json("baseline.json", BASE_CONFIG)
        desired = self.write_json(
            "desired.json", {**BASE_CONFIG, "addonName": "wanted"}
        )
        self.state.config["addonDescription"] = "existing-drift-secret"

        result = self.run_script(
            "apply",
            "--credentials",
            self.credentials,
            "--baseline",
            baseline,
            "--desired",
            desired,
        )

        self.assertEqual(result.returncode, 3)
        self.assertEqual(self.state.puts, [])
        self.assertIn("REFUSED", result.stderr)
        self.assertNotIn("existing-drift-secret", result.stdout + result.stderr)
        self.assert_values_withheld(result)

    def test_second_read_catches_concurrent_drift(self) -> None:
        baseline = self.write_json("baseline.json", BASE_CONFIG)
        desired = self.write_json(
            "desired.json", {**BASE_CONFIG, "addonName": "wanted"}
        )
        self.state.drift_on_get = 2

        result = self.run_script(
            "apply",
            "--credentials",
            self.credentials,
            "--baseline",
            baseline,
            "--desired",
            desired,
        )

        self.assertEqual(result.returncode, 3)
        self.assertEqual(self.state.gets, 2)
        self.assertEqual(self.state.puts, [])
        self.assertIn("changed during", result.stderr)
        self.assert_values_withheld(result)

    def test_private_file_mode_is_required_before_network_access(self) -> None:
        self.credentials.chmod(0o644)

        result = self.run_script(
            "snapshot",
            "--credentials",
            self.credentials,
            "--output",
            self.root / "never-created.json",
        )

        self.assertEqual(result.returncode, 1)
        self.assertEqual(self.state.gets, 0)
        self.assertIn("mode 0600", result.stderr)
        self.assert_values_withheld(result)

    def test_http_error_body_is_withheld(self) -> None:
        self.state.get_status = 401

        result = self.run_script(
            "snapshot",
            "--credentials",
            self.credentials,
            "--output",
            self.root / "never-created.json",
        )

        self.assertEqual(result.returncode, 1)
        self.assertIn("HTTP 401", result.stderr)
        self.assert_values_withheld(result)

    def test_redirect_is_refused_without_forwarding_credentials(self) -> None:
        self.state.redirect_location = self.base_url + "/redirected"

        result = self.run_script(
            "snapshot",
            "--credentials",
            self.credentials,
            "--output",
            self.root / "never-created.json",
        )

        self.assertEqual(result.returncode, 1)
        self.assertEqual(self.state.gets, 1)
        self.assertIn("HTTP 302", result.stderr)
        self.assert_values_withheld(result)


if __name__ == "__main__":
    unittest.main()
