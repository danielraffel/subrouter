#!/usr/bin/env python3
from __future__ import annotations

import hashlib
import json
import os
import signal
import socket
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
RUNNER = ROOT / "deploy" / "run-shadow-rehearsal.py"


def _free_port() -> int:
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        return int(listener.getsockname()[1])


def _listening(port: int) -> bool:
    try:
        with socket.create_connection(("127.0.0.1", port), timeout=0.2):
            return True
    except OSError:
        return False


class ShadowRehearsalTest(unittest.TestCase):
    def setUp(self) -> None:
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name)
        self.root.chmod(0o700)
        self.workspace_witness = self.root / "workspace.txt"
        self.canary_witness = self.root / "canary.txt"
        self.candidate = self._script(
            "candidate.py",
            """
import argparse
import json
import os
import signal
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

parser = argparse.ArgumentParser()
parser.add_argument("command")
parser.add_argument("--addr", required=True)
args = parser.parse_args()
host, port = args.addr.rsplit(":", 1)
Path(os.environ["TEST_WORKSPACE_WITNESS"]).write_text(os.environ["SUBROUTER_SHADOW_WORKSPACE"])

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        body = json.dumps({"ok": True, "draining": False}).encode()
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *_args):
        pass

server = ThreadingHTTPServer((host, int(port)), Handler)
signal.signal(signal.SIGTERM, lambda *_args: (_ for _ in ()).throw(SystemExit(0)))
server.serve_forever()
""",
        )
        self.prepare = self._script(
            "prepare.py",
            """
import os
from pathlib import Path
state = Path(os.environ["SUBROUTER_SHADOW_STATE_DIR"])
(state / "prepared").write_text("yes")
""",
        )
        self.canary = self._script(
            "canary.py",
            """
import json
import os
import urllib.request
from pathlib import Path
assert (Path(os.environ["SUBROUTER_SHADOW_STATE_DIR"]) / "prepared").read_text() == "yes"
with urllib.request.urlopen(os.environ["SUBROUTER_SHADOW_BASE_URL"] + "/_subrouter/ready") as response:
    assert json.load(response)["ok"] is True
Path(os.environ["TEST_CANARY_WITNESS"]).write_text("passed")
""",
        )

    def tearDown(self) -> None:
        self.temporary.cleanup()

    def _script(self, name: str, body: str) -> Path:
        path = self.root / name
        path.write_text(f"#!{sys.executable}\n{body.lstrip()}")
        path.chmod(0o700)
        return path

    def _run(
        self,
        port: int,
        *,
        candidate_hash: str | None = None,
        canary: Path | None = None,
    ) -> subprocess.CompletedProcess[str]:
        environment = dict(os.environ)
        environment["TEST_WORKSPACE_WITNESS"] = str(self.workspace_witness)
        environment["TEST_CANARY_WITNESS"] = str(self.canary_witness)
        return subprocess.run(
            [
                sys.executable,
                str(RUNNER),
                "--candidate",
                str(self.candidate),
                "--candidate-sha256",
                candidate_hash or hashlib.sha256(self.candidate.read_bytes()).hexdigest(),
                "--addr",
                f"127.0.0.1:{port}",
                "--prepare-callback",
                str(self.prepare),
                "--canary-callback",
                str(canary or self.canary),
                "--startup-timeout-seconds",
                "5",
                "--callback-timeout-seconds",
                "5",
            ],
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=environment,
            timeout=15,
        )

    def test_success_proves_process_listener_and_workspace_absence(self) -> None:
        port = _free_port()
        result = self._run(port)
        self.assertEqual(result.returncode, 0, result.stderr)
        evidence = json.loads(result.stdout)
        self.assertTrue(evidence["ok"])
        self.assertEqual(evidence["phases"], {"prepared": True, "healthy_ready": True, "canary": True})
        self.assertTrue(all(evidence["teardown"].values()))
        self.assertEqual(self.canary_witness.read_text(), "passed")
        workspace = Path(self.workspace_witness.read_text())
        self.assertFalse(workspace.exists())
        self.assertFalse(_listening(port))

    def test_failed_canary_still_proves_teardown(self) -> None:
        failing = self._script("failing.py", "raise SystemExit(7)\n")
        port = _free_port()
        result = self._run(port, canary=failing)
        self.assertEqual(result.returncode, 1)
        evidence = json.loads(result.stdout)
        self.assertFalse(evidence["ok"])
        self.assertEqual(evidence["failure"], "shadow callback failed")
        self.assertTrue(all(evidence["teardown"].values()))
        workspace = Path(self.workspace_witness.read_text())
        self.assertFalse(workspace.exists())
        self.assertFalse(_listening(port))

    def test_hash_mismatch_fails_before_prepare_or_listener(self) -> None:
        port = _free_port()
        result = self._run(port, candidate_hash="0" * 64)
        self.assertEqual(result.returncode, 1)
        evidence = json.loads(result.stdout)
        self.assertEqual(evidence["failure"], "candidate sha256 does not match")
        self.assertFalse(self.workspace_witness.exists())
        self.assertFalse(_listening(port))
        self.assertTrue(all(evidence["teardown"].values()))

    def test_sigterm_cleans_running_canary_and_candidate(self) -> None:
        sleeping = self._script(
            "sleeping.py",
            """
import os
import time
from pathlib import Path
Path(os.environ["TEST_CANARY_WITNESS"]).write_text("started")
time.sleep(60)
""",
        )
        port = _free_port()
        environment = dict(os.environ)
        environment["TEST_WORKSPACE_WITNESS"] = str(self.workspace_witness)
        environment["TEST_CANARY_WITNESS"] = str(self.canary_witness)
        process = subprocess.Popen(
            [
                sys.executable,
                str(RUNNER),
                "--candidate",
                str(self.candidate),
                "--candidate-sha256",
                hashlib.sha256(self.candidate.read_bytes()).hexdigest(),
                "--addr",
                f"127.0.0.1:{port}",
                "--prepare-callback",
                str(self.prepare),
                "--canary-callback",
                str(sleeping),
                "--startup-timeout-seconds",
                "5",
                "--callback-timeout-seconds",
                "120",
            ],
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=environment,
        )
        # Process startup can be delayed by concurrent cross-build and race
        # jobs on shared CI hosts; the production callback still has its own
        # explicit timeout. Wait long enough to observe that it actually began.
        deadline = time.monotonic() + 30
        while not self.canary_witness.exists() and time.monotonic() < deadline:
            time.sleep(0.05)
        self.assertTrue(self.canary_witness.exists())
        process.send_signal(signal.SIGTERM)
        stdout, stderr = process.communicate(timeout=30)
        self.assertEqual(process.returncode, 1, stderr)
        evidence = json.loads(stdout)
        self.assertFalse(evidence["ok"])
        self.assertTrue(all(evidence["teardown"].values()))
        workspace = Path(self.workspace_witness.read_text())
        self.assertFalse(workspace.exists())
        self.assertFalse(_listening(port))

    def test_help_is_self_describing(self) -> None:
        result = subprocess.run(
            [sys.executable, str(RUNNER), "--help"],
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
        )
        self.assertEqual(result.returncode, 0)
        self.assertIn("--prepare-callback", result.stdout)
        self.assertIn("--canary-callback", result.stdout)
        self.assertIn("prove teardown", result.stdout)


if __name__ == "__main__":
    unittest.main()
