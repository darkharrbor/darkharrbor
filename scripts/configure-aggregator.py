#!/usr/bin/env python3
"""Guarded AIOStreams user-config snapshot and apply helper.

The API accepts a complete user configuration, so this helper deliberately does
not invent partial-merge semantics.  It writes only when the live configuration
still matches an operator-captured baseline, then verifies the saved result.
"""

from __future__ import annotations

import argparse
import base64
import copy
import json
import os
import socket
import stat
import sys
from pathlib import Path
from urllib import error, parse, request

API_MANAGED_FIELDS = frozenset(
    {"accessKey", "encryptedPassword", "ip", "trusted", "uuid"}
)
MAX_RESPONSE_BYTES = 8 * 1024 * 1024
REQUEST_TIMEOUT_SECONDS = 120

MANUAL_STEPS = """Manual steps NOT performed by this optional helper:
  1. Set and verify the aggregator/shared-tier compose environment from this guide.
  2. Add Usenet providers in the shared-tier dashboard, restart it after server
     changes, and confirm the NNTP pool reports a non-zero server count.
  3. Run darkharrbor doctor, inspect a real stream lookup, and prove playback
     coverage/commit. Saved configuration alone is not live evidence.
"""


class SafeError(Exception):
    """An error whose message contains no credential or config value."""


class Refused(SafeError):
    """A safe compare-before-write refusal."""


class NoRedirect(request.HTTPRedirectHandler):
    """Treat redirects as errors so Basic credentials never follow them."""

    def redirect_request(self, *_args: object, **_kwargs: object) -> None:
        return None


def private_json(path: Path, label: str) -> dict:
    flags = os.O_RDONLY | getattr(os, "O_NOFOLLOW", 0)
    fd = -1
    try:
        fd = os.open(path, flags)
        metadata = os.fstat(fd)
        if not stat.S_ISREG(metadata.st_mode):
            raise SafeError(f"{label} must be a regular file")
        if stat.S_IMODE(metadata.st_mode) & 0o077:
            raise SafeError(f"{label} must be mode 0600 (no group/other access)")
        with os.fdopen(fd, "r", encoding="utf-8") as handle:
            fd = -1
            value = json.load(handle)
    except SafeError:
        raise
    except (OSError, UnicodeError, json.JSONDecodeError):
        raise SafeError(f"{label} could not be read as private JSON") from None
    finally:
        if fd >= 0:
            os.close(fd)

    if not isinstance(value, dict):
        raise SafeError(f"{label} must contain one JSON object")
    return value


def write_private_json(path: Path, value: dict) -> None:
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0)
    fd = -1
    created = False
    try:
        fd = os.open(path, flags, 0o600)
        created = True
        os.fchmod(fd, 0o600)
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            fd = -1
            json.dump(value, handle, indent=2, sort_keys=True, ensure_ascii=False)
            handle.write("\n")
            handle.flush()
            os.fsync(handle.fileno())
    except FileExistsError:
        raise SafeError("snapshot output already exists; choose a new file") from None
    except OSError:
        if created:
            try:
                os.unlink(path)
            except OSError:
                pass
        raise SafeError("snapshot output could not be written") from None
    finally:
        if fd >= 0:
            os.close(fd)


def normalized_config(value: dict) -> dict:
    return {key: item for key, item in value.items() if key not in API_MANAGED_FIELDS}


MISSING = object()


def json_pointer(raw: str) -> tuple[str, ...]:
    if not raw.startswith("/") or raw == "/":
        raise SafeError("server-normalized paths must be non-root JSON pointers")
    return tuple(part.replace("~1", "/").replace("~0", "~") for part in raw[1:].split("/"))


def pointer_value(value: object, pointer: tuple[str, ...]) -> object:
    current = value
    for part in pointer:
        if isinstance(current, dict) and part in current:
            current = current[part]
        elif isinstance(current, list) and part.isdigit() and int(part) < len(current):
            current = current[int(part)]
        else:
            return MISSING
    return current


