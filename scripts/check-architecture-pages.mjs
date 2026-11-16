#!/usr/bin/env node

import { spawnSync } from "node:child_process";
import { existsSync, readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const ROOT = fileURLToPath(new URL("../", import.meta.url));
const ASSETS = join(ROOT, "doc", "assets");
const ENGINE = join(ROOT, "yk-tako-eino");
const PAGES = new Map([
  ["overview", "tako-architecture.html"],
  ["run", "tako-run-lifecycle.html"],
  ["orchestration", "tako-orchestration.html"],
  ["data", "tako-data-model.html"]
]);
const errors = [];
const stats = { nodes: 0, edges: 0, links: 0, code: 0, docs: 0 };
const fail = (file, message) => errors.push(`${file}: ${message}`);
const read = path => readFileSync(path, "utf8");
const localTarget = (file, href) => {
  const [pathPart, anchor] = href.split("#", 2);
  const target = pathPart ? resolve(dirname(file), decodeURIComponent(pathPart)) : file;
  return { target, anchor };
};
const checkLocalLink = (file, href, source = "HTML") => {
  if (!href || !href.trim()) {
    fail(file, `${source} 有空链接`);
    return;
  }
  if (/^(?:https?:|mailto:|data:|javascript:)/i.test(href)) return;
  const { target, anchor } = localTarget(file, href);
  if (!existsSync(target)) {
    fail(file, `${source} 目标不存在：${href}`);
    return;
  }
  stats.links += 1;
  if (anchor && target.endsWith(".html")) {
    const html = read(target);
    if (!new RegExp(`\\bid=["']${anchor.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}["']`).test(html)) {
      fail(file, `${source} 锚点不存在：${href}`);
    }
  }
};
const uniqueIDs = (file, values, kind) => {
  const seen = new Set();
  values.forEach(value => {
    if (!value) fail(file, `${kind} 存在空 ID`);
    if (seen.has(value)) fail(file, `${kind} ID 重复：${value}`);
    seen.add(value);
  });
  return seen;
};

for (const [page, name] of PAGES) {
  const file = join(ASSETS, name);
  if (!existsSync(file)) {
    fail(name, "页面不存在");
    continue;
  }
  const html = read(file);
  if (!new RegExp(`<body[^>]+data-page=["']${page}["']`).test(html)) fail(name, `body data-page 应为 ${page}`);
  const blocks = [...html.matchAll(/<script\s+type=["']application\/json["']\s+id=["']page-data["']>([\s\S]*?)<\/script>/g)];
  if (blocks.length !== 1) {
    fail(name, `应有且仅有一个 page-data JSON，实际 ${blocks.length}`);
    continue;
  }
  let data;
  try {
    data = JSON.parse(blocks[0][1]);
  } catch (error) {
    fail(name, `page-data 不是合法 JSON：${error.message}`);
    continue;
  }
  if (!Array.isArray(data.viewBox) || data.viewBox.length !== 4 || data.viewBox.some(value => !Number.isFinite(value))) {
    fail(name, "viewBox 必须是四个有限数字");
  }
  for (const [otherPage, otherName] of PAGES) {
    if (otherPage === page) continue;
    const navPattern = new RegExp(`<nav[^>]+class=["']page-nav["'][\\s\\S]*?href=["']${otherName}["']`);
    if (!navPattern.test(html)) fail(name, `导航缺少 ${otherName}`);
  }
  const ids = [...html.matchAll(/\bid=["']([^"']+)["']/g)].map(match => match[1]);
  uniqueIDs(name, ids, "HTML");
  for (const match of html.matchAll(/\b(?:href|src)=["']([^"']*)["']/g)) checkLocalLink(file, match[1]);
  for (const match of html.matchAll(/\b(?:href|src)=["'](https?:\/\/[^"']+)["']/gi)) {
    fail(name, `存在外部运行时资源：${match[1]}`);
  }

  const nodeIDs = uniqueIDs(name, (data.nodes || []).map(node => node.id), "节点");
  const edgeIDs = uniqueIDs(name, (data.edges || []).map(edge => edge.id), "连线");
  stats.nodes += nodeIDs.size;
  stats.edges += edgeIDs.size;
  const edgeSet = new Set((data.edges || []).map(edge => edge.id));
  for (const edge of data.edges || []) {
    if (!nodeIDs.has(edge.from)) fail(name, `${edge.id} 的起点不存在：${edge.from}`);
    if (!nodeIDs.has(edge.to)) fail(name, `${edge.id} 的终点不存在：${edge.to}`);
    if (!edge.label || !edge.description) fail(name, `${edge.id} 缺少说明`);
  }
  for (const step of data.steps || []) {
    if (!edgeSet.has(step.edge)) fail(name, `步骤“${step.title}”引用不存在的连线：${step.edge}`);
  }
  for (const node of data.nodes || []) {
    if (!node.title || !node.summary) fail(name, `${node.id} 缺少标题或职责说明`);
    for (const codePath of node.code || []) {
      stats.code += 1;
      if (!existsSync(join(ENGINE, codePath))) fail(name, `${node.id} 的代码路径不存在：${codePath}`);
    }
    for (const doc of node.docs || []) {
      stats.docs += 1;
      const docPath = typeof doc === "string" ? doc : doc.path;
      if (!docPath || !existsSync(join(ROOT, "doc", docPath))) fail(name, `${node.id} 的文档路径不存在：${docPath}`);
    }
  }
  for (const section of data.sections || []) {
    if (section.type === "table") {
      for (const row of section.rows || []) {
        if (row.length !== section.columns.length) fail(name, `“${section.title}”有行列数不一致`);
      }
    }
    for (const item of section.items || []) {
      for (const link of item.links || []) checkLocalLink(file, link.href, `章节“${section.title}”`);
    }
  }

  const raw = blocks[0][1];
  const mustContain = (values, reason) => values.forEach(value => {
    if (!raw.includes(value)) fail(name, `缺少${reason}：${value}`);
  });
  if (page === "run") {
    mustContain(["start", "delta", "tool_call", "tool_result", "usage", "task", "question", "resume", "terminal"], "帧 kind");
    mustContain(["runlog_entries", "run_model_messages", "run_journal", "run_questions", "run_tasks"], "run 记录");
  }
  if (page === "orchestration") {
    mustContain(["bash_output", "wait_task", "shared", "isolated", "goalAssessment.outcome == feasible", "claimed_in_place=true", "journey"], "编排机制");
  }
  if (page === "data") {
    mustContain(["tk:runlive:<runId>", "tk:runepoch:<runId>", "tk:events:<tid>", "tk:agent-task-wake", "Redis Stream", "content/", "runtime/", "USER.md", "MEMORY.md"], "存储机制");
  }
  const stalePatterns = [
    [/Redis\s+Pub\/Sub/i, "把 Redis 事件写成 Pub/Sub"],
    [/目标不可行时[^。]{0,30}(?:建|创建)继任/, "把不可行目标写成创建继任组"],
    [/每个\s*Task\s*领一份独立工作区/, "把所有 Task 写成独立工作区"],
    [/memory_items[^\n]{0,80}dialogue_chunks[^\n]{0,40}notes/, "把 notes 列入现行记忆表"],
    [/工具根里的东西[^\n]{0,40}进入本场 run 的工具池/, "把工具根写成模型工具池"],
    [/每秒一轮|按秒扫/, "把调度写成每秒扫描"],
    [/只剩\s*(?:tk:runlive|两类临时|2\s*类)/, "把临时状态写成只剩两类"]
  ];
  for (const [pattern, reason] of stalePatterns) {
    if (pattern.test(raw)) fail(name, `发现旧口径：${reason}`);
  }
}

for (const name of ["tako-architecture.css", "tako-architecture.js"]) {
  const file = join(ASSETS, name);
  if (!existsSync(file)) {
    fail(name, "共享资源不存在");
    continue;
  }
  const text = read(file);
  if (/@import\s|url\(\s*["']?https?:\/\//i.test(text)) fail(name, "存在外部 CSS 资源");
  for (const file2 of PAGES.values()) checkLocalLink(file, file2, "共享资源导航检查");
}
for (const name of [...PAGES.values(), "tako-architecture.css", "tako-architecture.js"]) {
  const file = join(ASSETS, name);
  if (!existsSync(file)) continue;
  const text = read(file);
  if (/unsplash|picsum|placehold(?:\.co|er\.com)|via\.placeholder|dummyimage|lorem\.space|lorem ipsum/i.test(text)) {
    fail(name, "存在占位内容或占位图地址");
  }
}

const sharedJS = join(ASSETS, "tako-architecture.js");
if (existsSync(sharedJS)) {
  const syntax = spawnSync(process.execPath, ["--check", sharedJS], { encoding: "utf8" });
  if (syntax.status !== 0) fail("tako-architecture.js", `JavaScript 语法错误：${syntax.stderr.trim()}`);
}

if (errors.length) {
  console.error(`架构页检查失败（${errors.length} 项）：`);
  errors.forEach(error => console.error(`- ${error}`));
  process.exitCode = 1;
} else {
  console.log(`架构页检查通过：${PAGES.size} 页，${stats.nodes} 个节点，${stats.edges} 条连线，${stats.links} 个本地链接，${stats.code} 个代码引用，${stats.docs} 个文档引用。`);
}
