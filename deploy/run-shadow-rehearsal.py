#!/usr/bin/env python3
"""Run an optional disposable Subrouter shadow without touching the live listener."""

from __future__ import annotations

import argparse
import hashlib
import hmac
import json
import os
import secrets
import shutil
import signal
import socket
import stat
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path


SCHEMA = "subrouter.shadow-rehearsal-evidence/v1"
SHADOW_CHALLENGE_HEADER = "X-Subrouter-Shadow-Challenge"
SHADOW_PROOF_FIELD = "shadow_candidate_proof"
SHADOW_HEALTH_DOMAIN = b"subrouter-shadow-health-v1\x00"

CREDENTIAL_SERVE_OPTIONS = {
    "admin-token": "SUBROUTER_ADMIN_TOKEN_FILE",
    "account-import-token": "SUBROUTER_ACCOUNT_IMPORT_TOKEN_FILE",
    "stack-publishable-client-key": "SUBROUTER_STACK_PUBLISHABLE_CLIENT_KEY",
    "stack-tenant-key-secret": "SUBROUTER_STACK_TENANT_KEY_SECRET_FILE",
    "stack-tenant-delete-token": "SUBROUTER_STACK_TENANT_DELETE_TOKEN_FILE",
    "bedrock-gateway-token": "SUBROUTER_BEDROCK_GATEWAY_TOKEN",
}


class ShadowError(Exception):
    pass


def _fail(message: str) -> None:
    raise ShadowError(message)


def _valid_sha256(value: str) -> bool:
    return len(value) == 64 and all(char in "0123456789abcdef" for char in value)


def _secure_file(raw_path: str, description: str, *, executable: bool = False) -> Path:
    if not raw_path or not os.path.isabs(raw_path):
        _fail(f"{description} must be an absolute path")
    path = Path(raw_path)
    try:
        info = path.lstat()
    except OSError:
        _fail(f"{description} is unavailable")
    if not stat.S_ISREG(info.st_mode) or stat.S_ISLNK(info.st_mode):
        _fail(f"{description} must be a regular non-symlink")
    if executable and not os.access(path, os.X_OK):
        _fail(f"{description} must be executable")
    return path


def _copy_candidate(source: Path, destination: Path, expected_hash: str) -> None:
    flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)
    source_fd = os.open(source, flags)
    output_fd = -1
    digest = hashlib.sha256()
    try:
        opened = os.fstat(source_fd)
        named = source.lstat()
        if (
            not stat.S_ISREG(opened.st_mode)
            or stat.S_ISLNK(named.st_mode)
            or (opened.st_dev, opened.st_ino) != (named.st_dev, named.st_ino)
        ):
            _fail("candidate identity changed while opening")
        output_fd = os.open(destination, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o700)
        while True:
            chunk = os.read(source_fd, 1024 * 1024)
            if not chunk:
                break
            digest.update(chunk)
            view = memoryview(chunk)
            while view:
                view = view[os.write(output_fd, view) :]
        os.fsync(output_fd)
    finally:
        os.close(source_fd)
        if output_fd >= 0:
            os.close(output_fd)
    if digest.hexdigest() != expected_hash:
        _fail("candidate sha256 does not match")


def _parse_loopback_addr(value: str) -> tuple[str, int, str]:
    try:
        parsed = urllib.parse.urlsplit(f"//{value}")
        host = parsed.hostname
        port = parsed.port
    except ValueError:
        _fail("shadow address must be loopback HOST:PORT")
    if host is None or port is None or not 1 <= port <= 65535:
        _fail("shadow address must be loopback HOST:PORT")
    try:
        address = socket.gethostbyname(host)
    except OSError:
        _fail("shadow address host is unavailable")
    if not address.startswith("127."):
        _fail("shadow address must use IPv4 loopback")
    return address, port, f"http://{address}:{port}"


def _listener_present(host: str, port: int) -> bool:
    try:
        with socket.create_connection((host, port), timeout=0.2):
            return True
    except OSError:
        return False


def _wait_listener_absent(host: str, port: int, timeout: float = 5.0) -> bool:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if not _listener_present(host, port):
            return True
        time.sleep(0.05)
    return not _listener_present(host, port)


def _group_exists(pgid: int) -> bool:
    try:
        os.killpg(pgid, 0)
        return True
    except ProcessLookupError:
        return False
    except PermissionError:
        return True


