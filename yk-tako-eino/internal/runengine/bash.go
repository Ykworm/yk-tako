// bash 执行工具（doc/17 §3）：researcher/coder「能写脚本拿不到结果」病灶的出口——
// 批量活先写脚本再跑。exe 是 exec.CommandContext 包 bash -c，作业根固定 workspace
// （模型给的 command 相对路径都在里面解析；不做符号链接逃逸、联网隔离、危险命令表——
// doc §3 明文本版不做）。错误经 wrapToolErrors 变「错误：…」回注，run 不因工具失败变 error。
package runengine

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
)

// bashOutputCap 是 stdout/stderr 各自的字节帽（doc/17 §3 推荐值 32 KB）：
// 超出截断、结果文本写明——引导模型把大输出落文件再读，而不是灌进上下文。
const bashOutputCap = 32 * 1024

// bashEnvWhitelist 是子进程环境白名单：引擎进程里可能有厂商 API Key 等密钥，
// tool_result 与 runlog 行是永久历史——env 全量透传 = 密钥进永久库。
var bashEnvWhitelist = []string{"PATH", "HOME", "TMPDIR", "LANG", "LC_ALL"}

// bashIn 是 bash 的入参 schema：由 InferTool 按 struct tag 反射生成。
type bashIn struct {
	Command string `json:"command" jsonschema:"description=要执行的 bash 命令，在 workspace 根目录下运行；无单次时长限制，stdout/stderr 各 32 KB 截断"`
}

// capWriter 包住 buf：帽内照收、帽外丢弃并置 truncated——大输出不进内存也不逼死管道对端。
type capWriter struct {
	buf        bytes.Buffer
	cap        int
	truncated  bool
}

func (w *capWriter) Write(p []byte) (int, error) {
	if w.buf.Len() >= w.cap {
		w.truncated = true
		return len(p), nil
	}
	room := w.cap - w.buf.Len()
	if len(p) > room {
		w.buf.Write(p[:room])
		w.truncated = true
		return len(p), nil
	}
	return w.buf.Write(p)
}

func (w *capWriter) truncatedNote(name string) string {
	if w.truncated {
		return fmt.Sprintf("\n（%s 超 32 KB 已截断——把需要的内容写进文件再读，不要直接重跑打印）", name)
	}
	return ""
}

// outputSections 拼装 stdout/stderr 两段，成功结果与被终止错误共用同形。
func outputSections(stdout, stderr *capWriter) string {
	out := fmt.Sprintf("stdout:\n%s%s", stdout.buf.String(), stdout.truncatedNote("stdout"))
	if stderr.buf.Len() > 0 {
		out += fmt.Sprintf("\nstderr:\n%s%s", stderr.buf.String(), stderr.truncatedNote("stderr"))
	}
	return out
}

// newBashTool 造 bash 工具。结果文本格式（验收对字节）：
// 成功 =「退出码：0\nstdout:\n<内容>」（stderr 非空再补一段）；非零退出同式带退出码；
// 启动失败返回 Go error；被终止 =「bash 被终止，死前输出：<已收输出>＋原因」经 wrapToolErrors
// 变「错误：…」结果文本——记录制（命令+最终输出落 runlog）在非正常死亡时不缺角，
// 卡死的命令事后可查「死前做到哪」（Qoder 记录形态同款）。
// 无自动超时：跑多久的裁定权在用户手里（手动停止随时掐，watchdog 判死也会掐），
// 自动一刀会误杀本来就合法的长命令（构建、装依赖）。
func newBashTool(workspace string) (tool.BaseTool, error) {
	return utils.InferTool("bash", "在 workspace 根目录下执行一条 bash 命令并返回退出码与 stdout/stderr。输出超 32 KB 会被截断，大输出请先写进文件再读。",
		func(ctx context.Context, in bashIn) (string, error) {
			cmd := exec.CommandContext(ctx, "bash", "-c", in.Command)
			cmd.Dir = workspace
			for _, k := range bashEnvWhitelist {
				if v, ok := os.LookupEnv(k); ok {
					cmd.Env = append(cmd.Env, k+"="+v)
				}
			}
			stdout, stderr := &capWriter{cap: bashOutputCap}, &capWriter{cap: bashOutputCap}
			cmd.Stdout, cmd.Stderr = stdout, stderr
			// WaitDelay（活体验收 2026-10-15 抓到的真卡死）：kill 只杀直接子进程 bash——
			// 多段命令（sleep 120; echo …）fork 出的孙进程活着并持有 stdout/stderr 管道写端，
			// EOF 永不来，cmd.Run 卡死在 awaitGoroutines。WaitDelay 在进程退出后给管道一个
			// 收口期限，到期强制关管道放行 Run——手动停止（ctx cancel）路径仍依赖这一收口。
			cmd.WaitDelay = 3 * time.Second
			err := cmd.Run()
			if ctx.Err() != nil {
				return "", fmt.Errorf("bash 被终止，死前输出：\n%s\n原因: %w", outputSections(stdout, stderr), ctx.Err())
			}
			code := 0
			if err != nil {
				ee, ok := err.(*exec.ExitError)
				if !ok {
					return "", fmt.Errorf("bash 启动失败: %w", err)
				}
				code = ee.ExitCode()
			}
			out := fmt.Sprintf("退出码：%d\n%s", code, outputSections(stdout, stderr))
			return out, nil
		})
}
