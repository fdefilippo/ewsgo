#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
# SPDX-License-Identifier: AGPL-3.0-or-later

"""Headless MailCraft EWS qualification with sanitized reporting."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import stat
import subprocess
import tempfile
import urllib.parse

from build import REVISION


def cli(binary, commands, store=None):
    args = [binary, "run"]
    if store and store.exists():
        args += ["--file", str(store)]
    for command, value in commands:
        args += ["--cmd", command + "=" + json.dumps(value)]
    if store:
        args += ["--save", str(store)]
    output = subprocess.run(args + ["--print"], capture_output=True, timeout=60)
    if output.returncode:
        raise ValueError("CLI command failed; private output suppressed")
    text, decoder, results = output.stdout.decode(), json.JSONDecoder(), []
    while text.strip():
        item, end = decoder.raw_decode(text.lstrip())
        results.append(item["result"])
        text = text.lstrip()[end:]
    return results


def credentials(path):
    st = path.stat()
    if not stat.S_ISREG(st.st_mode) or st.st_mode & 0o077:
        raise ValueError("credentials require an owner-only regular file")
    values = {}
    for line in path.read_text().splitlines():
        if line.strip() and not line.lstrip().startswith("#"):
            key, value = line.split(":", 1)
            key = key.strip()
            if key in values or key not in ("url", "user", "pass", "denied_mailbox"):
                raise ValueError("invalid credentials format")
            values[key] = value.strip()
    return values


def runner(binary, request, password, ca="", mode="sync"):
    private = {"request": request, "password": password, "ca": ca, "mode": mode,
               "transport": str(Path(__file__).with_name("transport.py").resolve())}
    result = subprocess.run([binary], input=json.dumps(private), text=True, capture_output=True, timeout=180)
    value = json.loads(result.stdout) if result.stdout else {"ok": False, "networkRequests": 0, "results": []}
    value["exitCode"] = result.returncode
    return value


def apply_results(binary, host, store, pieces, command="sync.applyEws"):
    if not pieces:
        return 0
    # Linux limits each argv string to 32 pages even if the total argument
    # vector fits. Account for the command name, separator, and NUL.
    limit = os.sysconf("SC_PAGESIZE") * 32 - len(command) - 2
    if all(len(json.dumps({"result": piece})) <= limit for piece in pieces):
        for piece in pieces:
            cli(binary, [(command, {"result": piece})], store)
        return len(pieces)
    # Feed the original engine one complete run so its end-of-run fence
    # includes errors or skipped pages from any earlier piece.
    payload = json.dumps({"mode": "apply", "store": str(store), "command": command, "pieces": pieces})
    output = subprocess.run([host], input=payload, text=True, capture_output=True, timeout=180)
    if output.returncode or json.loads(output.stdout) != {"applied": len(pieces)}:
        raise ValueError("MailCraft engine apply failed; private details suppressed")
    return len(pieces)


def cycle(args, url, user, password, store):
    if not store.exists():
        cli(args.cli, [("account.add", {"email": user, "kind": "exchange", "ews": {"url": url}})], store)
    request = cli(args.cli, [("sync.ewsRequest", {})], store)[0]
    if request.get("url") != url or request.get("email") != user:
        raise ValueError("stored account does not match requested scope")
    response = runner(args.runner, request, password, args.ca, args.mode)
    applied = apply_results(args.cli, args.runner, store, response["results"])
    return {"ok": response["ok"], "networkRequests": response["networkRequests"], "runnerExitCode": response["exitCode"],
            "appliedPieces": applied, "pages": sum(len(p.get("pages", [])) for p in response["results"]),
            "whole": any(p.get("whole", False) for p in response["results"])}


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--runner", required=True)
    parser.add_argument("--cli", default="/usr/bin/mailcraft-cli")
    parser.add_argument("--fixture", action="store_true")
    parser.add_argument("--credentials", type=Path)
    parser.add_argument("--url")
    parser.add_argument("--ca", default="")
    parser.add_argument("--mode", choices=["sync", "probe"], default="sync")
    parser.add_argument("--store", type=Path)
    parser.add_argument("--report", type=Path)
    args = parser.parse_args()
    if bool(args.fixture) == bool(args.credentials) or (args.credentials and not args.url):
        parser.error("choose --fixture or --credentials with --url")
    os.umask(0o077)
    fixture = None
    try:
        if args.fixture:
            fixture = subprocess.Popen([args.runner, "fixture"], stdout=subprocess.PIPE, text=True)
            url, user, password = fixture.stdout.readline().strip(), "lab@example.invalid", "synthetic"
        else:
            values = credentials(args.credentials)
            url, user, password = args.url, values["user"], values["pass"]
            if urllib.parse.urlsplit(url).scheme != "https":
                raise ValueError("real credentials require HTTPS")
        with tempfile.TemporaryDirectory(prefix="ewsgo-mailcraft-") as directory:
            store = args.store or Path(directory) / "private.mailcraft"
            if store.exists() and store.stat().st_mode & 0o077:
                raise ValueError("mailbox store requires owner-only permissions")
            report = cycle(args, url, user, password, store)
            if args.fixture:
                bad = runner(args.runner, {"url": url, "email": user}, "wrong-synthetic", mode="probe")
                report["syntheticBadPasswordRejected"] = not bad["ok"] and bad["exitCode"] != 0 and bad["networkRequests"] > 0
                report["ok"] &= report["syntheticBadPasswordRejected"]
        report.update({"profile": "synthetic-network" if args.fixture else "zimbra-network", "mailcraftRevision": REVISION,
                       "cliVersion": subprocess.check_output([args.cli, "--version"], text=True).strip(),
                       "installedCLIRevision": "undeclared", "cliSHA256": hashlib.sha256(Path(args.cli).read_bytes()).hexdigest()})
        report["harnessSourceSHA256"] = hashlib.sha256(Path(__file__).with_name("main.rs").read_bytes()).hexdigest()
    except Exception:
        report = {"ok": False, "failure": "harness failed; private details suppressed"}
    finally:
        if fixture:
            fixture.terminate()
            fixture.wait(timeout=5)
    text = json.dumps(report, indent=2) + "\n"
    if args.report:
        args.report.write_text(text)
    print(text, end="")
    raise SystemExit(0 if report["ok"] else 1)


if __name__ == "__main__":
    main()
