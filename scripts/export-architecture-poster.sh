#!/usr/bin/env bash
# 从交互架构图导出 4K 静态海报（矢量渲染，文字零乱码）。
#
# 原理：doc/assets/tako-architecture.html 带上 ?poster=1 后只保留 SVG 画布，
# 由无头 Chrome 按 viewBox 比例整屏截图成 PNG。图里所有中文都是真实矢量文本，
# 不经过任何图像模型，所以不存在乱码。
#
# 品牌头用的是 tmp/tako-brand/final 的定稿 Logo（Tako Grid 图形 + 轮廓字标，
# 燕麦 #F0DEC2 / 焦糖 #BB7738 / 可可 #543A29）。品牌资产的入库问题见
# doc/problems/品牌只落Logo/问题与方案.md —— 素材目前只在 tmp/，尚未进 web/public/brand/。
#
# 用法：scripts/export-architecture-poster.sh [输出路径]
# 依赖：Google Chrome（或用 TAKO_CHROME 指定其他 Chromium 内核浏览器）
set -euo pipefail
cd "$(dirname "$0")/.."

OUT="${1:-doc/assets/tako-architecture.png}"
SRC="$PWD/doc/assets/tako-architecture.html"
CHROME="${TAKO_CHROME:-/Applications/Google Chrome.app/Contents/MacOS/Google Chrome}"

[ -x "$CHROME" ] || { echo "找不到浏览器：$CHROME（用 TAKO_CHROME 覆盖）" >&2; exit 2; }
[ -f "$SRC" ]  || { echo "找不到 $SRC" >&2; exit 2; }

# 画布 viewBox 是 1470×1080；3840 宽对应 2822 高（3840 / 1470 * 1080）
W=3840
H=2822
TMP="$(mktemp -d "${TMPDIR:-/tmp}/tako-poster.XXXXXX")"

"$CHROME" --headless=new --disable-gpu --hide-scrollbars --no-sandbox \
  --user-data-dir="$TMP/cr" --force-device-scale-factor=1 \
  --window-size="$W,$H" --virtual-time-budget=6000 \
  --screenshot="$TMP/out.png" "file://$SRC?poster=1" >/dev/null 2>&1

[ -s "$TMP/out.png" ] || { echo "截图失败" >&2; exit 1; }
cp "$TMP/out.png" "$OUT"
echo "已导出：$OUT  $(file -b "$OUT" | cut -d, -f2)  $(du -h "$OUT" | cut -f1)"
echo "临时目录（可自行清理）：$TMP"
