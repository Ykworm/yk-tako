#!/usr/bin/env bash
# Git 隔离工作区活体（doc/74 §8.1/§9；施工包 §12.2 第 1 场）：预起引擎（调度循环开着），
# 真模型在隔离 worktree 里跑五场——
#   1. 并行成功：a/b 各写一份文件、c 汇合；执行期间原项目一个字节不动，发布才 ff 前进；
#   2. 父冲突：a/b 改同一文件不同内容 → c 的 worktree 出冲突标记 → 模型裁决 → 发布；
#   3. 验证返工：verifyCommand 先拒后过（程序判定，不靠 prompt 自述），帧里看得见两次提交；
#   4. 主分支前进：执行期间原项目 main 被人推进 → 发布受阻 workspace_changed →
#      retry-publication 铸处理 run → 模型 merge refs/tako/base 重新提交 → ff 发布；
#   5. 不可变产物：发布后原项目再前进，artifact 端点仍回结果版本的字节。
#
# 参数（全部环境变量，不写死本机路径端口）：
#   TAKO_BASE     引擎地址，默认 http://127.0.0.1:8943（必须开着调度循环）
#   TAKO_ENV      取模型密钥的 .env，默认 <仓库根>/yk-tako-eino/.env
#   TAKO_PG_DSN   直连 PG 的 DSN（**必填**，行数/状态断言靠它）
#   TAKO_PSQL     psql 可执行文件，默认 psql
#   TAKO_WS_ROOT  引擎的 workspace 根（文件字节断言要用），默认 <仓库根>/tmp/workspace-acc
#   TAKO_MODEL    租户模型 ref，默认 deepseek/deepseek-flash
# 纪律：git init/commit 只发生在脚本自建的隔离项目夹具里；不杀任何进程；不调 /api/dev/reset。
# Git 执行者要能跑 bash（出厂 researcher 排除 bash）：租户自定义规格，toolPolicy 默认 inherit。
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/agent_task_live_lib.sh"
ml_need_dsn

SCRATCH=$(mktemp -d "${TMPDIR:-/tmp}/tako-ws-git.XXXXXX")
trap 'rm -rf "$SCRATCH"' EXIT

echo "引擎 $B · Git 隔离五场（并行/父冲突/验证返工/主分支前进/不可变产物）"
ml_signup wsgit

# Git 执行者：inherit 基础池（拿得到 bash），规格只说角色，git 约定由 Provider overlay 注入。
AGENT="wsgit_$(date +%s)_$$"
jq -cn --arg n "$AGENT" \
  '{name:$n,kind:"subagent",description:"Git 隔离工作区执行者",spec:"你是隔离 Git worktree 的执行者。按任务内容改动文件，git add 并 git commit 后调用 submit_task_result 提交，artifacts 声明本次交付的文件（相对路径，必须已提交进 HEAD）。"}' |
  curl -sS -X POST "$B/api/tenants/$TEN/agents" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' -d @- >/dev/null

WID=$(curl -sS -X POST "$B/api/workspaces" -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
  -d '{"name":"ws-git-live"}' | jq -r .data.workspace.id)
[ -n "$WID" ] && [ "$WID" != "null" ] || { no "建项目失败"; ml_summary; }
PROJ="$WS_ROOT/tenants/$TEN/workspaces/$WID"
mkdir -p "$PROJ"
# 夹具：只在隔离测试项目里 init + commit（不碰任何真实项目）。
git -C "$PROJ" init -q -b main
printf '基线内容\n' > "$PROJ/report.md"
printf '共享基线\n' > "$PROJ/shared.txt"
git -C "$PROJ" add report.md shared.txt
git -C "$PROJ" -c user.name=probe -c user.email=probe@tako.test commit -qm seed
SEED=$(git -C "$PROJ" rev-parse HEAD)
at_probe_run "$WID"
echo "  项目 $WID · 夹具 seed=$SEED · 来源会话 $SID"