def _terminate_group(process: subprocess.Popen[bytes], timeout: float = 5.0) -> bool:
    pgid = process.pid
    if _group_exists(pgid):
        try:
            os.killpg(pgid, signal.SIGTERM)
        except (ProcessLookupError, PermissionError):
            pass
    try:
        process.wait(timeout=timeout)
    except subprocess.TimeoutExpired:
        try:
            os.killpg(pgid, signal.SIGKILL)
        except (ProcessLookupError, PermissionError):
            pass
        try:
            process.wait(timeout=1)
        except subprocess.TimeoutExpired:
            return False
    deadline = time.monotonic() + 1
    while _group_exists(pgid) and time.monotonic() < deadline:
        try:
            os.killpg(pgid, signal.SIGKILL)
        except (ProcessLookupError, PermissionError):
            pass
        time.sleep(0.05)
    return not _group_exists(pgid)


def _callback_environment(
    workspace: Path,
    state_dir: Path,
    candidate: Path,
    candidate_sha256: str,
    addr: str,
    base_url: str,
    candidate_log: Path,
) -> dict[str, str]:
    environment = dict(os.environ)
    environment.pop("SUBROUTER_SHADOW_HEALTH_KEY_FILE", None)
    environment.update(
        {
            "SUBROUTER_STATE_DIR": str(state_dir),
            "SUBROUTER_SHADOW_WORKSPACE": str(workspace),
            "SUBROUTER_SHADOW_STATE_DIR": str(state_dir),
            "SUBROUTER_SHADOW_CANDIDATE_PATH": str(candidate),
            "SUBROUTER_SHADOW_CANDIDATE_SHA256": candidate_sha256,
            "SUBROUTER_SHADOW_ADDR": addr,
            "SUBROUTER_SHADOW_BASE_URL": base_url,
            "SUBROUTER_SHADOW_LOG_FILE": str(candidate_log),
        }
    )
    return environment


def _run_callback(path: Path, environment: dict[str, str], log_path: Path, timeout: int) -> None:
    with log_path.open("wb") as output:
        process = subprocess.Popen(
            [str(path)],
            stdin=subprocess.DEVNULL,
            stdout=output,
            stderr=subprocess.STDOUT,
            env=environment,
            start_new_session=True,
        )
        try:
            try:
                return_code = process.wait(timeout=timeout)
            except subprocess.TimeoutExpired:
                _terminate_group(process)
                _fail("shadow callback timed out")
            if return_code != 0:
                _fail("shadow callback failed")
            if _group_exists(process.pid):
                _terminate_group(process)
                _fail("shadow callback left descendant processes")
        except BaseException:
            _terminate_group(process)
            raise


def _load_serve_args(raw_path: str | None) -> list[str]:
    if raw_path is None:
        return []
    path = _secure_file(raw_path, "serve args JSON")
    try:
        parsed = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, UnicodeError, json.JSONDecodeError):
        _fail("serve args JSON is invalid")
    if not isinstance(parsed, list) or not all(isinstance(item, str) for item in parsed):
        _fail("serve args JSON must be an array of strings")
    for argument in parsed:
        if (
            "\x00" in argument
            or argument == "serve"
            or argument in ("--addr", "-addr")
            or argument.startswith("--addr=")
            or argument.startswith("-addr=")
        ):
            _fail("serve args JSON must not override serve or --addr")
        for option_name, environment_name in CREDENTIAL_SERVE_OPTIONS.items():
            option = "--" + option_name
            short_option = "-" + option_name
            if (
                argument in (option, short_option)
                or argument.startswith(option + "=")
                or argument.startswith(short_option + "=")
            ):
                _fail(
                    f"serve args JSON must not contain {option}; "
                    f"use {environment_name} in the helper environment"
                )
    return parsed


def _probe(base_url: str, path: str) -> bool:
    try:
        with urllib.request.urlopen(base_url + path, timeout=0.5) as response:
            if response.status != 200:
                return False
            body = response.read(1024)
        parsed = json.loads(body)
        return parsed.get("ok") is True and (path != "/_subrouter/ready" or parsed.get("draining") is not True)
    except (OSError, urllib.error.URLError, json.JSONDecodeError, AttributeError):
        return False


