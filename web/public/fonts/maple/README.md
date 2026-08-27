# Maple Mono Normal CN lazy web fonts

- Typeface: Maple Mono Normal CN v7.9
- Upstream: https://github.com/subframe7536/maple-font
- Persistent source cache: `~/.cache/maple-font/v7.9/`
- Official archive SHA-256: `0ee9557b3f4c94564b667a45ee9fb22818f880d87bf170687c7b3d0151c584cb`
- License: SIL Open Font License 1.1; see `LICENSE.txt`

`generated/` contains complete WOFF2 coverage for weights 400, 600, and 700. Each weight has one small eager UI shard plus lazy fallback shards grouped into 8,192-codepoint Unicode regions. The shards are disjoint and together cover all 22,731 Unicode codepoints supported by the official CN source font.

The browser uses the generated `unicode-range` declarations in `web/src/maple-font.css` to download only the shards required by text on the page. Content hashes are part of every filename, so rebuilt subsets cannot be confused with an older browser cache.

The build script scans static text in `web/src`, `web/index.html`, and fixed backend strings in non-test Go files. These characters are placed in the eager UI shard; all other supported characters remain available from the fallback shards, so dynamic API text does not fall back merely because it was absent during the build.

Commands:

```bash
cd web
npm run fonts:check  # verify manifest, hashes, WOFF2 signatures, and generated CSS
npm run fonts:build  # force a rebuild from the persistent v7.9 TTF cache
npm run build        # automatically rebuilds only when scanned UI text changed
```

Set `MAPLE_FONT_CACHE` to override the source-cache directory. Generated assets are committed, so unchanged builds do not require fontTools or the source TTF files.