# 组目录（断言 worktree / 结果引用 / 冲突记录用）。
ws_dir(){ echo "$WS_ROOT/tenants/$TEN/.tako/orchestrations/$1/workspace"; }
# 字节断言：artifact 端点回的原始字节与期望逐字一致（cmp，不靠 shell 字符串吞换行）。
artifact_eq(){ # $1=taskId $2=path $3=期望内容 $4=标签
  curl -sS "$B/api/tasks/$1/artifact?path=$2" -H "Authorization: Bearer $TOK" -o "$SCRATCH/got.bin"
  printf '%s' "$3" > "$SCRATCH/want.bin"
  if cmp -s "$SCRATCH/got.bin" "$SCRATCH/want.bin"; then ok "$4"; else no "$4 —— 字节不符（$(wc -c < "$SCRATCH/got.bin") 字节）"; fi
}
task_field(){ at_task "$1"; echo "$AT_TASK" | jq -r "$2 // empty"; }

# ── 1. 并行成功：a/b 各写一份、c 汇合；执行期间原项目不动 ─────────────────────────
STAMP="$(date +%s)-$$"
TASKS=$(jq -cn --arg a "在工作目录创建文件 alpha.txt，内容恰好一行「甲-$STAMP」。git add 并 git commit，然后调用 submit_task_result，summary 写文件路径，artifacts 声明 alpha.txt（kind=file）。" \
  --arg b "在工作目录创建文件 beta.txt，内容恰好一行「乙-$STAMP」。git add 并 git commit，然后调用 submit_task_result，summary 写文件路径，artifacts 声明 beta.txt（kind=file）。" \
  --arg c "工作目录里已有上游留下的 alpha.txt 与 beta.txt。读齐两份，创建 final.txt：第一行是 alpha.txt 的原文，第二行是 beta.txt 的原文（各一行，不多不少）。git add 并 git commit，然后调用 submit_task_result，summary 写 final.txt 路径，artifacts 声明 final.txt（kind=file）。" \
  --arg g "$AGENT" \
  '[{key:"a",content:$a,agent:$g},{key:"b",content:$b,agent:$g},{key:"c",content:$c,agent:$g}]')
RESP=$(at_create_orch "acc-wsgit-par-$STAMP" "$WID" "$SID" "两件并行各写一份证据，c 汇合" "$TASKS" \
  '[{"parent":"a","child":"c"},{"parent":"b","child":"c"}]' "isolated")
OID=$(echo "$RESP" | jq -r '.data.orchestrationId // ""')
[ -n "$OID" ] && [ "$OID" != "null" ] || { echo "创建失败：$RESP" >&2; exit 2; }
at_orch "$OID"
TID_A=$(echo "$AT_ORCH" | jq -r '.tasks[] | select(.key=="a") | .taskId')
TID_B=$(echo "$AT_ORCH" | jq -r '.tasks[] | select(.key=="b") | .taskId')
TID_C=$(echo "$AT_ORCH" | jq -r '.tasks[] | select(.key=="c") | .taskId')
echo "  场1 orchId=$OID a=$TID_A b=$TID_B c=$TID_C"

# a 被领取的那一刻，c 还没跑、发布更没发生：原项目必须还是 seed，两份证据都不在项目里。
for i in $(seq 1 60); do
  CUR=$(task_field "$TID_A" '.currentRunId')
  [ -n "$CUR" ] && break
  sleep 1
done
[ -n "$CUR" ] || { no "60s 没等到 a 被领取"; ml_summary; }
eq "a 领取时原项目 HEAD 仍是 seed（执行不碰原项目）" "$SEED" "$(git -C "$PROJ" rev-parse HEAD)"
[ ! -f "$PROJ/alpha.txt" ] && [ ! -f "$PROJ/beta.txt" ] && ok "a 领取时项目里没有执行者的文件" || no "执行期间项目出现了执行者的文件"

