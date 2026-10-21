#!/usr/bin/env bash
# 校验两边的用例号集合一致：
#   人读版 = doc/19-v6/19v6-04-验收-curl.md（散文里的「用例 N」）
#   机器版 = scripts/acceptance/expected.json（cases[].case + declared_not_asserted 的键）
#
# 为什么要有这个脚本：期望字节有两份（散文 + 机器清单），两份同批改。
# 光靠自觉会单边漂——散文改了、清单没改，机器断言就在验一个没人认的契约。
# 这个脚本把「同批改」变成可执行的检查。
#
# 退出码：0 一致；1 不一致；2 文件不在。
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo="$(cd "$here/../.." && pwd)"
doc="$repo/doc/19-v6/19v6-04-验收-curl.md"
man="$here/expected.json"

[ -f "$doc" ] || { echo "找不到 $doc" >&2; exit 2; }
[ -f "$man" ] || { echo "找不到 $man" >&2; exit 2; }

doc_cases=$(grep -oE '用例 [0-9]+' "$doc" | sort -u -V)
man_cases=$(python3 - "$man" <<'PY'
import json, sys
m = json.load(open(sys.argv[1]))
names = {c["case"] for c in m["cases"]} | set(m.get("declared_not_asserted", {}).keys())
print("\n".join(sorted(names, key=lambda s: int(s.split()[1]))))
PY
)

if [ "$doc_cases" = "$man_cases" ]; then
  echo "用例号集合一致。"
  echo
  echo "已断言（expected.json 的 cases[]）："
  python3 - "$man" <<'PY'
import json, sys
m = json.load(open(sys.argv[1]))
for c in m["cases"]:
    print(f"  {c['case']}  {c['id']}  [{c.get('tier', 'core')}]  {c.get('desc', '')}")
PY
  echo
  echo "声明了但暂不断言（declared_not_asserted）："
  python3 - "$man" <<'PY'
import json, sys
m = json.load(open(sys.argv[1]))
for k, v in m.get("declared_not_asserted", {}).items():
    print(f"  {k}  {v}")
PY
  exit 0
fi

echo "用例号集合不一致："
echo "--- 只有散文里有 ---"
comm -23 <(printf '%s\n' "$doc_cases") <(printf '%s\n' "$man_cases")
echo "--- 只有清单里有 ---"
comm -13 <(printf '%s\n' "$doc_cases") <(printf '%s\n' "$man_cases")
echo
echo "改了一边就要改另一边：散文见 doc/19-v6/19v6-04-验收-curl.md，清单见 scripts/acceptance/expected.json。"
exit 1
