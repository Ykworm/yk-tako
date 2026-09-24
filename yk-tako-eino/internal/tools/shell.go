package tools

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
)

// bashArgs 是 bash 的参数。
type bashArgs struct {
	Command string `json:"command" jsonschema:"description=要执行的 shell 命令"`
}

// Bash 给 agent 真实的 shell。故意不做拦截，拦截是中间件的事。
func Bash(ctx context.Context) (tool.BaseTool, error) {
	return utils.InferTool("bash", "在宿主机 shell 中执行命令并返回输出。工作目录为进程当前目录，超时 10 秒。",
		func(ctx context.Context, args bashArgs) (string, error) {
			runCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(runCtx, "bash", "-lc", args.Command)
			cmd.Dir, _ = os.Getwd()
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			err := cmd.Run()
			out := joinOutput(stdout.String(), stderr.String())
			if err != nil {
				if out == "" {
					out = err.Error()
				} else {
					out = "命令执行失败: " + err.Error() + "\n" + out
				}
				return out, nil
			}
			if out == "" {
				return "(命令执行成功，无输出)", nil
			}
			return out, nil
		})
}

// joinOutput 合并 stdout 和 stderr，各自去掉首尾空白，空的就不占位置。
func joinOutput(stdout, stderr string) string {
	stdout = trimSpace(stdout)
	stderr = trimSpace(stderr)
	switch {
	case stdout != "" && stderr != "":
		return stdout + "\n" + stderr
	case stdout != "":
		return stdout
	default:
		return stderr
	}
}

// trimSpace 去掉首尾的空白字符（含换行和制表符）。
func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\n' || s[0] == '\t' || s[0] == '\r') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\n' || s[len(s)-1] == '\t' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