if at_wait_orch "$OID" "succeeded" 300; then ok "场1 整组收口 succeeded"; else no "场1 没收口（$AT_LAST）"; at_orch "$OID"; echo "$AT_ORCH" | jq -c '.tasks[] | {key,status,error}'; ml_summary; fi
GD=$(ws_dir "$OID")
eq "组 Provider 固定为 git" "git" "$(jq -r .provider "$GD/state.json")"
# 隔离：a 的 worktree 只有 alpha，b 的只有 beta，c 汇合两份。
[ -f "$GD/work/$TID_A/alpha.txt" ] && [ ! -f "$GD/work/$TID_A/beta.txt" ] && ok "a 的 worktree 看不到 b 的文件（并行隔离）" || no "a 的 worktree 隔离被破坏"
[ -f "$GD/work/$TID_B/beta.txt" ] && [ ! -f "$GD/work/$TID_B/alpha.txt" ] && ok "b 的 worktree 看不到 a 的文件（并行隔离）" || no "b 的 worktree 隔离被破坏"
artifact_eq "$TID_A" "alpha.txt" "甲-$STAMP
" "a 的产物字节逐字一致"
artifact_eq "$TID_C" "final.txt" "甲-$STAMP
乙-$STAMP
" "c 的汇合产物字节逐字一致"
# 结果引用导出进原项目（对象进仓，主分支与工作树不动）；发布后 main 才 ff 前进。
eq "c 的结果引用已导出到原项目" "1" "$(git -C "$PROJ" rev-parse --verify -q "refs/tako/results/$TID_C/1" >/dev/null 2>&1 && echo 1 || echo 0)"
NEWHEAD=$(git -C "$PROJ" rev-parse HEAD)
[ "$NEWHEAD" != "$SEED" ] && ok "发布后 main 前进了（$NEWHEAD）" || no "发布后 main 没前进"
git -C "$PROJ" merge-base --is-ancestor "$SEED" "$NEWHEAD" && ok "seed 是新 HEAD 的祖先（ff，不是重写）" || no "main 历史被重写"
eq "发布后项目 final.txt 字节" "甲-$STAMP
乙-$STAMP" "$(cat "$PROJ/final.txt")"
eq "发布后项目 report.md 未被碰" "基线内容" "$(cat "$PROJ/report.md")"
for k in a b c; do
  T=$(echo "$AT_ORCH" | jq -r ".tasks[] | select(.key==\"$k\") | .taskId")
  eq "$k 恰好一场执行 run" "1" "$(ml_sql "SELECT count(*) FROM runs WHERE session_id=(SELECT execution_session_id FROM agent_tasks WHERE id='$T')")"
done
if at_wait_delivered "$OID" 180; then ok "场1 交付完成"; else no "场1 没交付"; ml_summary; fi
eq "场1 来源会话恰好两场 run（探针 + 交付）" "2" "$(at_session_runs "$SID")"

# ── 2. 父冲突：a/b 改同一文件不同内容 → c 裁决 ───────────────────────────────────
STAMP2="$(date +%s)-$$"
TASKS=$(jq -cn --arg a "把 shared.txt 的内容改成恰好一行「甲改-$STAMP2」。git add 并 git commit，然后调用 submit_task_result，artifacts 声明 shared.txt（kind=file）。" \
  --arg b "把 shared.txt 的内容改成恰好一行「乙改-$STAMP2」。git add 并 git commit，然后调用 submit_task_result，artifacts 声明 shared.txt（kind=file）。" \
  --arg c "合并上游结果时 shared.txt 会有冲突（工作区里有冲突标记）。裁决：把 shared.txt 改成恰好两行，第一行「甲改-$STAMP2」、第二行「乙改-$STAMP2」，去掉所有冲突标记。git add 并 git commit（完成合并提交），然后调用 submit_task_result，artifacts 声明 shared.txt（kind=file）。" \
  --arg g "$AGENT" \
  '[{key:"a",content:$a,agent:$g},{key:"b",content:$b,agent:$g},{key:"c",content:$c,agent:$g}]')
RESP=$(at_create_orch "acc-wsgit-conf-$STAMP2" "$WID" "$SID" "两个父任务改同一文件，c 裁决冲突" "$TASKS" \
  '[{"parent":"a","child":"c"},{"parent":"b","child":"c"}]' "isolated")
OID2=$(echo "$RESP" | jq -r '.data.orchestrationId // ""')
[ -n "$OID2" ] && [ "$OID2" != "null" ] || { echo "创建失败：$RESP" >&2; exit 2; }
at_orch "$OID2"
TID_C2=$(echo "$AT_ORCH" | jq -r '.tasks[] | select(.key=="c") | .taskId')
echo "  场2 orchId=$OID2 c=$TID_C2"
if at_wait_orch "$OID2" "succeeded" 300; then ok "场2 整组收口 succeeded"; else no "场2 没收口（$AT_LAST）"; at_orch "$OID2"; echo "$AT_ORCH" | jq -c '.tasks[] | {key,status,error}'; ml_summary; fi
GD2=$(ws_dir "$OID2")
# 冲突是程序检出的（mergeParents 写冲突记录），不是模型自述。
[ -s "$GD2/provider/conflicts/$TID_C2.json" ] && ok "c 的冲突记录在组目录（程序检出）" || no "c 没有冲突记录（父冲突没被程序发现）"
artifact_eq "$TID_C2" "shared.txt" "甲改-$STAMP2
乙改-$STAMP2
" "c 裁决后的产物字节逐字一致"
eq "发布后项目 shared.txt 是裁决结果" "甲改-$STAMP2
乙改-$STAMP2" "$(cat "$PROJ/shared.txt")"
if at_wait_delivered "$OID2" 180; then ok "场2 交付完成"; else no "场2 没交付"; ml_summary; fi

