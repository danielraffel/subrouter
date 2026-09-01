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
        self.challenge_witness = self.root / "challenges.txt"
        self.candidate_addr_witness = self.root / "candidate-addr.txt"
        self.candidate_sessions_witness = self.root / "candidate-sessions.txt"
        self.candidate_cloud_config_witness = self.root / "candidate-cloud-config.txt"
        self.candidate_codex_state_witness = self.root / "candidate-codex-state.txt"
        self.callback_addr_witness = self.root / "callback-addr.txt"
        self.test_home = self.root / "home"
        legacy_accounts = self.test_home / ".codex-accounts" / "accounts"
        legacy_accounts.mkdir(parents=True)
        self.legacy_witness = legacy_accounts / "must-not-be-read.json"
        self.legacy_witness.write_text("live-legacy-state")
        self.candidate = self._script(
            "candidate.py",
            """
import argparse
import hashlib
import hmac
import json
import os
import signal
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

parser = argparse.ArgumentParser()
parser.add_argument("command")
parser.add_argument("--addr", required=True)
parser.add_argument("--sessions", required=True)
args = parser.parse_args()
host, port = args.addr.rsplit(":", 1)
Path(os.environ["TEST_WORKSPACE_WITNESS"]).write_text(os.environ["SUBROUTER_SHADOW_WORKSPACE"])
Path(os.environ["TEST_CANDIDATE_ADDR_WITNESS"]).write_text(args.addr)
Path(os.environ["TEST_CANDIDATE_SESSIONS_WITNESS"]).write_text(args.sessions)
Path(os.environ["TEST_CANDIDATE_CLOUD_CONFIG_WITNESS"]).write_text(os.environ["SUBROUTER_CLOUD_CONFIG"])
codex_state = Path(os.environ["SUBROUTER_SHADOW_STATE_DIR"]) / "codex"
Path(os.environ["TEST_CANDIDATE_CODEX_STATE_WITNESS"]).write_text(",".join(sorted(path.name for path in codex_state.iterdir())))
Path(args.sessions).write_text("shadow-only")
shadow_key = bytes.fromhex(Path(os.environ["SUBROUTER_SHADOW_HEALTH_KEY_FILE"]).read_text().strip())

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        payload = {"ok": True, "draining": False}
        challenge_hex = self.headers.get("X-Subrouter-Shadow-Challenge")
        if self.path == "/_subrouter/health" and challenge_hex:
            challenge = bytes.fromhex(challenge_hex)
            with Path(os.environ["TEST_CHALLENGE_WITNESS"]).open("a") as witness:
                witness.write(challenge_hex + "\\n")
            payload["shadow_candidate_proof"] = hmac.new(
                shadow_key,
                b"subrouter-shadow-health-v1\\x00" + challenge,
                hashlib.sha256,
            ).hexdigest()
        body = json.dumps(payload).encode()
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
assert "SUBROUTER_SHADOW_HEALTH_KEY_FILE" not in os.environ
with urllib.request.urlopen(os.environ["SUBROUTER_SHADOW_BASE_URL"] + "/_subrouter/ready") as response:
    assert json.load(response)["ok"] is True
Path(os.environ["TEST_CANARY_WITNESS"]).write_text("passed")
Path(os.environ["TEST_CALLBACK_ADDR_WITNESS"]).write_text(os.environ["SUBROUTER_SHADOW_ADDR"])
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
        serve_args: Path | None = None,
        addr_host: str = "127.0.0.1",
    ) -> subprocess.CompletedProcess[str]:
        environment = dict(os.environ)
        environment["TEST_WORKSPACE_WITNESS"] = str(self.workspace_witness)
        environment["TEST_CANARY_WITNESS"] = str(self.canary_witness)
        environment["TEST_CHALLENGE_WITNESS"] = str(self.challenge_witness)
        environment["TEST_CANDIDATE_ADDR_WITNESS"] = str(self.candidate_addr_witness)
        environment["TEST_CANDIDATE_SESSIONS_WITNESS"] = str(self.candidate_sessions_witness)
        environment["TEST_CANDIDATE_CLOUD_CONFIG_WITNESS"] = str(self.candidate_cloud_config_witness)
        environment["TEST_CANDIDATE_CODEX_STATE_WITNESS"] = str(self.candidate_codex_state_witness)
        environment["TEST_CALLBACK_ADDR_WITNESS"] = str(self.callback_addr_witness)
        environment["HOME"] = str(self.test_home)
        environment["SUBROUTER_SHADOW_HEALTH_KEY_FILE"] = "must-not-reach-callback"
        command = [
            sys.executable,
            str(RUNNER),
            "--candidate",
            str(self.candidate),
            "--candidate-sha256",
            candidate_hash or hashlib.sha256(self.candidate.read_bytes()).hexdigest(),
            "--addr",
            f"{addr_host}:{port}",
            "--prepare-callback",
            str(self.prepare),
            "--canary-callback",
            str(canary or self.canary),
            "--startup-timeout-seconds",
            "5",
            "--callback-timeout-seconds",
            "5",
        ]
        if serve_args is not None:
            command.extend(["--serve-args-json", str(serve_args)])
        return subprocess.run(
            command,
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
        self.assertEqual(
            evidence["phases"],
            {
                "prepared": True,
                "healthy_ready": True,
                "canary": True,
                "post_canary_owned_ready": True,
            },
        )
        self.assertTrue(all(evidence["teardown"].values()))
        self.assertEqual(self.canary_witness.read_text(), "passed")
        challenges = self.challenge_witness.read_text().splitlines()
        self.assertEqual(len(challenges), 2)
        self.assertEqual(len(set(challenges)), 2)
        workspace = Path(self.workspace_witness.read_text())
        sessions = Path(self.candidate_sessions_witness.read_text())
        cloud_config = Path(self.candidate_cloud_config_witness.read_text())
        self.assertTrue(sessions.is_relative_to(workspace))
        self.assertTrue(cloud_config.is_relative_to(workspace))
        self.assertFalse(sessions.exists())
        self.assertFalse(cloud_config.exists())
        self.assertEqual(self.candidate_codex_state_witness.read_text(), "shadow-isolated-root")
        self.assertEqual(self.legacy_witness.read_text(), "live-legacy-state")
        self.assertFalse(workspace.exists())
        self.assertFalse(_listening(port))

    def test_resolved_numeric_address_is_reused_without_hostname_reresolution(self) -> None:
        port = _free_port()
        result = self._run(port, addr_host="localhost")
        self.assertEqual(result.returncode, 0, result.stderr)
        expected = f"127.0.0.1:{port}"
        self.assertEqual(self.candidate_addr_witness.read_text(), expected)
        self.assertEqual(self.callback_addr_witness.read_text(), expected)
        self.assertNotIn("localhost", result.stdout + result.stderr)

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

    def test_replacing_canary_path_after_pinning_cannot_change_executed_callback(self) -> None:
        prepare_entered = self.root / "prepare-entered"
        release_prepare = self.root / "release-prepare"
        replacement_witness = self.root / "replacement-canary.txt"
        blocking_prepare = self._script(
            "blocking-prepare.py",
            """
import os
import time
from pathlib import Path
state = Path(os.environ["SUBROUTER_SHADOW_STATE_DIR"])
(state / "prepared").write_text("yes")
Path(os.environ["TEST_PREPARE_ENTERED"]).write_text("yes")
while not Path(os.environ["TEST_RELEASE_PREPARE"]).exists():
    time.sleep(0.01)
""",
        )
        replacement_canary = self._script(
            "replacement-canary.py",
            """
import os
from pathlib import Path
Path(os.environ["TEST_REPLACEMENT_WITNESS"]).write_text("replacement-ran")
""",
        )
        port = _free_port()
        environment = dict(os.environ)
        environment.update(
            {
                "TEST_WORKSPACE_WITNESS": str(self.workspace_witness),
                "TEST_CANARY_WITNESS": str(self.canary_witness),
                "TEST_CHALLENGE_WITNESS": str(self.challenge_witness),
                "TEST_CANDIDATE_ADDR_WITNESS": str(self.candidate_addr_witness),
                "TEST_CANDIDATE_SESSIONS_WITNESS": str(self.candidate_sessions_witness),
                "TEST_CANDIDATE_CLOUD_CONFIG_WITNESS": str(self.candidate_cloud_config_witness),
                "TEST_CANDIDATE_CODEX_STATE_WITNESS": str(self.candidate_codex_state_witness),
                "TEST_CALLBACK_ADDR_WITNESS": str(self.callback_addr_witness),
                "TEST_PREPARE_ENTERED": str(prepare_entered),
                "TEST_RELEASE_PREPARE": str(release_prepare),
                "TEST_REPLACEMENT_WITNESS": str(replacement_witness),
            }
        )
        runner = subprocess.Popen(
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
                str(blocking_prepare),
                "--canary-callback",
                str(self.canary),
                "--startup-timeout-seconds",
                "30",
                "--callback-timeout-seconds",
                "30",
            ],
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=environment,
        )
        try:
            deadline = time.monotonic() + 30
            while not prepare_entered.exists() and time.monotonic() < deadline:
                time.sleep(0.01)
            self.assertTrue(prepare_entered.exists(), "runner never entered pinned prepare callback")
            os.replace(replacement_canary, self.canary)
            release_prepare.write_text("yes")
            stdout, stderr = runner.communicate(timeout=45)
        finally:
            if runner.poll() is None:
                runner.terminate()
                runner.communicate(timeout=5)

        self.assertEqual(runner.returncode, 0, stderr + stdout)
        evidence = json.loads(stdout)
        self.assertTrue(evidence["ok"])
        self.assertEqual(self.canary_witness.read_text(), "passed")
        self.assertFalse(replacement_witness.exists())
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

    def test_port_race_responder_without_candidate_key_cannot_pass(self) -> None:
        prepare_entered = self.root / "prepare-entered"
        release_prepare = self.root / "release-prepare"
        spoof_challenged = self.root / "spoof-challenged"
        blocking_prepare = self._script(
            "blocking-prepare.py",
            """
import os
import time
from pathlib import Path
Path(os.environ["TEST_WORKSPACE_WITNESS"]).write_text(os.environ["SUBROUTER_SHADOW_WORKSPACE"])
Path(os.environ["TEST_PREPARE_ENTERED"]).write_text("yes")
while not Path(os.environ["TEST_RELEASE_PREPARE"]).exists():
    time.sleep(0.01)
""",
        )
        sleeping_candidate = self._script(
            "sleeping-candidate.py",
            """
import time
time.sleep(60)
""",
        )
        spoof_server = self._script(
            "spoof-server.py",
            """
import json
import os
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        challenge = self.headers.get("X-Subrouter-Shadow-Challenge", "")
        if challenge:
            Path(os.environ["TEST_SPOOF_CHALLENGED"]).write_text(challenge)
        body = json.dumps({
            "ok": True,
            "draining": False,
            "shadow_candidate_proof": "0" * 64,
        }).encode()
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
        if challenge:
            threading.Thread(target=self.server.shutdown, daemon=True).start()
    def log_message(self, *_args):
        pass

server = ThreadingHTTPServer(("127.0.0.1", int(sys.argv[1])), Handler)
server.serve_forever()
server.server_close()
""",
        )

        port = _free_port()
        environment = dict(os.environ)
        environment.update(
            {
                "TEST_WORKSPACE_WITNESS": str(self.workspace_witness),
                "TEST_CANARY_WITNESS": str(self.canary_witness),
                "TEST_CHALLENGE_WITNESS": str(self.challenge_witness),
                "TEST_PREPARE_ENTERED": str(prepare_entered),
                "TEST_RELEASE_PREPARE": str(release_prepare),
                "TEST_SPOOF_CHALLENGED": str(spoof_challenged),
            }
        )
        runner = subprocess.Popen(
            [
                sys.executable,
                str(RUNNER),
                "--candidate",
                str(sleeping_candidate),
                "--candidate-sha256",
                hashlib.sha256(sleeping_candidate.read_bytes()).hexdigest(),
                "--addr",
                f"127.0.0.1:{port}",
                "--prepare-callback",
                str(blocking_prepare),
                "--canary-callback",
                str(self.canary),
                "--startup-timeout-seconds",
                "1",
                "--callback-timeout-seconds",
                "5",
            ],
            text=True,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=environment,
        )
        deadline = time.monotonic() + 5
        while not prepare_entered.exists() and time.monotonic() < deadline:
            time.sleep(0.01)
        self.assertTrue(prepare_entered.exists(), "runner never entered prepare callback")

        spoof = subprocess.Popen(
            [str(spoof_server), str(port)],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            env=environment,
        )
        try:
            deadline = time.monotonic() + 5
            while not _listening(port) and time.monotonic() < deadline:
                time.sleep(0.01)
            self.assertTrue(_listening(port), "spoof listener did not start")
            release_prepare.write_text("yes")
            stdout, stderr = runner.communicate(timeout=15)
            spoof.wait(timeout=5)
        finally:
            if spoof.poll() is None:
                spoof.terminate()
                spoof.wait(timeout=5)
            if runner.poll() is None:
                runner.terminate()
                runner.communicate(timeout=5)

        self.assertEqual(runner.returncode, 1, stderr)
        evidence = json.loads(stdout)
        self.assertEqual(evidence["failure"], "shadow candidate did not become healthy and ready")
        self.assertEqual(
            evidence["phases"],
            {
                "prepared": True,
                "healthy_ready": False,
                "canary": False,
                "post_canary_owned_ready": False,
            },
        )
        self.assertTrue(spoof_challenged.exists())
        self.assertFalse(self.canary_witness.exists())
        self.assertTrue(all(evidence["teardown"].values()))
        self.assertFalse(Path(self.workspace_witness.read_text()).exists())
        self.assertFalse(_listening(port))

    def _assert_signal_cleans_running_canary_and_candidate(self, sent_signal: signal.Signals) -> None:
        self.workspace_witness.unlink(missing_ok=True)
        self.canary_witness.unlink(missing_ok=True)
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
        environment["TEST_CHALLENGE_WITNESS"] = str(self.challenge_witness)
        environment["TEST_CANDIDATE_ADDR_WITNESS"] = str(self.candidate_addr_witness)
        environment["TEST_CANDIDATE_SESSIONS_WITNESS"] = str(self.candidate_sessions_witness)
        environment["TEST_CANDIDATE_CLOUD_CONFIG_WITNESS"] = str(self.candidate_cloud_config_witness)
        environment["TEST_CANDIDATE_CODEX_STATE_WITNESS"] = str(self.candidate_codex_state_witness)
        environment["TEST_CALLBACK_ADDR_WITNESS"] = str(self.callback_addr_witness)
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
        process.send_signal(sent_signal)
        stdout, stderr = process.communicate(timeout=30)
        self.assertEqual(process.returncode, 1, stderr)
        evidence = json.loads(stdout)
        self.assertFalse(evidence["ok"])
        self.assertTrue(all(evidence["teardown"].values()))
        workspace = Path(self.workspace_witness.read_text())
        self.assertFalse(workspace.exists())
        self.assertFalse(_listening(port))

    def test_handled_signals_clean_running_canary_and_candidate(self) -> None:
        handled = [signal.SIGINT, signal.SIGTERM]
        for signal_name in ("SIGHUP", "SIGQUIT"):
            optional_signal = getattr(signal, signal_name, None)
            if optional_signal is not None:
                handled.append(optional_signal)
        for sent_signal in handled:
            with self.subTest(signal=sent_signal.name):
                self._assert_signal_cleans_running_canary_and_candidate(sent_signal)

    def test_repeated_signal_cannot_interrupt_term_resistant_child_cleanup(self) -> None:
        resistant = self._script(
            "term-resistant.py",
            """
import os
import signal
import time
from pathlib import Path
signal.signal(signal.SIGTERM, signal.SIG_IGN)
Path(os.environ["TEST_CANARY_WITNESS"]).write_text("started")
time.sleep(60)
""",
        )
        port = _free_port()
        environment = dict(os.environ)
        environment.update(
            {
                "TEST_WORKSPACE_WITNESS": str(self.workspace_witness),
                "TEST_CANARY_WITNESS": str(self.canary_witness),
                "TEST_CHALLENGE_WITNESS": str(self.challenge_witness),
                "TEST_CANDIDATE_ADDR_WITNESS": str(self.candidate_addr_witness),
                "TEST_CANDIDATE_SESSIONS_WITNESS": str(self.candidate_sessions_witness),
                "TEST_CANDIDATE_CLOUD_CONFIG_WITNESS": str(self.candidate_cloud_config_witness),
                "TEST_CANDIDATE_CODEX_STATE_WITNESS": str(self.candidate_codex_state_witness),
                "TEST_CALLBACK_ADDR_WITNESS": str(self.callback_addr_witness),
            }
        )
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
                str(resistant),
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
        deadline = time.monotonic() + 30
        while not self.canary_witness.exists() and time.monotonic() < deadline:
            time.sleep(0.05)
        self.assertTrue(self.canary_witness.exists(), "runner never entered resistant callback")
        process.send_signal(signal.SIGTERM)
        time.sleep(0.1)
        process.send_signal(signal.SIGTERM)
        stdout, stderr = process.communicate(timeout=30)
        self.assertEqual(process.returncode, 1, stderr)
        evidence = json.loads(stdout)
        self.assertIn("interrupted by signal", evidence["failure"])
        self.assertTrue(all(evidence["teardown"].values()))
        self.assertFalse(Path(self.workspace_witness.read_text()).exists())
        self.assertFalse(_listening(port))

    def test_credential_bearing_serve_arguments_are_rejected_before_prepare(self) -> None:
        options = (
            "--admin-token",
            "--account-import-token",
            "--stack-publishable-client-key",
            "--stack-tenant-key-secret",
            "--stack-tenant-delete-token",
            "--bedrock-gateway-token",
        )
        for option in options:
            spellings = (option, "-" + option.removeprefix("--"))
            for spelling in spellings:
                for arguments in ([spelling, "must-not-appear"], [spelling + "=must-not-appear"]):
                    with self.subTest(arguments=arguments):
                        serve_args = self.root / "serve-args.json"
                        serve_args.write_text(json.dumps(arguments))
                        port = _free_port()
                        result = self._run(port, serve_args=serve_args)
                        self.assertEqual(result.returncode, 1)
                        evidence = json.loads(result.stdout)
                        self.assertEqual(
                            evidence["failure"].split(";", 1)[0],
                            f"serve args JSON must not contain {option}",
                        )
                        self.assertNotIn("must-not-appear", result.stdout + result.stderr)
                        self.assertFalse(self.workspace_witness.exists())
                        self.assertFalse(_listening(port))

    def test_persistent_state_serve_arguments_are_rejected_before_prepare(self) -> None:
        options = (
            "--sessions",
            "--transcripts",
            "--cloud-config",
            "--transcript-gcs-uri",
            "--transcript-azure-url",
        )
        for option in options:
            spellings = (option, "-" + option.removeprefix("--"))
            for spelling in spellings:
                for arguments in ([spelling, "must-not-escape"], [spelling + "=must-not-escape"]):
                    with self.subTest(arguments=arguments):
                        serve_args = self.root / "serve-args.json"
                        serve_args.write_text(json.dumps(arguments))
                        result = self._run(_free_port(), serve_args=serve_args)
                        self.assertEqual(result.returncode, 1)
                        evidence = json.loads(result.stdout)
                        self.assertEqual(
                            evidence["failure"],
                            f"serve args JSON must not contain {option}; shadow state, logs, and configuration must remain disposable",
                        )
                        self.assertNotIn("must-not-escape", result.stdout + result.stderr)
                        self.assertFalse(self.workspace_witness.exists())

    def test_external_mutation_serve_arguments_are_rejected_before_prepare(self) -> None:
        for spelling in (
            "--bedrock-autobump",
            "-bedrock-autobump",
            "--bedrock-autobump=true",
            "-bedrock-autobump=true",
        ):
            with self.subTest(spelling=spelling):
                serve_args = self.root / "serve-args.json"
                serve_args.write_text(json.dumps([spelling]))
                result = self._run(_free_port(), serve_args=serve_args)
                self.assertEqual(result.returncode, 1)
                evidence = json.loads(result.stdout)
                self.assertEqual(
                    evidence["failure"],
                    "serve args JSON must not contain --bedrock-autobump; shadow rehearsals must not trigger external mutations",
                )
                self.assertFalse(self.workspace_witness.exists())

    def test_single_or_double_dash_address_override_is_rejected_before_prepare(self) -> None:
        for arguments in (["--addr", "0.0.0.0:1"], ["--addr=0.0.0.0:1"], ["-addr", "0.0.0.0:1"], ["-addr=0.0.0.0:1"]):
            with self.subTest(arguments=arguments):
                serve_args = self.root / "serve-args.json"
                serve_args.write_text(json.dumps(arguments))
                result = self._run(_free_port(), serve_args=serve_args)
                self.assertEqual(result.returncode, 1)
                evidence = json.loads(result.stdout)
                self.assertEqual(evidence["failure"], "serve args JSON must not override serve or --addr")
                self.assertFalse(self.workspace_witness.exists())

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
        self.assertIn("non-credential, non-persistent", result.stdout)
        self.assertIn("serve arguments", result.stdout)
        self.assertIn("prove teardown", result.stdout)


if __name__ == "__main__":
    unittest.main()
