// 本文件是隔离工作区的通用验证命令执行口（方案真源 = doc/74 §8.2/§10.6）。
//
// verifyCommand 在**现有 bash 沙箱**里跑（startBash，requireSandbox=true），固定 10 分钟
// 超时，复用工具 PATH（subprocessPath）、环境白名单与输出留存（tailCollector）。输出总是
// 落盘到组目录 verification/<taskId>.out/.err（force spill）——发布面板与排查要读它，
// 不能因为输出小就没有文件。证据（running→passed/failed）写 verification/<taskId>.json。
//
// 调用方必须已持有该 Task 的共享锁（证据写入走 Locked 变体）。验证失败返回 error，
// 由 AcceptResult 拒绝接受结果——程序判定，不靠 prompt 自述。
package runengine

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// verifyTimeout 是验证命令的固定时长上限（doc/74 §10.6）。
const verifyTimeout = 10 * time.Minute

// runVerifyCommand 在 workRoot 沙箱执行 command，写验证证据。退出码 0 = 通过（返回 nil）；
// 非零 / 超时 / 启动失败 = 不通过（返回 error，Detail 带输出尾部）。ctx 取消（run 被停止）
// 原样返回 ctx.Err()，证据标 failed。
func runVerifyCommand(ctx context.Context, g *GroupHandle, taskID, workRoot, command string) error {
	started := time.Now().UTC()
	outRel := "verification/" + taskID + ".out"
	ev := workspaceVerificationEvidence{
		TaskID: taskID, Status: verificationStatusRunning,
		OutputPath: outRel, StartedAt: started,
	}
	// 证据是观测面：写失败只记日志，不阻断验证本身。
	if err := writeVerificationEvidenceLocked(g, ev); err != nil {
		log.Printf("workspace-verify: 写 running 证据失败：%v", err)
	}

	finish := func(status string, code int) {
		fin := time.Now().UTC()
		ev.Status, ev.ExitCode, ev.FinishedAt = status, code, &fin
		if err := writeVerificationEvidenceLocked(g, ev); err != nil {
			log.Printf("workspace-verify: 写 %s 证据失败：%v", status, err)
		}
		trace.SpanFromContext(ctx).SetAttributes(attribute.String("tako.workspace.verification_status", status))
	}

	cmd, cleanup, _, runCtx, err := startBash(ctx, workRoot, command, verifyTimeout, true)
	if err != nil {
		finish(verificationStatusFailed, -1)
		return fmt.Errorf("验证命令启动失败：%w", err)
	}
	defer cleanup()
	cmd.Env = append(cmd.Env, "PATH="+subprocessPath(workRoot, toolsRootFor(ctx, workRoot)))
	for _, k := range bashEnvWhitelist {
		if v, ok := os.LookupEnv(k); ok {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	// 与执行者 bash 同一份 git 环境：验证命令在 Git worktree 里跑 git 也要能起（doc/74 §7）。
	if tb := agentTaskRunBoxFrom(ctx); tb != nil && tb.WorkspaceProvider == workspaceProviderGit {
		cmd.Env = append(cmd.Env, gitSandboxEnv()...)
	}
	spill := &bashSpill{
		dirRel: "verification",
		dirAbs: filepath.Join(g.Dir, "verification"),
		name:   taskID,
	}
	// 重试同一 Task 时清掉上一轮的输出：spill 用 O_EXCL，残留文件会让本轮落盘被弃掉、
	// 证据指到旧输出。验证证据每轮覆盖，输出也该每轮重置。
	outAbs := filepath.Join(spill.dirAbs, taskID+".out")
	errAbs := filepath.Join(spill.dirAbs, taskID+".err")
	_ = os.Remove(outAbs)
	_ = os.Remove(errAbs)
	stdout := &tailCollector{cap: bashOutputCap, spill: spill, ext: "out", force: true}
	stderr := &tailCollector{cap: bashOutputCap, spill: spill, ext: "err", force: true}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = 3 * time.Second
	runErr := cmd.Run()
	stdout.closeSpill()
	stderr.closeSpill()
	// 静默命令（如 test -f marker）没有任何 stdout，force spill 也不会建文件——
	// 补一个空的，证据 OutputPath 才总有一个可读文件（发布面板/排查要读它）。
	if stdout.relPath == "" {
		if err := os.MkdirAll(spill.dirAbs, 0o755); err == nil {
			if f, err := os.OpenFile(outAbs, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644); err == nil {
				_ = f.Close()
			}
		}
	}

	if errors.Is(runErr, context.DeadlineExceeded) || errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		finish(verificationStatusFailed, -1)
		return fmt.Errorf("验证命令超时（%s），死前输出：\n%s", verifyTimeout, outputSections(stdout, stderr))
	}
	if ctx.Err() != nil {
		finish(verificationStatusFailed, -1)
		return fmt.Errorf("验证被终止：%w", ctx.Err())
	}
	code := 0
	if runErr != nil {
		ee, ok := runErr.(*exec.ExitError)
		if !ok {
			finish(verificationStatusFailed, -1)
			return fmt.Errorf("验证命令启动失败：%w", runErr)
		}
		code = ee.ExitCode()
	}
	if code != 0 {
		finish(verificationStatusFailed, code)
		return fmt.Errorf("验证命令未通过（退出码 %d）：\n%s", code, outputSections(stdout, stderr))
	}
	finish(verificationStatusPassed, 0)
	return nil
}