def _owned_health(base_url: str, key: bytes) -> bool:
    challenge = secrets.token_bytes(32)
    request = urllib.request.Request(
        base_url + "/_subrouter/health",
        headers={SHADOW_CHALLENGE_HEADER: challenge.hex()},
    )
    try:
        with urllib.request.urlopen(request, timeout=0.5) as response:
            if response.status != 200:
                return False
            body = response.read(4096)
        parsed = json.loads(body)
        proof = parsed.get(SHADOW_PROOF_FIELD)
        expected = hmac.new(key, SHADOW_HEALTH_DOMAIN + challenge, hashlib.sha256).hexdigest()
        return parsed.get("ok") is True and isinstance(proof, str) and hmac.compare_digest(proof, expected)
    except (OSError, urllib.error.URLError, json.JSONDecodeError, AttributeError):
        return False


def _wait_ready(process: subprocess.Popen[bytes], base_url: str, key: bytes, timeout: int) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if process.poll() is not None:
            _fail("shadow candidate exited before readiness")
        if _owned_health(base_url, key) and _probe(base_url, "/_subrouter/ready"):
            return
        time.sleep(0.05)
    _fail("shadow candidate did not become healthy and ready")


def _write_shadow_health_key(path: Path, key: bytes) -> None:
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    try:
        body = key.hex().encode("ascii")
        view = memoryview(body)
        while view:
            view = view[os.write(descriptor, view) :]
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def _ignore_signals(handled_signals: list[signal.Signals]) -> None:
    for handled_signal in handled_signals:
        signal.signal(handled_signal, signal.SIG_IGN)


