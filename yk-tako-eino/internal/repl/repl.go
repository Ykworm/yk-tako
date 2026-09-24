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
