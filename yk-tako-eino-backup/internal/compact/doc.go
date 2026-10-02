// Package compact 在主模型每次 Generate 之前压缩过长的对话。
//
// 算法见仓库 doc/12-上下文压缩.md 第 9 节。压缩是一次摘要 Generate，
// 不是第二个 ChatModelAgent。摘要不进入根 agent 的 MessageOutput。
package compact
