// 本文件：摘要提示词，以及写进 user_message 的拼接。
package compact

import "strings"

// summaryPrompt 是唯一的摘要提示词。不按会话类型换模板。
const summaryPrompt = `你在给一段已经结束的对话做摘要，供后面的模型接着用。
按这段对话实际在谈的事写。能抽到的才写，没有的栏目不要编，也不要留空标题。
可以写的栏目：已知事实、关键决策、未解决问题、用户偏好。
不要复述全文。`

// SummaryHeading 标出摘要正文。模型看到的摘要消息和写进 user_message 的前缀用同一句。
const SummaryHeading = "【上下文摘要】"

// ragHeading 标出这一次压缩检索回来的旧对话。只出现在当次发送的消息里。
const ragHeading = "【检索到的旧对话】"

// CombineUser 把摘要接在用户原话前面。摘要为空时保持原话，避免多出一个空标题。
func CombineUser(summary, userText string) string {
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return userText
	}
	return SummaryHeading + "\n" + summary + "\n\n" + userText
}
