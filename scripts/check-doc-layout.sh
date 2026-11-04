#!/usr/bin/env bash
# 文档归属闸：目录结构与相对链接。
set -euo pipefail
cd "$(dirname "$0")/.."

fail=0
bad() { echo "FAIL $*"; fail=1; }

# 1. 旧目录不许复活
if [ -d docs ]; then
  bad "docs/ 仍存在（记录应放 doc/log/，归档应放 doc/archive/）"
fi

# 2. doc/ 顶层不许有日期命名的文件
while IFS= read -r f; do
  bad "doc/ 顶层出现日期文件 $f"
done < <(find doc -maxdepth 1 -type f -name '*.md' | grep -E '/[0-9]{4}-[0-9]{2}-[0-9]{2}-' || true)

# 3. doc/log/ 顶层 md（README 除外）必须以 YYYY-MM-DD- 开头
for f in doc/log/*.md; do
  base=$(basename "$f")
  [ "$base" = "README.md" ] && continue
  case "$base" in
    [0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]-*) ;;
    *) bad "doc/log/$base 不是日期命名" ;;
  esac
done

# 4. doc/log/ 子目录必须是日期命名，且只放 README.md / tasks.md
for d in doc/log/*/; do
  [ -d "$d" ] || continue
  base=$(basename "$d")
  case "$base" in
    [0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]-*) ;;
    *) bad "doc/log/$base/ 不是日期命名" ;;
  esac
  while IFS= read -r f; do
    name=$(basename "$f")
    case "$name" in
      README.md|tasks.md) ;;
      *) bad "doc/log/$base/$name 不属于该目录（只放 README.md / tasks.md）" ;;
    esac
  done < <(find "$d" -type f)
done

# 5. doc/archive/README.md 覆盖归档目录下全部 md
while IFS= read -r f; do
  rel=${f#doc/archive/}
  [ "$rel" = "README.md" ] && continue
  grep -qF "$rel" doc/archive/README.md || bad "doc/archive/README.md 未收录 $rel"
done < <(find doc/archive -type f -name '*.md' | sort)

# 6. doc/ 内所有相对 .md 链接可解析
if ! python3 - <<'PY'
import os, re, sys
from urllib.parse import unquote

link_re = re.compile(r"\]\(([^)\s]+)\)")
ROOTS = ["doc"]
broken = []
total = 0
for root in ROOTS:
    for dirpath, _, files in os.walk(root):
        for name in files:
            if not name.endswith(".md"):
                continue
            path = os.path.join(dirpath, name)
            with open(path, encoding="utf-8", errors="ignore") as f:
                for lineno, line in enumerate(f, 1):
                    for raw in link_re.findall(line):
                        target = raw.split("#")[0].strip()
                        if not target or target.startswith(("http://", "https://", "mailto:", "file://")):
                            continue
                        target = unquote(target)
                        if not target.endswith(".md"):
                            continue
                        total += 1
                        resolved = os.path.normpath(os.path.join(dirpath, target))
                        if not os.path.exists(resolved):
                            broken.append(f"{path}:{lineno}: {target} -> {resolved}")
print(f"md links checked: {total}")
for b in broken:
    print(f"FAIL {b}")
sys.exit(1 if broken else 0)
PY
then
  fail=1
fi

if [ "$fail" -ne 0 ]; then
  echo "doc layout: FAILED"
  exit 1
fi
echo "doc layout: OK"
