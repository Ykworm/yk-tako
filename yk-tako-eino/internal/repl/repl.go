package repl

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

// Console 把用户输入和审批提问放在同一个 reader 上，避免审批的 y 被下一轮吃掉。
type Console struct {
	in *bufio.Reader
}

func New() *Console {
	return &Console{in: bufio.NewReader(os.Stdin)}
}

// ReadLine 读一行。EOF 时 ok 为 false，管道输入可以干净退出。
func (c *Console) ReadLine(prompt string) (string, bool) {
	fmt.Fprint(os.Stdout, prompt)
	line, err := c.in.ReadString('\n')
	if err != nil && err != io.EOF {
		fmt.Fprintln(os.Stderr, "读输入失败:", err)
		return "", false
	}
	line = strings.TrimSpace(line)
	if err == io.EOF && line == "" {
		return "", false
	}
	return line, true
}

// Clip 把多行文本压成一行，再按 rune 截断。工具回执的单行预览用它。
func Clip(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

// Exitf 打印错误并退出。只在各脚本的 main 初始化阶段用。
func Exitf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

// Indent 给每行加统一前导空格。任务清单的缩进打印用它。
func Indent(s string) string {
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		lines[i] = "     " + line
	}
	return strings.Join(lines, "\n")
}
