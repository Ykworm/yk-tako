#!/usr/bin/env bash
# 停掉 up.sh 起的隔离引擎（施工包 §6.1）：只停 engine.pid 登记且命令行确属本次二进制的进程。
# 默认保留证据、隔离库与运行目录供复核；--purge 才删除运行目录（含 workspace 与日志）。
# 绝不调用 /api/dev/reset，绝不自动 drop database，绝不杀端口上的其他进程。
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
purge=""
run_dir="${1:-}"

if [ "$run_dir" = "--purge" ]; then purge=1; run_dir=""; fi
if [ -z "$run_dir" ]; then
  # 默认取最近一次 runtime-* 目录。
  run_dir="$(ls -d "$repo_root"/tmp/eval/runtime-* 2>/dev/null | sort | tail -1 || true)"
fi
if [ -z "$run_dir" ] || [ ! -d "$run_dir" ]; then
  echo "找不到运行目录（tmp/eval/runtime-*）；用法：reset.sh [run_dir|--purge]" >&2
  exit 1
fi
# 转绝对路径：命令行与 cwd 校验都按绝对路径比。
run_dir="$(cd "$run_dir" && pwd)"
pid_file="$run_dir/engine.pid"
if [ ! -f "$pid_file" ]; then
  # 已正常停过的实例没有 pidfile；--purge 可清目录，普通调用不动。
  if [ -n "$purge" ] && { [ -f "$run_dir/env.sh" ] || [ -x "$run_dir/yktako" ]; }; then
    rm -rf "$run_dir"
    echo "已删除运行目录 ${run_dir}（此前已停止；数据库与 Redis 键不在此目录，需自行清理）。"
    exit 0
  fi
  echo "${run_dir} 没有 engine.pid，不是 up.sh 创建的实例；不动。" >&2
  exit 1
fi
pid="$(head -1 "$pid_file" 2>/dev/null || true)"
if [ -z "$pid" ]; then
  echo "${pid_file} 为空。" >&2
  exit 1
fi

# 校验确属本次二进制：argv0 是 ./yktako（相对路径），所以主判据是
# cwd == 运行目录 且命令行带 -workspace <run_dir>/workspace；
# 命令行直接出现本实例二进制绝对路径也算（防误杀复用 PID，两条都不中才拒停）。
cmdline="$(ps -p "$pid" -o command= 2>/dev/null || true)"
cwd="$(lsof -a -p "$pid" -d cwd -Fn 2>/dev/null | tail -1 | sed 's/^n//')"
belongs=0
if [ "$cwd" = "$run_dir" ] && echo "$cmdline" | grep -qF -- "-workspace $run_dir/workspace"; then
  belongs=1
fi
if echo "$cmdline" | grep -qF "$run_dir/yktako"; then
  belongs=1
fi
if [ -z "$cmdline" ]; then
  echo "PID ${pid} 已不在；清掉 pidfile 收尾。"
  rm -f "$pid_file"
elif [ "$belongs" = 0 ]; then
  echo "PID ${pid} 不属于本实例（cwd=${cwd}，cmdline=${cmdline}）" >&2
  echo "拒绝停止；请人工核对。" >&2
  exit 1
else
  kill -TERM "$pid" 2>/dev/null || true
  for _ in $(seq 1 40); do
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.25
  done
  if kill -0 "$pid" 2>/dev/null; then
    echo "10s 内未退出；确认后可手动 kill -9 ${pid}。" >&2
    exit 1
  fi
  rm -f "$pid_file"
  echo "已停 PID ${pid}。"
fi

if [ -n "$purge" ]; then
  rm -rf "$run_dir"
  echo "已删除运行目录 ${run_dir}（数据库与 Redis 键不在此目录，需自行清理）。"
else
  echo "保留运行目录 ${run_dir}（证据、workspace、日志）；数据库与 Redis 键默认保留供复核。"
fi
