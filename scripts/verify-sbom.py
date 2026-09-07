#!/usr/bin/env python3
"""Fail when the checked-in CycloneDX module inventory differs from go.mod."""

import json
import argparse
import pathlib
import subprocess
import sys


root = pathlib.Path(__file__).resolve().parent.parent
parser = argparse.ArgumentParser()
parser.add_argument("--version", help="expected application version for a release tag")
parser.add_argument("--modules-from", help="read module list from file (- for stdin) instead of running go list")
args = parser.parse_args()
with (root / "sbom.json").open(encoding="utf-8") as handle:
    document = json.load(handle)

metadata_version = document.get("metadata", {}).get("component", {}).get("version")
main_versions = [
    component.get("version")
    for component in document.get("components", [])
    if component.get("name") == "github.com/darkharrbor/darkharrbor"
]
if len(main_versions) != 1 or metadata_version != main_versions[0]:
    print("SBOM application versions are missing or inconsistent", file=sys.stderr)
    raise SystemExit(1)
if args.version and metadata_version != args.version:
    print("SBOM application version does not match release tag", file=sys.stderr)
    raise SystemExit(1)

sbom_entries = [
    (component["name"], component["version"])
    for component in document.get("components", [])
    if component.get("name") != "github.com/darkharrbor/darkharrbor"
]
sbom = set(sbom_entries)
if len(sbom) != len(sbom_entries):
    print("duplicate module/version entry in SBOM", file=sys.stderr)
    raise SystemExit(1)
if args.modules_from:
    if args.modules_from == "-":
        mod_text = sys.stdin.read()
    else:
        mod_text = pathlib.Path(args.modules_from).read_text(encoding="utf-8")
    modules = {tuple(line.split(" ", 1)) for line in mod_text.splitlines() if line}
else:
    result = subprocess.run(
        ["go", "list", "-mod=readonly", "-m", "-f", "{{if not .Main}}{{.Path}} {{.Version}}{{end}}", "all"],
        cwd=root / "src",
        check=True,
        capture_output=True,
        text=True,
    )
    modules = {tuple(line.split(" ", 1)) for line in result.stdout.splitlines() if line}

if modules != sbom:
    for name, entries in (("missing from SBOM", modules - sbom), ("stale in SBOM", sbom - modules)):
        for module, version in sorted(entries):
            print(f"{name}: {module} {version}", file=sys.stderr)
    raise SystemExit(1)

print(f"SBOM inventory: PASS (application {metadata_version}, {len(modules)} modules)")
