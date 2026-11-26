#!/usr/bin/env python3
# SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
# SPDX-License-Identifier: AGPL-3.0-or-later

"""Build original pinned EWS runner in an external directory; no upstream vendoring."""
import argparse
import hashlib
import json
from pathlib import Path
import subprocess
import tarfile
import urllib.request

REVISION = "0a630bb45d75293a3a8ea0f6d1a001de6b8e97a3"
ARCHIVE_SHA256 = "b798a9b57e4ce8370ca32a1d4c432ab679cc311364003bc066d77f6a7b656e76"


def build(cache):
    cache = cache.resolve()
    cache.mkdir(parents=True, exist_ok=True, mode=0o700)
    archive = cache / "mailcraft.tar.gz"
    if not archive.exists():
        with urllib.request.urlopen(f"https://codeload.github.com/UCS-Care-Group/mailcraft/tar.gz/{REVISION}", timeout=60) as response:
            data = response.read(100 << 20)
        if hashlib.sha256(data).hexdigest() != ARCHIVE_SHA256:
            raise ValueError("source archive digest differs")
        archive.write_bytes(data)
    if hashlib.sha256(archive.read_bytes()).hexdigest() != ARCHIVE_SHA256:
        raise ValueError("source archive digest differs")
    # Re-extract verified bytes on each build so a previously modified cache cannot drift.
    with tarfile.open(archive) as source:
        source.extractall(cache, filter="data")
    source = cache / f"mailcraft-{REVISION}"
    project = cache / "host"
    (project / "src").mkdir(parents=True, exist_ok=True)
    (project / "src/main.rs").write_bytes(Path(__file__).with_name("main.rs").read_bytes())
    manifest = '[package]\nname="ewsgo-mailcraft-host"\nversion="0.1.0"\nedition="2024"\n[dependencies]\nserde_json="=1.0.149"\n'
    for name in ("engine", "ews", "model"):
        feature = ', features=["stand-in"]' if name == "ews" else ""
        manifest += f'mailcraft-{name}={{ path={json.dumps(str(source / "crates" / name))}{feature} }}\n'
    (project / "Cargo.toml").write_text(manifest)
    lock = Path(__file__).with_name("Cargo.lock")
    if lock.exists():
        (project / "Cargo.lock").write_bytes(lock.read_bytes())
    subprocess.run(["cargo", "build", "--manifest-path", str(project / "Cargo.toml")] + (["--locked"] if lock.exists() else []), check=True)
    print(json.dumps({"binary": str(project / "target/debug/ewsgo-mailcraft-host"), "revision": REVISION, "sourceArchiveSHA256": ARCHIVE_SHA256}))


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--cache", type=Path, required=True)
    build(parser.parse_args().cache)