def comparison_view(value: dict, pointers: tuple[tuple[str, ...], ...]) -> dict:
    result = copy.deepcopy(value)
    for pointer in pointers:
        current = result
        for part in pointer[:-1]:
            if isinstance(current, dict) and part in current:
                current = current[part]
            elif (
                isinstance(current, list)
                and part.isdigit()
                and int(part) < len(current)
            ):
                current = current[int(part)]
            else:
                current = MISSING
                break
        if current is MISSING:
            continue
        final = pointer[-1]
        marker = {"__darkharrbor_server_normalized__": True}
        if isinstance(current, dict) and final in current:
            current[final] = marker
        elif (
            isinstance(current, list)
            and final.isdigit()
            and int(final) < len(current)
        ):
            current[int(final)] = marker
    return result


def accepted_server_normalized_paths(
    baseline: dict, desired: dict, raw_paths: list[str]
) -> tuple[tuple[str, ...], ...]:
    pointers = tuple(dict.fromkeys(json_pointer(raw) for raw in raw_paths))
    for pointer in pointers:
        before = pointer_value(baseline, pointer)
        after = pointer_value(desired, pointer)
        if before is MISSING or after is MISSING:
            raise SafeError(
                "each server-normalized path must exist in baseline and desired"
            )
        if before != after:
            raise SafeError(
                "a server-normalized path differs between baseline and desired; "
                "it cannot be ignored"
            )
    return pointers


def load_credentials(path: Path) -> tuple[str, str, str]:
    value = private_json(path, "credentials file")
    required = {"base_url", "uuid", "password"}
    if set(value) != required:
        raise SafeError(
            "credentials file must contain exactly base_url, uuid, and password"
        )

    base_url = value["base_url"]
    uuid = value["uuid"]
    password = value["password"]
    if not all(isinstance(item, str) and item for item in (base_url, uuid, password)):
        raise SafeError("credentials fields must be non-empty strings")
    if ":" in uuid:
        raise SafeError("credentials uuid cannot contain ':'")

    parts = parse.urlsplit(base_url)
    if (
        parts.scheme not in {"http", "https"}
        or not parts.netloc
        or parts.hostname is None
        or parts.username is not None
        or parts.password is not None
        or parts.query
        or parts.fragment
    ):
        raise SafeError(
            "credentials base_url must be an http(s) URL without userinfo, query, or fragment"
        )
    return base_url.rstrip("/"), uuid, password


class UserConfigAPI:
    def __init__(self, base_url: str, uuid: str, password: str) -> None:
        self.endpoint = base_url + "/api/v1/user"
        token = base64.b64encode(f"{uuid}:{password}".encode("utf-8")).decode("ascii")
        self.headers = {
            "Accept": "application/json",
            "Authorization": "Basic " + token,
        }
        # This is a credential-bearing local control-plane call; never inherit
        # HTTP(S)_PROXY from the operator shell.
        self.opener = request.build_opener(request.ProxyHandler({}), NoRedirect)

    def call(
        self, method: str, payload: dict | None = None, *, raw: bool = False
    ) -> dict:
        body = None
        headers = dict(self.headers)
        if payload is not None:
            body = json.dumps(payload, separators=(",", ":")).encode("utf-8")
            headers["Content-Type"] = "application/json"
        endpoint = self.endpoint + ("?raw=true" if raw else "")
        req = request.Request(endpoint, data=body, headers=headers, method=method)

        try:
            with self.opener.open(req, timeout=REQUEST_TIMEOUT_SECONDS) as response:
                response_body = response.read(MAX_RESPONSE_BYTES + 1)
        except error.HTTPError as exc:
            exc.close()
            raise SafeError(
                f"aggregator {method} failed with HTTP {exc.code}; response body withheld"
            ) from None
        except (error.URLError, TimeoutError, socket.timeout):
            raise SafeError(
                f"aggregator {method} failed at the transport layer; details withheld"
            ) from None

        if len(response_body) > MAX_RESPONSE_BYTES:
            raise SafeError(f"aggregator {method} response exceeded the safe size limit")
        try:
            decoded = json.loads(response_body)
        except (UnicodeError, json.JSONDecodeError):
            raise SafeError(f"aggregator {method} returned invalid JSON") from None
        if not isinstance(decoded, dict) or decoded.get("success") is not True:
            raise SafeError(
                f"aggregator {method} returned an unsuccessful response; body withheld"
            )
        return decoded

    def get(self) -> dict:
        # raw=true returns the saved child config instead of a parent-merged view.
        decoded = self.call("GET", raw=True)
        data = decoded.get("data")
        config = data.get("userData") if isinstance(data, dict) else None
        if not isinstance(config, dict):
            raise SafeError("aggregator GET response did not contain a user config")
        return normalized_config(config)

    def put(self, config: dict) -> None:
        self.call("PUT", {"config": config})