def _evidence(ok: bool, candidate_sha256: str, phases: dict[str, bool], teardown: dict[str, bool], failure: str = "") -> str:
    body: dict[str, object] = {
        "schema": SCHEMA,
        "ok": ok,
        "candidate_sha256": candidate_sha256,
        "phases": phases,
        "teardown": teardown,
    }
    if failure:
        body["failure"] = failure
    return json.dumps(body, sort_keys=True, separators=(",", ":"))


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Run an optional hash-pinned Subrouter shadow and prove teardown"
    )
    parser.add_argument("--candidate", required=True, help="absolute candidate executable path")
    parser.add_argument("--candidate-sha256", required=True, help="expected lowercase SHA-256")
    parser.add_argument("--addr", required=True, help="unused IPv4 loopback HOST:PORT")
    parser.add_argument("--prepare-callback", required=True, help="absolute executable run before shadow start")
    parser.add_argument("--canary-callback", required=True, help="absolute executable run after readiness")
    parser.add_argument(
        "--serve-args-json",
        help="optional JSON array of non-credential serve arguments",
    )
    parser.add_argument("--startup-timeout-seconds", type=int, default=30)
    parser.add_argument("--callback-timeout-seconds", type=int, default=120)
    arguments = parser.parse_args()

    candidate_sha256 = arguments.candidate_sha256
    phases = {
        "prepared": False,
        "healthy_ready": False,
        "canary": False,
        "post_canary_owned_ready": False,
    }
    teardown = {
        "process_group_absent": True,
        "listener_absent": True,
        "workspace_absent": True,
        "state_absent": True,
        "logs_absent": True,
    }
    failure = ""
    workspace: Path | None = None
    state_dir: Path | None = None
    candidate_log: Path | None = None
    candidate_process: subprocess.Popen[bytes] | None = None
    host = "127.0.0.1"
    port = 0

    handled_signals = [signal.SIGINT, signal.SIGTERM]
    for signal_name in ("SIGHUP", "SIGQUIT"):
        optional_signal = getattr(signal, signal_name, None)
        if optional_signal is not None and optional_signal not in handled_signals:
            handled_signals.append(optional_signal)
    def interrupt_handler(signum: int, _frame: object) -> None:
        # A second signal must not interrupt callback or candidate cleanup.
        # Mask the signal being handled first to close the repeat-signal race.
        signal.signal(signum, signal.SIG_IGN)
        _ignore_signals(handled_signals)
        raise ShadowError(f"shadow rehearsal interrupted by signal {signum}")

    prior_handlers = {
        sent_signal: signal.signal(sent_signal, interrupt_handler)
        for sent_signal in handled_signals
    }
    try:
        if not _valid_sha256(candidate_sha256):
            _fail("candidate sha256 is invalid")
        if arguments.startup_timeout_seconds < 1 or arguments.startup_timeout_seconds > 300:
            _fail("startup timeout must be 1..300 seconds")
        if arguments.callback_timeout_seconds < 1 or arguments.callback_timeout_seconds > 600:
            _fail("callback timeout must be 1..600 seconds")
        candidate = _secure_file(arguments.candidate, "candidate", executable=True)
        prepare = _secure_file(arguments.prepare_callback, "prepare callback", executable=True)
        canary = _secure_file(arguments.canary_callback, "canary callback", executable=True)
        serve_args = _load_serve_args(arguments.serve_args_json)
        host, port, base_url = _parse_loopback_addr(arguments.addr)
        resolved_addr = f"{host}:{port}"
        if _listener_present(host, port):
            _fail("shadow listener is already in use")

        workspace = Path(tempfile.mkdtemp(prefix="subrouter-shadow-"))
        workspace.chmod(0o700)
        state_dir = workspace / "state"
        state_dir.mkdir(mode=0o700)
        prepare_candidate = workspace / "candidate-prepare"
        candidate_log = workspace / "candidate.log"
        _copy_candidate(candidate, prepare_candidate, candidate_sha256)
        environment = _callback_environment(
            workspace, state_dir, prepare_candidate, candidate_sha256, resolved_addr, base_url, candidate_log
        )
        _run_callback(
            prepare, environment, workspace / "prepare.log", arguments.callback_timeout_seconds
        )
        phases["prepared"] = True

        # Pin a fresh server copy after preparation. A vendor account command
        # may update its own executable; those bytes must never become the
        # shadow candidate without matching the reviewed digest again.
        pinned_candidate = workspace / "candidate-serve"
        _copy_candidate(candidate, pinned_candidate, candidate_sha256)
        environment["SUBROUTER_SHADOW_CANDIDATE_PATH"] = str(pinned_candidate)
        shadow_health_key = secrets.token_bytes(32)
        shadow_health_key_file = workspace / "shadow-health-key"
        _write_shadow_health_key(shadow_health_key_file, shadow_health_key)
        candidate_environment = dict(environment)
        candidate_environment["SUBROUTER_SHADOW_HEALTH_KEY_FILE"] = str(shadow_health_key_file)
        with candidate_log.open("wb") as output:
            candidate_process = subprocess.Popen(
                [str(pinned_candidate), "serve", "--addr", resolved_addr, *serve_args],
                stdin=subprocess.DEVNULL,
                stdout=output,
                stderr=subprocess.STDOUT,
                env=candidate_environment,
                start_new_session=True,
            )
            _wait_ready(
                candidate_process,
                base_url,
                shadow_health_key,
                arguments.startup_timeout_seconds,
            )
            phases["healthy_ready"] = True
            shadow_health_key_file.unlink()
            _run_callback(
                canary, environment, workspace / "canary.log", arguments.callback_timeout_seconds
            )
            phases["canary"] = True
            if candidate_process.poll() is not None:
                _fail("shadow candidate exited during canary")
            if not _owned_health(base_url, shadow_health_key) or not _probe(
                base_url, "/_subrouter/ready"
            ):
                _fail("shadow candidate lost ownership or readiness after canary")
            phases["post_canary_owned_ready"] = True
    except ShadowError as error:
        failure = str(error)
    except Exception:
        failure = "internal shadow rehearsal failure"
    finally:
        _ignore_signals(handled_signals)
        if candidate_process is not None:
            teardown["process_group_absent"] = _terminate_group(candidate_process)
        if port:
            teardown["listener_absent"] = _wait_listener_absent(host, port)
        if workspace is not None:
            try:
                shutil.rmtree(workspace)
            except OSError:
                pass
            teardown["workspace_absent"] = not workspace.exists()
            teardown["state_absent"] = state_dir is not None and not state_dir.exists()
            teardown["logs_absent"] = candidate_log is not None and not candidate_log.exists()
        for sent_signal, handler in prior_handlers.items():
            signal.signal(sent_signal, handler)

    ok = not failure and all(phases.values()) and all(teardown.values())
    if not ok and not failure:
        failure = "shadow teardown proof failed"
    print(_evidence(ok, candidate_sha256, phases, teardown, failure))
    return 0 if ok else 1


if __name__ == "__main__":
    raise SystemExit(main())