# ── 3. 验证返工：verifyCommand 先拒后过 ──────────────────────────────────────────
STAMP3="$(date +%s)-$$"
CONTENT="先创建 draft.txt，内容恰好一行「草稿-$STAMP3」，git add 并 git commit，然后调用 submit_task_result（summary 写「草稿」，artifacts 声明 draft.txt）。这次提交会被验证拒绝（pass.txt 还不存在）——这是预期的。被拒绝后创建 pass.txt，内容恰好一行「通过」，git add 并 git commit，再次调用 submit_task_result（summary 写「完成」，artifacts 声明 draft.txt 和 pass.txt 两个）。"
RESP=$(at_create_task "acc-wsgit-verify-$STAMP3" "$WID" "$SID" "$CONTENT" "$AGENT" "isolated" 'test -f pass.txt')
TID3=$(echo "$RESP" | jq -r '.data.taskId // ""')
OID3=$(echo "$RESP" | jq -r '.data.orchestrationId // ""')
[ -n "$TID3" ] && [ "$TID3" != "null" ] || { echo "创建失败：$RESP" >&2; exit 2; }
echo "  场3 taskId=$TID3 orchId=$OID3"
if at_wait_task "$TID3" "succeeded" 300; then ok "场3 验证返工后 succeeded"; else no "场3 没到 succeeded（$AT_LAST）"; at_task "$TID3"; echo "$AT_TASK" | jq -c '{status,error,workspace}'; ml_summary; fi
eq "验证证据 passed" "passed" "$(task_field "$TID3" '.workspace.verification.status')"
eq "验证退出码 0" "0" "$(task_field "$TID3" '.workspace.verification.exitCode')"
eq "验证输出位置" "verification/$TID3.out" "$(task_field "$TID3" '.workspace.verification.outputPath')"
eq "被拒的提交不落版本（resultRevision=1）" "1" "$(task_field "$TID3" '.resultRevision')"
# 帧里看得见两次 submit_task_result：第一次被验证拒绝（错误：…验证命令未通过），第二次成功。
RID=$(task_field "$TID3" '.currentRunId')
ml_frames
SUBN=$(printf '%s' "$FRAMES" | jq -s '[.[].data.frames[]? | select(.kind=="tool_call" and .name=="submit_task_result")] | length')
eq "执行帧里恰好两次 submit_task_result（先拒后过）" "2" "$SUBN"
FIRST_OUT=$(printf '%s' "$FRAMES" | jq -s '[.[].data.frames[]? | select(.kind=="tool_result" and .name=="submit_task_result")] | .[0].output // ""')
case "$FIRST_OUT" in *"验证命令未通过"*) ok "第一次提交被验证程序拒绝（帧里可见）";; *) no "第一次提交没有验证拒绝痕迹（$FIRST_OUT）";; esac
artifact_eq "$TID3" "pass.txt" "通过
" "返工后的产物字节逐字一致"
if at_wait_delivered "$OID3" 180; then ok "场3 交付完成"; else no "场3 没交付"; ml_summary; fi