def print_manual_steps() -> None:
    print(MANUAL_STEPS, end="")


def snapshot(args: argparse.Namespace) -> int:
    base_url, uuid, password = load_credentials(Path(args.credentials))
    config = UserConfigAPI(base_url, uuid, password).get()
    write_private_json(Path(args.output), config)
    print("Saved a private baseline snapshot (mode 0600); no config was changed.")
    return 0


def apply(args: argparse.Namespace) -> int:
    base_url, uuid, password = load_credentials(Path(args.credentials))
    baseline = normalized_config(private_json(Path(args.baseline), "baseline file"))
    desired = normalized_config(private_json(Path(args.desired), "desired file"))
    pointers = accepted_server_normalized_paths(
        baseline, desired, args.server_normalized_path
    )
    baseline_view = comparison_view(baseline, pointers)
    desired_view = comparison_view(desired, pointers)
    api = UserConfigAPI(base_url, uuid, password)

    live = api.get()
    if comparison_view(live, pointers) == desired_view:
        print("No change: the live aggregator config already matches desired.")
        print_manual_steps()
        return 0
    if comparison_view(live, pointers) != baseline_view:
        raise Refused(
            "REFUSED: live aggregator config differs from the supplied baseline; "
            "no write was sent"
        )

    # Narrow the API's lack of an ETag/conditional update window. This cannot
    # make PUT atomic, so operators must still avoid concurrent dashboard saves.
    if comparison_view(api.get(), pointers) != baseline_view:
        raise Refused(
            "REFUSED: live aggregator config changed during the pre-write check; "
            "no write was sent"
        )

    api.put(desired)
    if comparison_view(api.get(), pointers) != desired_view:
        raise SafeError(
            "aggregator PUT returned success but read-back differs; values withheld"
        )

    print("Applied and verified the desired aggregator config.")
    print_manual_steps()
    return 0


def parser() -> argparse.ArgumentParser:
    result = argparse.ArgumentParser(
        description="Guarded AIOStreams user-config snapshot/apply helper"
    )
    commands = result.add_subparsers(dest="command", required=True)

    take = commands.add_parser("snapshot", help="save the current config privately")
    take.add_argument("--credentials", required=True)
    take.add_argument("--output", required=True)
    take.set_defaults(run=snapshot)

    put = commands.add_parser("apply", help="compare, apply, and verify a config")
    put.add_argument("--credentials", required=True)
    put.add_argument("--baseline", required=True)
    put.add_argument("--desired", required=True)
    put.add_argument(
        "--server-normalized-path",
        action="append",
        default=[],
        help=(
            "JSON pointer whose unchanged value the server rewrites; repeat as "
            "needed. Refused if baseline and desired differ at that path."
        ),
    )
    put.set_defaults(run=apply)
    return result


def main(argv: list[str] | None = None) -> int:
    try:
        args = parser().parse_args(argv)
        return args.run(args)
    except Refused as exc:
        print(str(exc), file=sys.stderr)
        return 3
    except SafeError as exc:
        print(f"ERROR: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
