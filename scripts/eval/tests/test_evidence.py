#!/usr/bin/env python3
"""evidence：工具配对、read_events、citations、主答案核对、联网扫描。"""
import json
import sys
import unittest
from pathlib import Path

EVAL_DIR = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(EVAL_DIR))

import casefile  # noqa: E402
import evidence as ev  # noqa: E402

CASE = casefile.load_case(EVAL_DIR / "cases" / "doc-verify.json")
FILES = CASE.files
FIXTURE_PATHS = CASE.fixture_paths
FIX_DIR = Path(__file__).resolve().parent / "fixtures"


def row(offset, run_id, role, agent, payload, text=""):
    return {"runlogOffset": offset, "runId": run_id, "role": role, "agent": agent,
            "model": "deepseek/deepseek-flash", "text": text, "payload": json.dumps(payload)}


def load(name):
    return json.loads((FIX_DIR / name).read_bytes().decode("utf-8"))


class TestPairing(unittest.TestCase):
    def test_same_toolcallid_different_agents_do_not_mix(self):
        rows = load("runlog-sample.json")
        pairs, unpaired = ev.pair_tool_events(rows)
        self.assertEqual(len(pairs), 2)
        by_agent = {(c[0].get("agent") or ""): c for c in pairs}
        self.assertEqual(set(by_agent), {"", "researcher"})
        root_pair = by_agent[""]
        self.assertEqual(root_pair[2]["runlogOffset"], 3)
        sub_pair = by_agent["researcher"]
        self.assertEqual(sub_pair[2]["runlogOffset"], 5)

    def test_unpaired_call_and_result(self):
        rows = [row(1, "r1", "tool_call", "", {"name": "read_file", "args": "{}", "toolCallId": "c1"}),
                row(2, "r1", "tool_result", "", {"name": "read_file", "output": "x", "toolCallId": "c2"})]
        pairs, unpaired = ev.pair_tool_events(rows)
        self.assertEqual(pairs, [])
        self.assertEqual(len(unpaired), 2)

    def test_missing_toolcallid_unpairable(self):
        rows = [row(1, "r1", "tool_call", "", {"name": "read_file", "args": "{}"})]
        pairs, unpaired = ev.pair_tool_events(rows)
        self.assertEqual(len(unpaired), 1)


