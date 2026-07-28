#!/usr/bin/env python3
"""Collect deterministic dependency, license, and resolved-version evidence."""

from __future__ import annotations

import argparse
import hashlib
import importlib.metadata
import json
from pathlib import Path


def parse_debian_status(status_path: Path) -> list[dict[str, str]]:
    """Build the installed Debian package inventory from dpkg status."""
    packages: list[dict[str, str]] = []
    for stanza in status_path.read_text(encoding="utf-8").split("\n\n"):
        fields: dict[str, str] = {}
        for line in stanza.splitlines():
            if not line or line[0].isspace() or ": " not in line:
                continue
            name, value = line.split(": ", 1)
            fields[name] = value
        if fields.get("Status") != "install ok installed":
            continue
        package_name = fields["Package"]
        copyright_path = Path("/usr/share/doc") / package_name.split(":", 1)[0] / "copyright"
        if not copyright_path.is_file():
            raise SystemExit(
                f"SecondBox provenance collection failed: missing Debian copyright file for {package_name}"
            )
        packages.append(
            {
                "architecture": fields["Architecture"],
                "copyrightPath": str(copyright_path),
                "copyrightSha256": sha256_file(copyright_path),
                "package": package_name,
                "source": fields.get("Source", package_name).split(" ", 1)[0],
                "version": fields["Version"],
            }
        )
    return sorted(packages, key=lambda package: (package["package"], package["architecture"]))


def collect_python_inventory() -> list[dict[str, object]]:
    """Build the installed Python distribution and declared-license inventory."""
    distributions: list[dict[str, object]] = []
    for distribution in importlib.metadata.distributions():
        name = distribution.metadata.get("Name")
        if not name:
            raise SystemExit("SecondBox provenance collection failed: Python distribution has no Name")
        metadata_text = distribution.read_text("METADATA") or ""
        license_classifiers = sorted(
            classifier
            for classifier in distribution.metadata.get_all("Classifier", [])
            if classifier.startswith("License ::")
        )
        distributions.append(
            {
                "license": distribution.metadata.get("License", ""),
                "licenseClassifiers": license_classifiers,
                "licenseExpression": distribution.metadata.get("License-Expression", ""),
                "metadataSha256": hashlib.sha256(metadata_text.encode()).hexdigest(),
                "name": name,
                "version": distribution.version,
            }
        )
    return sorted(
        distributions,
        key=lambda distribution: (str(distribution["name"]).casefold(), str(distribution["version"])),
    )


def sha256_file(path: Path) -> str:
    """Hash one provenance input without loading arbitrary license files into memory."""
    digest = hashlib.sha256()
    with path.open("rb") as source:
        while chunk := source.read(1024 * 1024):
            digest.update(chunk)
    return digest.hexdigest()


def write_json(path: Path, value: object) -> None:
    """Write stable JSON so released rootfs evidence can be compared byte-for-byte."""
    path.write_text(json.dumps(value, indent=2, sort_keys=True) + "\n", encoding="utf-8")


def main() -> None:
    """Collect all required rootfs provenance outputs into one image-owned directory."""
    parser = argparse.ArgumentParser()
    parser.add_argument("--output-dir", type=Path, required=True)
    args = parser.parse_args()
    args.output_dir.mkdir(parents=True, exist_ok=True)

    debian_packages = parse_debian_status(Path("/var/lib/dpkg/status"))
    python_distributions = collect_python_inventory()
    write_json(
        args.output_dir / "rootfs-debian-license-inventory.json",
        {"schemaVersion": 1, "packages": debian_packages},
    )
    write_json(
        args.output_dir / "rootfs-python-license-inventory.json",
        {"schemaVersion": 1, "distributions": python_distributions},
    )
    (args.output_dir / "rootfs-debian-packages.lock").write_text(
        "".join(
            f"{package['package']}:{package['architecture']}={package['version']}\n"
            for package in debian_packages
        ),
        encoding="utf-8",
    )


if __name__ == "__main__":
    main()
