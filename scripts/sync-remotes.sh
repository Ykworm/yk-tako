#!/usr/bin/env bash
# 先推主仓 GitHub（origin），成功后再推镜像 AtomGit（atomgit）。AtomGit 永远不领先。
set -euo pipefail

branch="${1:-main}"

git push --follow-tags origin "$branch"
git push --follow-tags atomgit "$branch"
