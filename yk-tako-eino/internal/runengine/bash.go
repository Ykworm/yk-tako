// bash 执行工具（doc/17 §3）：researcher/coder「能写脚本拿不到结果」病灶的出口——
// 批量活先写脚本再跑。exe 是 exec.CommandContext 包 bash -c，作业根固定 workspace
// （模型给的 command 相对路径都在里面解析；不做符号链接逃逸、联网隔离、危险命令表——
// doc §3 明文本版不做）。错误经 wrapToolErrors 变「错误：…」回注，run 不因工具失败变 error。
package runengine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
)

// bashEnvWhitelist 是子进程环境白名单：引擎进程里可能有厂商 API Key 等密钥，
// tool_result 与 runlog 行是永久历史——env 全量透传 = 密钥进永久库。
//
// PATH 不在这里：它由 subprocessPath 单独给（doc/52 D39），只留围栏读得到的目录。
// 原样透传宿主的 PATH 会让 `~/.pyenv/shims/python3` 被找到、执行时被围栏拒成 EPERM。
var bashEnvWhitelist = []string{"HOME", "TMPDIR", "LANG", "LC_ALL"}

// bashIn 是 bash 的入参 schema：由 InferTool 按 struct tag 反射生成。
type bashIn struct {
	Command string `json:"command" jsonschema:"description=要执行的 bash 命令，在 workspace 根目录下运行；无单次时长限制，stdout/stderr 各保留尾部 64000 字节，完整输出落盘后给出路径"`
}

// outputSections 拼装 stdout/stderr 两段，成功结果与被终止错误共用同形。
func outputSections(stdout, stderr *tailCollector) string {
	out := fmt.Sprintf("stdout:\n%s%s", stdout.text(), stdout.note("stdout"))
	if len(stderr.tail) > 0 {
		out += fmt.Sprintf("\nstderr:\n%s%s", stderr.text(), stderr.note("stderr"))
	}
	return out
}

// newBashTool 造 bash 工具。结果文本格式（验收对字节）：
// 成功 =「退出码：0\nstdout:\n<内容>」（stderr 非空再补一段）；非零退出同式带退出码；
// 启动失败返回 Go error；被终止 =「bash 被终止，死前输出：<已收输出>＋原因」经 wrapToolErrors
// 变「错误：…」结果文本——记录制（命令+最终输出落 runlog）在非正常死亡时不缺角，
// 卡死的命令事后可查「死前做到哪」（Qoder 记录形态同款）。
// 时长帽 30 秒，到点把已收到的输出交回工具结果，不把父 run 打成 error。
// 工作目录来自 ctx 里的会话根；这只是默认目录，不是牢笼。有 sandbox-exec 时同一条命令再套一层配置。
func newBashTool(workspace string) (tool.BaseTool, error) {
	return utils.InferTool("bash", "在 workspace 根目录下执行一条 bash 命令并返回退出码与 stdout/stderr。"+
		"输出超过 64000 字节时只保留尾部，完整输出会落盘并给出路径（用 read_file 分页读它）。"+
		"单次最长 30 秒，大输出请先写进文件再读。",
		func(ctx context.Context, in bashIn) (string, error) {
			// 没有 sandbox-exec 就拒绝。cwd 只是默认目录，不能当成隔离。
			return runBash(ctx, activeRoot(ctx, workspace), in.Command, bashTimeout, true)
		})
}

// runBash 执行一条命令。cwd 不是安全边界：requireSandbox 为真时没有 sandbox-exec 就拒绝执行。
func runBash(ctx context.Context, workspace, command string, timeout time.Duration, requireSandbox bool) (string, error) {
	// 清扫过期落盘（doc/58 §4.4）：每次 bash 调用前扫一次 .tako/bash/。
	sweepBashSpills(workspace, time.Now())
	cmd, cleanup, _, runCtx, err := startBash(ctx, workspace, command, timeout, requireSandbox)
	if err != nil {
		return "", err
	}
	defer cleanup()
	cmd.Env = append(cmd.Env, "PATH="+subprocessPath(workspace, toolsRootFor(ctx, workspace)))
	for _, k := range bashEnvWhitelist {
		if v, ok := os.LookupEnv(k); ok {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	spill := newBashSpill(ctx, workspace)
	stdout := &tailCollector{cap: bashOutputCap, spill: spill, ext: "out"}
	stderr := &tailCollector{cap: bashOutputCap, spill: spill, ext: "err"}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = 3 * time.Second
	err = cmd.Run()
	stdout.closeSpill()
	stderr.closeSpill()
	// sandbox-exec 被时长帽杀掉时，Run 返回的是退出码 -1，不是 DeadlineExceeded。
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return fmt.Sprintf("bash 超时，死前输出：\n%s", outputSections(stdout, stderr)), nil
	}
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
	// 围栏被触发的信号：macOS 沙箱的拒绝文案是 "Operation not permitted"。
	// 裸给模型看这句它多半不懂「为什么」→ 补一句人话：只能在项目目录内读写，越界已被拒。
	if strings.Contains(out, "Operation not permitted") || strings.Contains(out, "not permitted") {
		out += fmt.Sprintf("\n（沙箱围栏：只能在项目目录 %s 内读写；越出这个范围的路径已被拒绝——请改用项目内的路径。）", workspace)
	}
	return out, nil
}
