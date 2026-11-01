#!/usr/bin/env bash
# 守「一行的键集 == row.schema.json」。键集不等即非零退出。
#
# 输入可以是 jsonl（一行一个对象）、含 "rows" 数组的 json、单个行对象，或行对象数组。
set -euo pipefail
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
python3 - "$ROOT" "$@" <<'PY'
import json
import pathlib
import sys

root = pathlib.Path(sys.argv[1])
keys = json.load(open(root / "scripts/bench/row.schema.json"))["keys"]
bad = 0
total = 0
for path in sys.argv[2:]:
    text = open(path).read()
    try:
        obj = json.loads(text)
        if isinstance(obj, dict) and "rows" in obj:
            rows = obj["rows"]
        elif isinstance(obj, dict):
            rows = [obj]
        else:
            rows = obj
    except json.JSONDecodeError:
        rows = [json.loads(line) for line in text.splitlines() if line.strip()]
    for i, row in enumerate(rows):
        total += 1
        if list(row.keys()) != keys:
            bad += 1
            print("❌ %s[%d] 键集不等" % (path, i))
            print("   缺: %s" % sorted(set(keys) - set(row.keys())))
            print("   多: %s" % sorted(set(row.keys()) - set(keys)))
        else:
            print("✅ %s[%d] 键集一致" % (path, i))
if bad:
    print("键集不一致的行：%d / %d" % (bad, total))
    sys.exit(1)
print("全部 %d 行的键集与 row.schema.json 一致" % total)
PY
