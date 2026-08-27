#!/usr/bin/env python3
"""Build complete, lazy WOFF2 shards for Maple Mono Normal CN v7.9.

The generated assets are committed. Normal frontend builds only verify the manifest and
skip fontTools entirely. When UI text changes, the script rebuilds from the persistent
source cache (override with MAPLE_FONT_CACHE).
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import shutil
import sys
import tempfile
from pathlib import Path

BUILD_VERSION = 4
FONT_VERSION = "7.9"
# 8K-codepoint shards preserve WOFF2 cross-glyph compression while still letting the
# browser fetch only the CJK region needed by dynamic text.
SHARD_SIZE = 0x2000
WEIGHTS = {
    400: "Regular",
    600: "SemiBold",
    700: "Bold",
}
BASE_UI_RANGES = (
    (0x0000, 0x00FF),
    (0x2000, 0x206F),
    (0x2070, 0x209F),
    (0x20A0, 0x20CF),
    (0x2100, 0x214F),
    (0x2190, 0x21FF),
    (0x2300, 0x23FF),
    (0x2500, 0x25FF),
)

SCRIPT_DIR = Path(__file__).resolve().parent
WEB_ROOT = SCRIPT_DIR.parent
REPO_ROOT = WEB_ROOT.parent
OUTPUT_DIR = WEB_ROOT / "public" / "fonts" / "maple" / "generated"
CSS_PATH = WEB_ROOT / "src" / "maple-font.css"
INDEX_PATH = WEB_ROOT / "index.html"
MANIFEST_PATH = OUTPUT_DIR / "manifest.json"


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def file_sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def collect_ui_codepoints() -> set[int]:
    codepoints: set[int] = set()
    for start, end in BASE_UI_RANGES:
        codepoints.update(range(start, end + 1))

    files: list[Path] = [WEB_ROOT / "index.html"]
    for suffix in ("*.ts", "*.tsx", "*.css", "*.html"):
        files.extend((WEB_ROOT / "src").rglob(suffix))
    # Backend-generated fixed error text is rendered by the management UI. Tests are
    # excluded because their fixtures should not enlarge the eager shard.
    files.extend(path for path in (REPO_ROOT / "server").rglob("*.go") if not path.name.endswith("_test.go"))

    for path in sorted(set(files)):
        if not path.is_file():
            continue
        for char in path.read_text(encoding="utf-8", errors="ignore"):
            cp = ord(char)
            if cp > 0xFF and not char.isspace():
                codepoints.add(cp)
    return codepoints


def ui_fingerprint(codepoints: set[int]) -> str:
    encoded = ",".join(f"{cp:X}" for cp in sorted(codepoints)).encode()
    return sha256_bytes(encoded)


def load_manifest() -> dict | None:
    try:
        return json.loads(MANIFEST_PATH.read_text(encoding="utf-8"))
    except (FileNotFoundError, json.JSONDecodeError):
        return None


def generated_assets_current(fingerprint: str) -> bool:
    manifest = load_manifest()
    if not manifest:
        return False
    if manifest.get("build_version") != BUILD_VERSION or manifest.get("font_version") != FONT_VERSION:
        return False
    if manifest.get("shard_size") != SHARD_SIZE or manifest.get("ui_fingerprint") != fingerprint:
        return False
    files = manifest.get("files")
    if not isinstance(files, list) or not files:
        return False
    for name in files:
        path = OUTPUT_DIR / name
        if not path.is_file():
            return False
        data = path.read_bytes()
        parts = name.rsplit(".", 2)
        if len(parts) != 3 or parts[1] != sha256_bytes(data)[:12] or not data.startswith(b"wOF2"):
            return False
    if not CSS_PATH.is_file() or file_sha256(CSS_PATH) != manifest.get("css_sha256"):
        return False
    preload_files = manifest.get("preload_files")
    if not isinstance(preload_files, list) or not INDEX_PATH.is_file():
        return False
    index = INDEX_PATH.read_text(encoding="utf-8")
    if any(f"/fonts/maple/generated/{name}" not in index for name in preload_files):
        return False
    return True


def compact_unicode_range(codepoints: set[int]) -> str:
    values = sorted(codepoints)
    if not values:
        return ""
    ranges: list[tuple[int, int]] = []
    start = previous = values[0]
    for cp in values[1:]:
        if cp == previous + 1:
            previous = cp
            continue
        ranges.append((start, previous))
        start = previous = cp
    ranges.append((start, previous))
    return ",".join(
        f"U+{start:04X}" if start == end else f"U+{start:04X}-{end:04X}"
        for start, end in ranges
    )


def subset_font(source: Path, output: Path, codepoints: set[int]) -> None:
    from fontTools import subset
    from fontTools.ttLib import TTFont

    options = subset.Options()
    options.flavor = "woff2"
    options.layout_features = ["*"]
    options.name_IDs = ["*"]
    options.name_languages = ["*"]
    options.name_legacy = True
    options.drop_tables.append("meta")
    font = TTFont(source, recalcTimestamp=False)
    font.recalcTimestamp = False
    font.flavor = "woff2"
    subsetter = subset.Subsetter(options=options)
    subsetter.populate(unicodes=codepoints)
    subsetter.subset(font)
    font.save(output)
    font.close()


def write_hashed_subset(
    source: Path,
    temp_dir: Path,
    label: str,
    codepoints: set[int],
) -> tuple[str, int]:
    temporary = temp_dir / f"{label}.woff2"
    subset_font(source, temporary, codepoints)
    data = temporary.read_bytes()
    digest = sha256_bytes(data)[:12]
    filename = f"{label}.{digest}.woff2"
    temporary.rename(temp_dir / filename)
    return filename, len(data)


def font_face(weight: int, filename: str, codepoints: set[int], label: str) -> str:
    return "\n".join(
        [
            f"/* {label} */",
            "@font-face {",
            "  font-family: 'Maple UI';",
            f"  src: url('/fonts/maple/generated/{filename}') format('woff2');",
            "  font-style: normal;",
            f"  font-weight: {weight};",
            "  font-display: swap;",
            f"  unicode-range: {compact_unicode_range(codepoints)};",
            "}",
        ]
    )


def build(codepoints: set[int], fingerprint: str) -> None:
    cache_root = Path(
        os.environ.get("MAPLE_FONT_CACHE", str(Path.home() / ".cache" / "maple-font" / f"v{FONT_VERSION}"))
    ).expanduser()
    sources = {
        weight: cache_root / f"MapleMonoNormal-CN-{source_weight}.ttf"
        for weight, source_weight in WEIGHTS.items()
    }
    missing = [str(path) for path in sources.values() if not path.is_file()]
    if missing:
        raise SystemExit(
            "Maple source cache is required because UI text changed. Missing:\n  "
            + "\n  ".join(missing)
            + "\nSet MAPLE_FONT_CACHE to the v7.9 source directory."
        )
    try:
        import fontTools  # noqa: F401
        import brotli  # noqa: F401
    except ImportError as exc:
        raise SystemExit("fontTools with WOFF2 support is required to rebuild Maple fonts") from exc

    OUTPUT_DIR.parent.mkdir(parents=True, exist_ok=True)
    temp_parent = Path(tempfile.mkdtemp(prefix="maple-generated.", dir=OUTPUT_DIR.parent))
    temp_assets = temp_parent / "generated"
    temp_assets.mkdir()
    faces: list[str] = []
    files: list[str] = []
    manifest_weights: dict[str, dict] = {}
    ui_files: dict[int, str] = {}

    try:
        for weight, source in sources.items():
            from fontTools.ttLib import TTFont

            probe = TTFont(source, lazy=True, recalcTimestamp=False)
            supported = {
                cp
                for table in probe["cmap"].tables
                if table.isUnicode()
                for cp in table.cmap
            }
            probe.close()
            eager = supported & codepoints
            fallback = supported - eager
            weight_files: list[dict] = []

            ui_filename, ui_size = write_hashed_subset(
                source, temp_assets, f"maple-cn-{weight}-ui", eager
            )
            files.append(ui_filename)
            ui_files[weight] = ui_filename
            weight_files.append({"file": ui_filename, "kind": "ui", "bytes": ui_size, "glyphs": len(eager)})
            faces.append(font_face(weight, ui_filename, eager, f"Maple CN {weight} eager UI shard"))

            buckets: dict[int, set[int]] = {}
            for cp in fallback:
                buckets.setdefault(cp // SHARD_SIZE, set()).add(cp)
            for bucket, shard_codepoints in sorted(buckets.items()):
                start = bucket * SHARD_SIZE
                end = start + SHARD_SIZE - 1
                label = f"maple-cn-{weight}-u{start:04x}-{end:04x}"
                filename, size = write_hashed_subset(source, temp_assets, label, shard_codepoints)
                files.append(filename)
                weight_files.append(
                    {
                        "file": filename,
                        "kind": "fallback",
                        "bytes": size,
                        "glyphs": len(shard_codepoints),
                        "range": f"U+{start:04X}-{end:04X}",
                    }
                )
                faces.append(font_face(weight, filename, shard_codepoints, f"Maple CN {weight} fallback {start:04X}-{end:04X}"))

            manifest_weights[str(weight)] = {
                "source_sha256": file_sha256(source),
                "supported_codepoints": len(supported),
                "ui_codepoints": len(eager),
                "fallback_codepoints": len(fallback),
                "files": weight_files,
            }

        css = (
            "/* Generated by scripts/build-maple-fonts.py. Do not edit manually. */\n"
            f"/* Maple Mono Normal CN v{FONT_VERSION}; complete lazy coverage at 400/600/700. */\n\n"
            + "\n\n".join(faces)
            + "\n"
        )
        css_bytes = css.encode()
        manifest = {
            "build_version": BUILD_VERSION,
            "font_version": FONT_VERSION,
            "shard_size": SHARD_SIZE,
            "ui_fingerprint": fingerprint,
            "requested_ui_codepoints": len(codepoints),
            "css_sha256": sha256_bytes(css_bytes),
            "preload_files": [ui_files[400], ui_files[600]],
            "files": sorted(files),
            "weights": manifest_weights,
        }
        (temp_assets / "manifest.json").write_text(
            json.dumps(manifest, ensure_ascii=False, indent=2) + "\n", encoding="utf-8"
        )
        temp_css = temp_parent / "maple-font.css"
        temp_css.write_bytes(css_bytes)

        index = INDEX_PATH.read_text(encoding="utf-8")
        preload_block = "\n".join(
            [
                "    <!-- maple-font-preloads:start -->",
                *[
                    f'    <link rel="preload" href="/fonts/maple/generated/{ui_files[weight]}" as="font" type="font/woff2" crossorigin />'
                    for weight in (400, 600)
                ],
                "    <!-- maple-font-preloads:end -->",
            ]
        )
        updated_index, replacements = re.subn(
            r"    <!-- maple-font-preloads:start -->.*?    <!-- maple-font-preloads:end -->",
            preload_block,
            index,
            count=1,
            flags=re.DOTALL,
        )
        if replacements != 1:
            raise RuntimeError("index.html is missing the generated Maple preload markers")
        temp_index = temp_parent / "index.html"
        temp_index.write_text(updated_index, encoding="utf-8")

        backup = OUTPUT_DIR.with_name("generated.previous")
        if backup.exists():
            shutil.rmtree(backup)
        if OUTPUT_DIR.exists():
            OUTPUT_DIR.rename(backup)
        temp_assets.rename(OUTPUT_DIR)
        temp_css.replace(CSS_PATH)
        temp_index.replace(INDEX_PATH)
        if backup.exists():
            shutil.rmtree(backup)
    finally:
        if temp_parent.exists():
            shutil.rmtree(temp_parent)

    total_bytes = sum((OUTPUT_DIR / name).stat().st_size for name in files)
    print(
        f"Maple CN v{FONT_VERSION}: generated {len(files)} WOFF2 shards, "
        f"{total_bytes / (1024 * 1024):.1f} MiB total"
    )


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--force", action="store_true", help="rebuild even when the manifest is current")
    parser.add_argument("--check", action="store_true", help="verify generated assets without rebuilding")
    args = parser.parse_args()

    codepoints = collect_ui_codepoints()
    fingerprint = ui_fingerprint(codepoints)
    current = generated_assets_current(fingerprint)
    if args.check:
        if not current:
            raise SystemExit("Maple generated assets are stale; run npm run fonts:build")
        print("Maple generated assets are current")
        return
    if current and not args.force:
        print("Maple generated assets are current")
        return
    build(codepoints, fingerprint)


if __name__ == "__main__":
    main()
