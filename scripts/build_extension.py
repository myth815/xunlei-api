#!/usr/bin/env python3
import argparse
import base64
import hashlib
import json
import re
from pathlib import Path
from zipfile import ZIP_DEFLATED, ZipFile, ZipInfo


ROOT = Path(__file__).resolve().parents[1]
SOURCE = ROOT / "extension"
EXPECTED_EXTENSION_ID = "pjfbokcciledhphokcaookifodbgdmbb"
RUNTIME_FILES = (
    "manifest.json",
    "background.js",
    "shared.mjs",
    "options.html",
    "options.css",
    "options.js",
    "icons/icon16.png",
    "icons/icon32.png",
    "icons/icon48.png",
    "icons/icon128.png",
)


def release_version(value: str) -> str:
    version = value.removeprefix("v")
    match = re.fullmatch(r"(\d+\.\d+\.\d+(?:\.\d+)?)(?:-[0-9A-Za-z.-]+)?", version)
    if not match:
        raise ValueError("extension version must use a release version such as 0.3.0 or 0.3.0-rc.1")
    return match.group(1)


def archive_info(name: str) -> ZipInfo:
    info = ZipInfo(name, date_time=(1980, 1, 1, 0, 0, 0))
    info.compress_type = ZIP_DEFLATED
    info.external_attr = 0o100644 << 16
    return info


def extension_id(public_key: str) -> str:
    try:
        digest = hashlib.sha256(base64.b64decode(public_key, validate=True)).digest()[:16]
    except (ValueError, TypeError) as error:
        raise SystemExit(f"manifest key is not valid base64: {error}") from error
    return "".join(chr(ord("a") + nibble) for byte in digest for nibble in (byte >> 4, byte & 0x0F))


def main() -> None:
    parser = argparse.ArgumentParser(description="Build the unpacked Chrome extension archive")
    parser.add_argument("--version", default=json.loads((SOURCE / "manifest.json").read_text())["version"])
    parser.add_argument("--output")
    args = parser.parse_args()
    version = release_version(args.version)
    output = Path(args.output) if args.output else ROOT / "dist" / f"xunlei-api-chrome-extension_{version}.zip"

    missing = [name for name in RUNTIME_FILES if not (SOURCE / name).is_file()]
    if missing:
        raise SystemExit(f"missing extension files: {', '.join(missing)}")

    manifest = json.loads((SOURCE / "manifest.json").read_text())
    if manifest.get("manifest_version") != 3:
        raise SystemExit("only Manifest V3 packages are supported")
    actual_extension_id = extension_id(manifest.get("key", ""))
    if actual_extension_id != EXPECTED_EXTENSION_ID:
        raise SystemExit(
            f"manifest key derives {actual_extension_id}, expected stable extension ID {EXPECTED_EXTENSION_ID}"
        )
    manifest["version"] = version
    manifest_bytes = (json.dumps(manifest, ensure_ascii=False, indent=2) + "\n").encode()

    output.parent.mkdir(parents=True, exist_ok=True)
    with ZipFile(output, "w") as archive:
        for name in RUNTIME_FILES:
            data = manifest_bytes if name == "manifest.json" else (SOURCE / name).read_bytes()
            archive.writestr(archive_info(name), data)

    digest = hashlib.sha256(output.read_bytes()).hexdigest()
    print(f"{output}: {output.stat().st_size} bytes sha256:{digest}")


if __name__ == "__main__":
    main()