class TestReadEvents(unittest.TestCase):
    def events_from(self, tool, args, output, agent=""):
        rows = [row(1, "r1", "tool_call", agent,
                    {"name": tool, "args": json.dumps(args, ensure_ascii=False), "toolCallId": "c1"}),
                row(2, "r1", "tool_result", agent,
                    {"name": tool, "output": output, "toolCallId": "c1"})]
        pairs, _ = ev.pair_tool_events(rows)
        return ev.extract_read_events(pairs, {"r1": 1}, FIXTURE_PATHS)

    def test_read_file_ok(self):
        events, unparsed = self.events_from("read_file", {"path": "决策-方案B.md"}, "正文内容")
        self.assertEqual(len(events), 1)
        self.assertEqual(events[0]["path"], "决策-方案B.md")
        self.assertEqual(unparsed, [])

    def test_read_file_error_not_access(self):
        events, unparsed = self.events_from("read_file", {"path": "决策-方案B.md"}, "错误：读不到")
        self.assertEqual(events, [])
        self.assertEqual(len(unparsed), 1)

    def test_read_file_footer_only_not_access(self):
        events, _ = self.events_from("read_file", {"path": "决策-方案B.md"},
                                     "[已省略 100 行]\n（第 1/5 页）")
        self.assertEqual(events, [])

    def test_read_file_dot_slash_normalized(self):
        events, _ = self.events_from("read_file", {"path": "./决策-方案B.md"}, "正文")
        self.assertEqual(events[0]["path"], "决策-方案B.md")

    def test_read_file_name_without_md(self):
        events, _ = self.events_from("read_text_file", {"path": "决策-方案B"}, "正文")
        self.assertEqual(events[0]["path"], "决策-方案B.md")

    def test_search_files_multi_hit(self):
        out = "决策-方案B.md:9:1. 维护成本翻倍……\n事故记录-0620.md:6:方案 B ……\n共 2 条命中，已省略 0 条"
        events, _ = self.events_from("search_files", {"query": "维护"}, out)
        self.assertEqual({e["path"] for e in events},
                         {"决策-方案B.md", "事故记录-0620.md"})
        self.assertTrue(all(e["tool"] == "search_files" for e in events))

    def test_search_files_no_hit(self):
        events, _ = self.events_from("search_files", {"query": "zzz"}, "无命中")
        self.assertEqual(events, [])

    def test_bash_grep_literal_ok(self):
        events, unparsed = self.events_from("bash", {"command": "grep 维护 决策-方案B.md"},
                                            "1. 维护成本翻倍：需要两套存储并行。")
        self.assertEqual(len(events), 1)
        self.assertEqual(events[0]["path"], "决策-方案B.md")
        self.assertEqual(unparsed, [])

    def test_bash_rg_literal_ok(self):
        events, _ = self.events_from("bash", {"command": "rg 维护 决策-方案B.md"}, "9:1. 维护成本翻倍……")
        self.assertEqual(len(events), 1)

    def test_bash_pipe_unparsed(self):
        events, unparsed = self.events_from("bash", {"command": "cat 决策-方案B.md | head -5"}, "正文")
        self.assertEqual(events, [])
        self.assertEqual(len(unparsed), 1)

    def test_bash_multi_path_unparsed(self):
        events, unparsed = self.events_from(
            "bash", {"command": "grep 维护 决策-方案B.md 事故记录-0620.md"}, "x")
        self.assertEqual(events, [])
        self.assertEqual(len(unparsed), 1)

    def test_directory_listing_not_access(self):
        events, _ = self.events_from("read_file", {"path": "content"}, "决策-方案B.md\n周报-0715.md")
        # 目录列表：没有 .md 目标正文、path 也不是文件 → 不产生访问
        self.assertEqual(events, [])

    def test_task_create_only_counts(self):
        events, _ = self.events_from("TaskCreate", {"subject": "查文档"}, "任务已建")
        self.assertEqual(events, [])


class TestCitations(unittest.TestCase):
    def cite(self, text, rnd=1):
        return ev.extract_citations(text, rnd, f"r{rnd}", FILES)

    def test_plain_path_with_section(self):
        out = self.cite("理由见 决策-方案B.md §2。")
        self.assertEqual(len(out), 1)
        self.assertEqual(out[0]["path"], "决策-方案B.md")
        self.assertEqual(out[0]["section"], 2)
        self.assertIsNone(out[0]["parseError"])

    def test_markdown_link_anchor(self):
        out = self.cite("见 [否决理由](决策-方案B.md#2-否决理由)。")
        self.assertEqual(out[0]["section"], 2)
        self.assertEqual(out[0]["path"], "决策-方案B.md")

    def test_link_L_fragment_is_line_not_section(self):
        out = self.cite("见 [x](决策-方案B.md#L10)。")
        self.assertEqual(out[0]["line"], 10)
        self.assertIsNone(out[0]["section"])

    def test_section_variants(self):
        for text in ("见 决策-方案B.md 第2节。", "见 决策-方案B.md 第 2 节。",
                     "见 决策-方案B.md 的 ## 2. 小节。"):
            out = self.cite(text)
            self.assertEqual(out[0]["section"], 2, text)

    def test_line_ref_colon(self):
        out = self.cite("见 决策-方案B.md:9。")
        self.assertEqual(out[0]["line"], 9)

    def test_unknown_md_kept_for_b1(self):
        out = self.cite("依据是 不存在的文件.md。")
        self.assertEqual(out[0]["path"], "不存在的文件.md")
        self.assertIsNone(out[0]["parseError"])

    def test_unknown_fragment_parse_error(self):
        out = self.cite("见 [x](决策-方案B.md#nonsense)。")
        self.assertEqual(out[0]["parseError"], "unsupported-fragment")

    def test_parent_escape_out_of_bounds(self):
        out = self.cite("见 [x](../secrets/决策-方案B.md)。")
        self.assertTrue(out[0]["outOfBounds"])
        self.assertEqual(out[0]["parseError"], None)
        self.assertNotEqual(out[0]["path"], "决策-方案B.md")

    def test_url_decoded_once(self):
        out = self.cite("见 [x](%E5%86%B3%E7%AD%96-%E6%96%B9%E6%A1%88B.md)。")
        self.assertEqual(out[0]["path"], "决策-方案B.md")

    def test_generic_name_not_guessed(self):
        out = self.cite("决策记录里写了理由。")
        self.assertEqual(out, [])

    def test_absolute_in_trial_workspace(self):
        out = ev.extract_citations("见 /ws/tenants/t1/workspaces/w1/content/决策-方案B.md。",
                                   1, "r1", FILES,
                                   abs_prefixes=("/ws/tenants/t1/workspaces/w1/content",))
        self.assertEqual(out[0]["path"], "决策-方案B.md")
        self.assertFalse(out[0]["outOfBounds"])

    def test_bare_section_no_file_ignored(self):
        out = self.cite("§2 写了理由。")
        self.assertEqual(out, [])


