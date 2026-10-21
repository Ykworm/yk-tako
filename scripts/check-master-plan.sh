#!/usr/bin/env bash
# 检查 doc/16 总纲有没有漏掉 doc/README.md §1「现行真源」表里的编号。
#
# 背景：doc/16 自称「一份文件看全做到哪了」。它曾连续两轮只更新了 README、
# 没更新总纲——下一场会话按 README 的「第一次进仓库」顺序读完总纲，以为
# doc/35–40 这六篇现行真源不存在，得再翻索引才看到结论。
# 本脚本把「目录页和结论页都得更」那条规则变成可执行的检查。
#
# 用法：scripts/check-master-plan.sh
# 退出码：0 = 全覆盖；1 = 有漏引（打印漏掉的编号）；2 = 环境不对（文件不在）。

set -uo pipefail

cd "$(dirname "$0")/.." || exit 2

master="doc/16-能力复刻总规划.md"
index="doc/README.md"

[ -f "$master" ] || { echo "找不到 $master"; exit 2; }
[ -f "$index" ] || { echo "找不到 $index"; exit 2; }

missing=()

# 取 README §1「现行真源」表的编号列（纯数字的才算；19-v6 / 19v6-* 是文档集，另判）。
while read -r n; do
  [ -z "$n" ] && continue
  [ "$n" = "16" ] && continue            # 总纲自己
  case "$n" in *-*) continue ;; esac      # 文档集编号（19-v6、19v6-01…）另有检查
  if ! grep -qE "doc/${n}([^0-9]|$)" "$master"; then
    missing+=("doc/${n}")
  fi
done < <(awk '/^## 1\. 现行真源/{f=1;next} /^## /{f=0} f && /^\| [0-9]+ \|/{print $2}' "$index" | tr -d ' ')

# v6 文档集：总纲至少要指向集入口和契约篇
if ! grep -qE "19v6-01" "$master"; then
  missing+=("doc/19-v6（文档集入口与 19v6-01 契约篇）")
fi

if [ ${#missing[@]} -eq 0 ]; then
  echo "OK：doc/16 总纲已覆盖 doc/README.md §1 的全部现行真源编号。"
  exit 0
fi

echo "FAIL：doc/16 总纲漏引以下现行真源 ——"
for m in "${missing[@]}"; do echo "  - $m"; done
echo
echo "补法：在 doc/16 的 §2 能力表或 §2c / §6 索引里加上对应指针；"
echo "同一批更新 doc/README.md §1 的 16 行说明与基线日期。"
exit 1
