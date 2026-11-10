package runengine

// 本文件是「记忆模式」的开关层（方案真源 = doc/65 §0 / §5；施工包 §3.1 / §5.1）。
//
// 两种模式不是同一套接口的两个实现，是两套互不混装的机制：
//
//	pipeline：PG memory_items + 后台抽取器 + 每回合 BM25 召回 + search_memory
//	          （memory.go / memory_pipeline.go / memory_search.go；常驻段渲染在 notes.go）
//	markdown：两份纯文本文件（USER.md / MEMORY.md）+ master 同步条目工具
//	          （memory_markdown.go）
//
// 模式是**实例级**的（TAKO_MEMORY_MODE）：USER.md 跨项目可见，按 workspace 切换会让
// 「同一个人在不同项目里看到两份不同的 USER.md」——所以只允许整实例一个模式，
// 不允许不同租户各选一个。
//
// 装配纪律（doc/65 §0 表末段）：markdown 模式**只装配文件工具**。抽取消费者、
// memory_jobs、postrun、每回合 BM25 召回、search_memory 全都不启动——
// 不能在配置切过去以后还留着旧机制继续写。

import (
	"errors"
	"fmt"
	"strings"
)

// MemoryMode 是记忆的运行模式。
type MemoryMode string

const (
	// MemoryModePipeline：完整管线（默认）。
	MemoryModePipeline MemoryMode = "pipeline"
	// MemoryModeMarkdown：同步文件。
	MemoryModeMarkdown MemoryMode = "markdown"
)

// String 让 %s / %v 打出模式名而不是引号。
func (m MemoryMode) String() string { return string(m) }

// ParseMemoryMode 解析 TAKO_MEMORY_MODE 的取值。
//
// 空 = pipeline（默认）；非法值 = 错（调用方在启动时 log.Fatalf——不带病启动，
// 免得「以为在 markdown 模式、其实还在跑抽取器」这种半切换状态活到线上）。
func ParseMemoryMode(raw string) (MemoryMode, error) {
	switch v := strings.ToLower(strings.TrimSpace(raw)); v {
	case "":
		return MemoryModePipeline, nil
	case string(MemoryModePipeline):
		return MemoryModePipeline, nil
	case string(MemoryModeMarkdown):
		return MemoryModeMarkdown, nil
	default:
		return "", fmt.Errorf("TAKO_MEMORY_MODE=%q 不是合法取值（只能 pipeline 或 markdown）", raw)
	}
}

// ErrMemoryModeUnsupported 是「当前模式不提供这个能力」的哨兵错误。
//
// markdown 模式没有 PG 记忆表、没有召回、没有 search_memory。观测口（items / jobs）
// 在这个模式下返回它，**不返回空数组假装文件里没有记忆**——空数组会被读成
// 「这个租户一条记忆都没有」，而真相是「记忆在文件里，观测口看不见」。
//
// HTTP 状态与业务码的映射（409 / 118001）在 server.go 的 memoryPipelineOnly；
// 这一层只给一个能被 errors.Is 认出来的哨兵，别让每个调用方各写一句文案。
var ErrMemoryModeUnsupported = errors.New("当前记忆模式不提供这个观测口")

// memoryMode 取当前 Store 的记忆模式。
//
// 空配置 = pipeline：单测不调 ConfigureMemory 时按 pipeline 的 PG 表读写。
// 认不出来的值也当 pipeline——启动期已经 fail fast 过一道，这里只是兜底，
// 不让一个脏值把整场 run 变成「没有记忆工具也没有抽取器」。
func (s *Store) memoryMode() MemoryMode {
	if s == nil {
		return MemoryModePipeline
	}
	if m := MemoryMode(strings.TrimSpace(string(s.memoryCfg.Mode))); m == MemoryModeMarkdown {
		return MemoryModeMarkdown
	}
	return MemoryModePipeline
}

// markdownMode 报「现在是 markdown 模式」。
func (s *Store) markdownMode() bool { return s.memoryMode() == MemoryModeMarkdown }