# ── 4. 主分支前进：发布受阻 workspace_changed → 人工重新发布 → merge 新基线 ────────
STAMP4="$(date +%s)-$$"
CONTENT4="把 report.md 的内容改成恰好一行「结果-$STAMP4」。git add 并 git commit，然后调用 submit_task_result，summary 写「完成」，artifacts 声明 report.md（kind=file）。"
RESP=$(at_create_task "acc-wsgit-adv-$STAMP4" "$WID" "$SID" "$CONTENT4" "$AGENT" "isolated")
TID4=$(echo "$RESP" | jq -r '.data.taskId // ""')
OID4=$(echo "$RESP" | jq -r '.data.orchestrationId // ""')
[ -n "$TID4" ] && [ "$TID4" != "null" ] || { echo "创建失败：$RESP" >&2; exit 2; }
GD4=$(ws_dir "$OID4")
echo "  场4 taskId=$TID4 orchId=$OID4"
# 组目录一出现（准备完成、执行刚开始）就在原项目 main 上推一个用户提交。
for i in $(seq 1 60); do [ -f "$GD4/state.json" ] && break; sleep 1; done
[ -f "$GD4/state.json" ] || { no "60s 没等到组准备"; ml_summary; }
printf '用户前进\n' > "$PROJ/user.txt"
git -C "$PROJ" add user.txt
git -C "$PROJ" -c user.name=probe -c user.email=probe@tako.test commit -qm user-advance
ADV=$(git -C "$PROJ" rev-parse HEAD)
echo "  原项目 main 被用户推进到 $ADV"
if at_wait_task "$TID4" "blocked" 300; then ok "场4 发布受阻 blocked"; else no "场4 没到 blocked（$AT_LAST）"; at_task "$TID4"; echo "$AT_TASK" | jq -c '{status,error,workspace}'; ml_summary; fi
eq "受阻原因 workspace_changed" "workspace_changed" "$(task_field "$TID4" '.workspace.publication.errorCode')"
eq "发布状态 blocked" "blocked" "$(task_field "$TID4" '.workspace.publication.status')"
eq "受阻时项目 report.md 没被覆盖" "基线内容" "$(cat "$PROJ/report.md")"
eq "新基线已带入本组仓库 refs/tako/base" "$ADV" "$(git --git-dir="$GD4/provider/repo.git" rev-parse refs/tako/base)"
eq "受阻时结果已固化（resultRevision=1）" "1" "$(task_field "$TID4" '.resultRevision')"
# 人工重新发布：202 铸处理 run，epoch+1。
RES=$(curl -sS -X POST "$B/api/tasks/$TID4/retry-publication" -H "Authorization: Bearer $TOK")
eq "retry-publication 202" "0" "$(echo "$RES" | jq -r .code)"
eq "publicationEpoch 涨到 2" "2" "$(echo "$RES" | jq -r '.data.publicationEpoch // 0')"
RETRY_RUN=$(echo "$RES" | jq -r '.data.runId // ""')
[ -n "$RETRY_RUN" ] && ok "处理 run 铸了（$RETRY_RUN）" || no "retry 没回 runId"
if at_wait_task "$TID4" "succeeded" 300; then ok "场4 重新发布后 succeeded"; else no "场4 重新发布没到 succeeded（$AT_LAST）"; at_task "$TID4"; echo "$AT_TASK" | jq -c '{status,error,workspace}'; ml_summary; fi
eq "重新提交生成新版本（resultRevision=2）" "2" "$(task_field "$TID4" '.resultRevision')"
eq "发布状态 published" "published" "$(task_field "$TID4" '.workspace.publication.status')"
eq "发布后项目 report.md 是结果内容" "结果-$STAMP4" "$(cat "$PROJ/report.md")"
eq "用户前进的 user.txt 还在（没被旧结果覆盖）" "用户前进" "$(cat "$PROJ/user.txt")"
git -C "$PROJ" merge-base --is-ancestor "$ADV" "$(git -C "$PROJ" rev-parse HEAD)" && ok "用户提交仍是新 HEAD 的祖先（merge 了新基线）" || no "用户提交被丢弃"
eq "场4 恰好两场执行 run（首次 + 处理）" "2" "$(ml_sql "SELECT count(*) FROM runs WHERE session_id=(SELECT execution_session_id FROM agent_tasks WHERE id='$TID4')")"
if at_wait_delivered "$OID4" 180; then ok "场4 交付完成"; else no "场4 没交付"; ml_summary; fi

# ── 5. 不可变产物：发布后原项目再前进，artifact 仍回结果版本字节 ───────────────────
printf '发布后又改\n' > "$PROJ/report.md"
git -C "$PROJ" add report.md
git -C "$PROJ" -c user.name=probe -c user.email=probe@tako.test commit -qm post-publish
artifact_eq "$TID4" "report.md" "结果-$STAMP4
" "发布后项目再前进，artifact 仍回结果版本字节（不可变）"
eq "项目当前文件已是新内容（与产物两份）" "发布后又改" "$(cat "$PROJ/report.md")"

ml_summary