class TestMainAnswer(unittest.TestCase):
    def test_main_answer_row_picks_root_only(self):
        rows = load("runlog-sample.json")
        row, problem = ev.main_answer_row(rows, "r1")
        self.assertIsNone(problem)
        self.assertTrue(row["text"].startswith("主答案"))

    def test_subagent_final_not_main_answer(self):
        rows = [r for r in load("runlog-sample.json") if r["runlogOffset"] != 7]
        row, problem = ev.main_answer_row(rows, "r1")
        self.assertEqual(problem, "missing")

    def test_ambiguous(self):
        rows = load("runlog-sample.json")
        dup = dict(rows[-1])
        dup["runlogOffset"] = 8
        rows.append(dup)
        row, problem = ev.main_answer_row(rows, "r1")
        self.assertEqual(problem, "ambiguous")

    def test_root_delta_concat(self):
        frames = load("frames-sample.json")
        text = ev.root_delta_text(frames, "r1")
        self.assertEqual(text, "主答案：理由见 决策-方案B.md §2。")

    def test_compare_answer_mismatch(self):
        frames = load("frames-sample.json")
        rows = load("runlog-sample.json")
        ans = [r for r in rows if r["role"] == "assistant" and r["agent"] == ""][-1]
        cmp = ev.compare_answer(ans, frames, "r1")
        self.assertTrue(cmp["ok"])
        bad = dict(ans)
        bad["text"] = "不一样"
        cmp = ev.compare_answer(bad, frames, "r1")
        self.assertFalse(cmp["ok"])
        self.assertEqual(cmp["reason"], "answer-bytes-mismatch")


class TestNetworkViolations(unittest.TestCase):
    def test_web_search_flagged(self):
        rows = [row(1, "r1", "tool_call", "", {"name": "web_search", "args": "{}", "toolCallId": "c1"}),
                row(2, "r1", "tool_result", "", {"name": "web_search", "output": "…", "toolCallId": "c1"})]
        pairs, _ = ev.pair_tool_events(rows)
        vios = ev.network_violations(pairs)
        self.assertEqual(len(vios), 1)

    def test_bash_wget_flagged(self):
        rows = [row(1, "r1", "tool_call", "",
                    {"name": "bash", "args": json.dumps({"command": "wget https://x.com/a"}), "toolCallId": "c1"}),
                row(2, "r1", "tool_result", "", {"name": "bash", "output": "…", "toolCallId": "c1"})]
        pairs, _ = ev.pair_tool_events(rows)
        vios = ev.network_violations(pairs)
        self.assertEqual(len(vios), 1)

    def test_clean_tools_no_violation(self):
        rows = load("runlog-sample.json")
        pairs, _ = ev.pair_tool_events(rows)
        self.assertEqual(ev.network_violations(pairs), [])


if __name__ == "__main__":
    unittest.main()
