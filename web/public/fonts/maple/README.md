# Maple Mono Normal CN web subsets

- Typeface: Maple Mono Normal / Maple Mono Normal CN
- Upstream: https://github.com/subframe7536/maple-font
- Local reference: `/home/maosite/develop/sprite/web/public/fonts/maple/`
- Persistent v7.9 source cache: `~/.cache/maple-font/v7.9/`
- Official archive SHA-256: `0ee9557b3f4c94564b667a45ee9fb22818f880d87bf170687c7b3d0151c584cb`
- License: SIL Open Font License 1.1; see `LICENSE.txt`

The Latin regular, semibold, and bold WOFF2 files match the Sprite application. The Chinese regular, semibold, and bold sources come from the official v7.9 `MapleMonoNormal-CN` release and are subset locally with fonttools/pyftsubset.

Static management UI glyphs are available at weights 400, 600, and 700, so headings, buttons, table headers, tooltips, and DaisyUI dialogs do not fall back to a system CJK font. Delayed broad CJK range shards use weight 400 and exclude the regular UI glyphs, preventing ordinary management pages from downloading multi-megabyte fallback shards.

The cache retains the official archive plus extracted `Regular`, `SemiBold`, and `Bold` TTF sources. Future UI subset rebuilds should use these local files instead of downloading the release again.
