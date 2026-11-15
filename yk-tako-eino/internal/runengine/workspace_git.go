// 本文件是 GitWorkspaceProvider（方案真源 = doc/74 §8 / 施工包 §7）：已有 Git 项目的隔离工作区。
//
// 它做什么：第一次准备时把原项目克隆成**本组**中间 bare 仓（不跨组共享、删 origin）；每件 Task
// 领取时建一个 linked worktree + 内部分支，无父从 baseCommit、有父从父结果提交起并按 taskKey
// 顺序 merge；接受结果时程序校验工作区干净、产物在 HEAD、所有父提交是 HEAD 祖先、验证命令通过，
// 再把确切 HEAD 写成 refs/tako/results/<taskId>/<rev>（先本组、再 fetch 导出到原始仓库内部引用），
// 固化不可变 result.json；发布时核原始分支：未前进则 fast-forward，已前进则 workspace_changed，
// 脏则 workspace_dirty，无法解释则 recovery_required。**不 push、不操作远端、不自动 reset --hard。**
//
// Git 内部状态（baseCommit/baseBranch/结果引用/冲突）全在共享卷的 provider/ 与 git 引用里，
// 不进 PG、不进 Task JSON。锁分层与 Directory 一致：组操作取组锁，Task 操作取 Task 锁，
// AcceptResult 由引擎提交链持 Task 锁、内部走 Locked 变体。所有 git 命令固定参数数组、不经 shell、
// 自带超时与输出上限，且都在 PG 事务外。
package runengine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// gitProvider 是已有 Git 项目的 WorkspaceProvider。
type gitProvider struct {
	reserve storageReserveFunc
}

var _ WorkspaceProvider = (*gitProvider)(nil)

// 内部 git 命令的时长上限与输出上限（doc/74 §10.6：内部 Git/文件操作也有超时与输出上限）。
const (
	gitTimeout   = 3 * time.Minute
	gitOutputCap = 64 * 1024
)

// 结果引用与基线引用的命名（doc/74 §8.2）。taskId 形如 at_01ABC、revision 是整数，天然 ref 安全。
func gitResultRefName(taskID string, revision int) string {
	return fmt.Sprintf("refs/tako/results/%s/%d", taskID, revision)
}

const gitBaseRef = "refs/tako/base"

// ── 内部状态形状（provider/ 下，不进 PG）──────────────────────────────────────

// gitGroupState 是 provider/git.json：第一次准备时固定的基线。
type gitGroupState struct {
	FormatVersion int       `json:"formatVersion"`
	BaseCommit    string    `json:"baseCommit"`
	BaseBranch    string    `json:"baseBranch"`
	CreatedAt     time.Time `json:"createdAt"`
}

// gitParentRef 是 provider/parents/<taskId>.json 的一项：PrepareTask 解析出的父结果提交。
// AcceptResult 据此校验「所有父提交都是 HEAD 祖先」，不需要查 PG。
type gitParentRef struct {
	TaskID         string `json:"taskId"`
	ResultRevision int    `json:"resultRevision"`
	Commit         string `json:"commit"`
}

// gitConflict 是父子合并冲突（留在子 worktree，交当前子任务裁决）。
type gitConflict struct {
	Path           string `json:"path"`
	ParentTaskID   string `json:"parentTaskId"`
	ParentRevision int    `json:"parentRevision"`
	ParentCommit   string `json:"parentCommit"`
}

// gitPublication 是 publication/<epoch>.json：Git 发布是单步 fast-forward，记录整体状态。
type gitPublication struct {
	FormatVersion  int       `json:"formatVersion"`
	Epoch          int64     `json:"epoch"`
	TaskID         string    `json:"taskId"`
	ResultRevision int       `json:"resultRevision"`
	Commit         string    `json:"commit"`
	BaseBranch     string    `json:"baseBranch"`
	Status         string    `json:"status"` // pending / published / blocked
	ErrorCode      string    `json:"errorCode"`
	Detail         string    `json:"detail"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

// ── 路径与分支命名 ──────────────────────────────────────────────────────────

func (g *GroupHandle) gitRepoDir() string   { return filepath.Join(g.providerDir(), "repo.git") }
func (g *GroupHandle) gitStatePath() string { return filepath.Join(g.providerDir(), "git.json") }
func (g *GroupHandle) gitParentsPath(taskID string) string {
	return filepath.Join(g.providerDir(), "parents", taskID+".json")
}

// gitBranchName 造内部分支 tako/<orchestrationId>/<taskKey>。taskKey 可能含 git ref 不允许的
// 字符，按 git check-ref-format 的规则净化（控制字符/空格/~^:?*[\ 与 .. 换成 _）。
func gitBranchName(orchID, taskKey string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '_' || r == '-' || r == '.' || r == '/':
			return r
		default:
			return '_'
		}
	}, taskKey)
	safe = strings.ReplaceAll(safe, "..", "_")
	safe = strings.Trim(safe, "./")
	if safe == "" {
		safe = "task"
	}
	return "tako/" + orchID + "/" + safe
}

// gitWorkBranch 是工作现场对应的内部分支：isolated 每 Task 一条，shared 整组一条。
func (g *GroupHandle) gitWorkBranch(taskKey string) string {
	if g.sharedMode() {
		return "tako/" + g.OrchestrationID + "/shared"
	}
	return gitBranchName(g.OrchestrationID, taskKey)
}

// ── git 命令执行口 ──────────────────────────────────────────────────────────

// gitBuffer 是带上限的输出缓冲（超出截断，避免大输出撑爆内存/日志）。
type gitBuffer struct {
	buf       bytes.Buffer
	cap       int
	truncated bool
}

func (b *gitBuffer) Write(p []byte) (int, error) {
	if b.buf.Len() < b.cap {
		remain := b.cap - b.buf.Len()
		if len(p) > remain {
			b.buf.Write(p[:remain])
			b.truncated = true
		} else {
			b.buf.Write(p)
		}
	} else {
		b.truncated = true
	}
	return len(p), nil
}

func (b *gitBuffer) String() string {
	s := strings.TrimSpace(b.buf.String())
	if b.truncated {
		s += "…（输出已截断）"
	}
	return s
}

// gitEnv 造受控环境：剥掉进程里可能泄漏的 GIT_*（GIT_DIR/GIT_WORK_TREE 会覆盖我们的显式
// --git-dir），屏蔽系统与全局配置（避免用户 alias/hooks/proxy 干扰），关掉凭据交互。
func gitEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_OPTIONAL_LOCKS=0",
	)
}

// runGit 在 dir（cwd，空=当前）执行固定参数的 git 命令，返回 stdout 与错误。
// 统一关掉 hooks（core.hooksPath=/dev/null）——发布 fast-forward 不该触发产品仓库的 post-merge。
// 错误带截断后的 stderr 供排查；不把整段敏感 stderr 直接当业务 message（引擎侧再净化）。
func runGit(ctx context.Context, dir string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	full := append([]string{"-c", "core.hooksPath=/dev/null", "-c", "advice.detachedHead=false"}, args...)
	cmd := exec.CommandContext(cctx, "git", full...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = gitEnv()
	var out, errb gitBuffer
	out.cap, errb.cap = gitOutputCap, gitOutputCap
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if cctx.Err() == context.DeadlineExceeded {
		return out.String(), fmt.Errorf("git 命令超时（%s）：%s", gitTimeout, strings.Join(args, " "))
	}
	if err != nil {
		return out.String(), fmt.Errorf("git %s 失败：%w；%s", args[0], err, errb.String())
	}
	return out.String(), nil
}

// gitOut 执行 git 并返回净化后的单行 stdout（rev-parse / symbolic-ref 等）。
func gitOut(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := runGit(ctx, dir, args...)
	return strings.TrimSpace(out), err
}

func (g *GroupHandle) gitDirArg() string { return "--git-dir=" + g.gitRepoDir() }

// ── 内部状态读写 ────────────────────────────────────────────────────────────

func readGitState(g *GroupHandle) (*gitGroupState, error) {
	b, err := os.ReadFile(g.gitStatePath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st gitGroupState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("Git 组状态损坏：%w", err)
	}
	return &st, nil
}

func (p *gitProvider) saveParents(g *GroupHandle, taskID string, parents []gitParentRef) error {
	if err := os.MkdirAll(filepath.Dir(g.gitParentsPath(taskID)), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(parents)
	if err != nil {
		return err
	}
	return atomicWriteFile(g.gitParentsPath(taskID), b, 0o644)
}

func (p *gitProvider) loadParents(g *GroupHandle, taskID string) ([]gitParentRef, error) {
	b, err := os.ReadFile(g.gitParentsPath(taskID))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ps []gitParentRef
	if err := json.Unmarshal(b, &ps); err != nil {
		return nil, err
	}
	return ps, nil
}

func (p *gitProvider) saveConflicts(g *GroupHandle, taskID string, conflicts []gitConflict) error {
	if len(conflicts) == 0 {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(g.conflictsPath(taskID)), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(conflicts)
	if err != nil {
		return err
	}
	return atomicWriteFile(g.conflictsPath(taskID), b, 0o644)
}

func (p *gitProvider) loadConflicts(g *GroupHandle, taskID string) ([]gitConflict, error) {
	b, err := os.ReadFile(g.conflictsPath(taskID))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cs []gitConflict
	if err := json.Unmarshal(b, &cs); err != nil {
		return nil, err
	}
	return cs, nil
}

// ── PrepareGroup：本组 bare clone ───────────────────────────────────────────

func (p *gitProvider) PrepareGroup(ctx context.Context, g *GroupHandle) error {
	if err := ensureGroupState(ctx, g, workspaceProviderGit); err != nil {
		return err
	}
	rel, err := g.Locker.Lock(ctx, []string{g.groupLockKey()})
	if err != nil {
		return err
	}
	defer rel()
	// 幂等：状态与 bare 仓都在 = 已准备。
	if st, err := readGitState(g); err != nil {
		return err
	} else if st != nil {
		if bare, e := gitOut(ctx, "", g.gitDirArg(), "rev-parse", "--is-bare-repository"); e == nil && bare == "true" {
			return nil
		}
	}
	// 半成残留：组锁内清掉重建（bare 仓可由原项目重克隆，重建安全）。
	_ = os.RemoveAll(g.gitRepoDir())

	// 校验原项目：是 Git 仓、HEAD 在分支上（不支持 Detached HEAD）、至少一个提交（不支持空仓）。
	if _, err := gitOut(ctx, g.ProjectRoot, "rev-parse", "--git-dir"); err != nil {
		return fmt.Errorf("原项目不是 Git 仓库：%w", err)
	}
	baseBranch, err := gitOut(ctx, g.ProjectRoot, "symbolic-ref", "--short", "HEAD")
	if err != nil || baseBranch == "" {
		return fmt.Errorf("原项目 HEAD 不在分支上（不支持 Detached HEAD）")
	}
	baseCommit, err := gitOut(ctx, g.ProjectRoot, "rev-parse", "--verify", "HEAD")
	if err != nil || baseCommit == "" {
		return fmt.Errorf("原项目没有任何提交（不支持空仓库）")
	}

	if p.reserve != nil {
		release, err := p.reserve(ctx, g.TenantID, directoryBytes(g.ProjectRoot))
		if err != nil {
			return err
		}
		defer release()
	}
	if err := os.MkdirAll(g.providerDir(), 0o755); err != nil {
		return err
	}
	if _, err := runGit(ctx, "", "clone", "--bare", "--no-hardlinks", g.ProjectRoot, g.gitRepoDir()); err != nil {
		return fmt.Errorf("克隆本组中间仓库失败：%w", err)
	}
	// 删 origin：不让模型沿 origin 回到原始项目（doc/74 §8.1）。
	if _, err := runGit(ctx, "", g.gitDirArg(), "remote", "remove", "origin"); err != nil {
		return fmt.Errorf("删除中间仓库 origin 失败：%w", err)
	}
	if bare, err := gitOut(ctx, "", g.gitDirArg(), "rev-parse", "--is-bare-repository"); err != nil || bare != "true" {
		return fmt.Errorf("中间仓库不是 bare")
	}
	// 本组提交身份（worktree 共享 repo.git 配置）。
	if _, err := runGit(ctx, "", g.gitDirArg(), "config", "user.name", "tako"); err != nil {
		return err
	}
	if _, err := runGit(ctx, "", g.gitDirArg(), "config", "user.email", "tako@localhost"); err != nil {
		return err
	}
	st := gitGroupState{FormatVersion: 1, BaseCommit: baseCommit, BaseBranch: baseBranch, CreatedAt: time.Now().UTC()}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return atomicWriteFile(g.gitStatePath(), b, 0o644)
}

// ── PrepareTask / RecoverTaskWork：linked worktree ──────────────────────────

func (p *gitProvider) PrepareTask(ctx context.Context, g *GroupHandle, task AgentTask, parents []ResultRef) (TaskWork, error) {
	// shared 的 worktree 整组只有一个：建它必须走组锁（不同 Task 的 Task 锁互不挡）。
	lockKey := g.taskLockKey(task.ID)
	if g.sharedMode() {
		lockKey = g.groupLockKey()
	}
	rel, err := g.Locker.Lock(ctx, []string{lockKey})
	if err != nil {
		return TaskWork{}, err
	}
	defer rel()
	workRoot := g.workDir(task.ID)
	if _, err := os.Stat(filepath.Join(workRoot, ".git")); err == nil {
		// 已准备（幂等 / 崩溃重试）：直接返回，不重新 merge（会撞上模型已做的修改）。
		return p.taskWorkFromExisting(ctx, g, task)
	}
	st, err := readGitState(g)
	if err != nil {
		return TaskWork{}, err
	}
	if st == nil {
		return TaskWork{}, fmt.Errorf("Git 组尚未准备，无法领取 Task %s", task.ID)
	}
	// 解析父结果提交（按 taskKey 排序由调用方保证）。无父从 baseCommit，有父从第一个父提交起。
	// shared：整组同一条内部支线，父任务的提交本来就在支线上，不再按结果 merge。
	resolved := make([]gitParentRef, 0, len(parents))
	if !g.sharedMode() {
		for _, ref := range parents {
			commit, err := gitOut(ctx, "", g.gitDirArg(), "rev-parse", "--verify", gitResultRefName(ref.TaskID, ref.ResultRevision))
			if err != nil || commit == "" {
				return TaskWork{}, fmt.Errorf("父结果 %s/%d 的提交不存在（尚未接受？）", ref.TaskID, ref.ResultRevision)
			}
			resolved = append(resolved, gitParentRef{TaskID: ref.TaskID, ResultRevision: ref.ResultRevision, Commit: commit})
		}
	}
	startCommit := st.BaseCommit
	if !g.sharedMode() && len(resolved) > 0 {
		startCommit = resolved[0].Commit
	}
	if p.reserve != nil {
		release, err := p.reserve(ctx, g.TenantID, directoryBytes(g.ProjectRoot))
		if err != nil {
			return TaskWork{}, err
		}
		defer release()
	}
	if err := os.MkdirAll(filepath.Dir(workRoot), 0o755); err != nil {
		return TaskWork{}, err
	}
	branch := g.gitWorkBranch(task.TaskKey)
	// 清理崩溃残留的 worktree 注册（只 prune 已失效的，不删未知目录）。
	if _, err := runGit(ctx, "", g.gitDirArg(), "worktree", "prune"); err != nil {
		return TaskWork{}, err
	}
	// 分支已存在（崩溃在「建分支」与「注册 worktree」之间）→ 复用它，保留可能已有的提交；
	// 否则从起始提交新建。
	if _, err := gitOut(ctx, "", g.gitDirArg(), "rev-parse", "--verify", "refs/heads/"+branch); err == nil {
		if _, err := runGit(ctx, "", g.gitDirArg(), "worktree", "add", workRoot, branch); err != nil {
			return TaskWork{}, fmt.Errorf("恢复 worktree 失败：%w", err)
		}
	} else {
		if _, err := runGit(ctx, "", g.gitDirArg(), "worktree", "add", "-b", branch, workRoot, startCommit); err != nil {
			return TaskWork{}, fmt.Errorf("创建 worktree 失败：%w", err)
		}
	}
	if err := p.verifyBinding(ctx, g, task, workRoot); err != nil {
		return TaskWork{}, err
	}
	var conflicts []gitConflict
	if !g.sharedMode() {
		if err := p.saveParents(g, task.ID, resolved); err != nil {
			return TaskWork{}, err
		}
		if conflicts, err = p.mergeParents(ctx, g, workRoot, resolved); err != nil {
			return TaskWork{}, err
		}
		if err := p.saveConflicts(g, task.ID, conflicts); err != nil {
			return TaskWork{}, err
		}
	}
	// 无父 Task 从继任组固化的复用快照带入前代结果（doc/75 §7.4）。
	// Git 组必须先提交：AcceptResult 的 requireCleanWorktree 拒绝未跟踪文件。
	reuseNote := ""
	reuseConflicts := 0
	if len(parents) == 0 {
		app, err := applyGroupReuse(g, workRoot)
		if err != nil {
			return TaskWork{}, err
		}
		if len(app.Applied) > 0 {
			if _, err := runGit(ctx, workRoot, "add", "-A"); err != nil {
				return TaskWork{}, fmt.Errorf("暂存复用结果失败：%w", err)
			}
			if _, err := runGit(ctx, workRoot, "commit", "-m", "tako: 带入前代任务仍有效的结果"); err != nil {
				return TaskWork{}, fmt.Errorf("提交复用结果失败：%w", err)
			}
		}
		reuseNote = app.Note
		reuseConflicts = len(app.Conflicts)
	}
	return TaskWork{Root: workRoot, RuntimeRoot: g.Dir, PromptOverlay: gitOverlay(g.sharedMode(), conflicts) + reuseNote, ExtraWriteRoots: []string{g.gitRepoDir()}, Conflicts: len(conflicts) + reuseConflicts}, nil
}

func (p *gitProvider) RecoverTaskWork(ctx context.Context, g *GroupHandle, task AgentTask, revision int) (TaskWork, error) {
	lockKey := g.taskLockKey(task.ID)
	if g.sharedMode() {
		lockKey = g.groupLockKey()
	}
	rel, err := g.Locker.Lock(ctx, []string{lockKey})
	if err != nil {
		return TaskWork{}, err
	}
	defer rel()
	workRoot := g.workDir(task.ID)
	if _, err := os.Stat(filepath.Join(workRoot, ".git")); err != nil {
		return TaskWork{}, fmt.Errorf("Task %s 的 worktree 不存在，无法恢复", task.ID)
	}
	if err := p.verifyBinding(ctx, g, task, workRoot); err != nil {
		return TaskWork{}, err
	}
	return p.taskWorkFromExisting(ctx, g, task)
}

func (p *gitProvider) taskWorkFromExisting(ctx context.Context, g *GroupHandle, task AgentTask) (TaskWork, error) {
	conflicts, err := p.loadConflicts(g, task.ID)
	if err != nil {
		return TaskWork{}, err
	}
	return TaskWork{Root: g.workDir(task.ID), RuntimeRoot: g.Dir, PromptOverlay: gitOverlay(g.sharedMode(), conflicts), ExtraWriteRoots: []string{g.gitRepoDir()}, Conflicts: len(conflicts)}, nil
}

// verifyBinding 核 worktree top、common dir 与分支都和 Task 绑定一致（doc/74 §7.1）。
func (p *gitProvider) verifyBinding(ctx context.Context, g *GroupHandle, task AgentTask, workRoot string) error {
	top, err := gitOut(ctx, workRoot, "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("读 worktree top 失败：%w", err)
	}
	if !samePath(top, workRoot) {
		return fmt.Errorf("worktree top 与 Task 绑定不符：%s != %s", top, workRoot)
	}
	common, err := gitOut(ctx, workRoot, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return fmt.Errorf("读 worktree common dir 失败：%w", err)
	}
	if !samePath(common, g.gitRepoDir()) {
		return fmt.Errorf("worktree common dir 不属于本组：%s != %s", common, g.gitRepoDir())
	}
	branch, err := gitOut(ctx, workRoot, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return fmt.Errorf("worktree HEAD 不在分支上：%w", err)
	}
	if branch != g.gitWorkBranch(task.TaskKey) {
		return fmt.Errorf("worktree 分支与 Task 绑定不符：%s", branch)
	}
	return nil
}

// samePath 比较两个路径是否指向同一位置（Abs + EvalSymlinks 后逐字比）。
func samePath(a, b string) bool {
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		ra = filepath.Clean(a)
	}
	rb, err := filepath.EvalSymlinks(b)
	if err != nil {
		rb = filepath.Clean(b)
	}
	return ra == rb
}

// mergeParents 从第二个父起依次 merge（第一个已是起始提交）。冲突留在子 worktree、记录清单，
// 不自动 abort——交当前子任务裁决。一旦冲突就停止后续 merge（未 conclude 的 merge 挡住下一次），
// 剩余父由 overlay 提示模型手动合并；AcceptResult 的祖先校验是程序兜底。
func (p *gitProvider) mergeParents(ctx context.Context, g *GroupHandle, workRoot string, resolved []gitParentRef) ([]gitConflict, error) {
	var conflicts []gitConflict
	if len(resolved) <= 1 {
		return conflicts, nil
	}
	for _, par := range resolved[1:] {
		if _, err := runGit(ctx, workRoot, "merge", "--no-edit", par.Commit); err != nil {
			out, _ := gitOut(ctx, workRoot, "diff", "--name-only", "--diff-filter=U")
			paths := splitNonEmptyLines(out)
			if len(paths) == 0 {
				return nil, fmt.Errorf("合并父结果 %s/%d 失败：%w", par.TaskID, par.ResultRevision, err)
			}
			for _, path := range paths {
				conflicts = append(conflicts, gitConflict{Path: path, ParentTaskID: par.TaskID, ParentRevision: par.ResultRevision, ParentCommit: par.Commit})
			}
			break
		}
	}
	return conflicts, nil
}

func splitNonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// gitOverlay 是 worktree 的 prompt 补充（只指导模型，程序判定不依赖它）。
func gitOverlay(shared bool, conflicts []gitConflict) string {
	var b strings.Builder
	if shared {
		b.WriteString("你在本编排组共用的 Git worktree 中工作，其他任务也会读写同一分支；原项目不会被直接改动。")
	} else {
		b.WriteString("你在一个隔离的 Git worktree 中工作，原项目不会被直接改动。")
	}
	b.WriteString("完成工作后请 git commit 你的修改，")
	b.WriteString("再用 submit_task_result 提交结果，artifacts 声明本次交付的文件（相对路径，必须已提交进 HEAD）。")
	if shared {
		b.WriteString("程序会校验工作区干净、产物在 HEAD。")
	} else {
		b.WriteString("程序会校验工作区干净、产物在 HEAD、所有父结果提交都已合并。")
	}
	if len(conflicts) > 0 {
		b.WriteString("\n\n合并父任务结果时出现冲突，需要你裁决（程序不会自动合并）：")
		seen := map[string]bool{}
		for _, c := range conflicts {
			if seen[c.Path] {
				continue
			}
			seen[c.Path] = true
			fmt.Fprintf(&b, "\n- %s：与父任务 %s 的提交冲突，请解决冲突标记后 git add 并 git commit。", c.Path, c.ParentTaskID)
		}
		b.WriteString("\n解决完冲突后，确保所有父结果提交都已合并进当前分支（必要时手动 git merge）。")
	}
	return b.String()
}

// ── AcceptResult：校验 + 写引用 + 固化 ──────────────────────────────────────

func (p *gitProvider) AcceptResult(ctx context.Context, g *GroupHandle, task AgentTask, revision int, result TaskResult, work TaskWork) (ImmutableResult, error) {
	workRoot := g.workDir(task.ID)
	if work.Root != workRoot {
		return ImmutableResult{}, fmt.Errorf("工作现场绑定不符：%s != %s", work.Root, workRoot)
	}
	if err := p.verifyBinding(ctx, g, task, workRoot); err != nil {
		return ImmutableResult{}, err
	}
	// 工作区必须干净：无未提交已跟踪修改、无未跟踪非运行时文件、无 merge/rebase/cherry-pick 中途。
	if err := p.requireCleanWorktree(ctx, workRoot); err != nil {
		return ImmutableResult{}, err
	}
	// 每个声明产物是 HEAD 里的已跟踪普通文件（blob），且与 HEAD 一致（工作区干净已保证）。
	for _, a := range result.Artifacts {
		clean, err := resolveArtifactPath(workRoot, a.Path)
		if err != nil {
			return ImmutableResult{}, err
		}
		typ, err := gitOut(ctx, workRoot, "cat-file", "-t", "HEAD:"+clean)
		if err != nil {
			return ImmutableResult{}, fmt.Errorf("%w: artifact 不在 HEAD 里：%s", errSubmitBad, clean)
		}
		if typ != "blob" {
			return ImmutableResult{}, fmt.Errorf("%w: artifact 不是普通文件：%s", errSubmitBad, clean)
		}
	}
	// 所有父结果提交都是 HEAD 祖先（shared 组没有按父 merge 的步骤：同一分支上
	// 父任务的提交已被支线纳入，由 DAG 领取顺序保证；不再逐父校验）。
	head, err := gitOut(ctx, workRoot, "rev-parse", "HEAD")
	if err != nil {
		return ImmutableResult{}, err
	}
	if !g.sharedMode() {
		parents, err := p.loadParents(g, task.ID)
		if err != nil {
			return ImmutableResult{}, err
		}
		for _, par := range parents {
			if _, err := runGit(ctx, workRoot, "merge-base", "--is-ancestor", par.Commit, head); err != nil {
				return ImmutableResult{}, fmt.Errorf("%w: 父结果 %s/%d 尚未合并进 HEAD", errSubmitBad, par.TaskID, par.ResultRevision)
			}
		}
	}
	if g.VerifyCommand != "" {
		if err := runVerifyCommand(ctx, g, task.ID, workRoot, g.VerifyCommand); err != nil {
			return ImmutableResult{}, err
		}
	}
	// 完成前重读 HEAD，变化则拒绝（doc/74 §7.2）。
	commit, err := gitOut(ctx, workRoot, "rev-parse", "HEAD")
	if err != nil {
		return ImmutableResult{}, err
	}
	if commit != head {
		return ImmutableResult{}, fmt.Errorf("接受结果期间 HEAD 发生变化，拒绝")
	}
	// 先固化不可变结果（reference = 确切 commit；artifacts/ 逐字复制声明产物，工作区==HEAD），
	// 再写引用：同 revision 内容变了会在固化这一步被身份检查拒绝（errImmutableIdentityConflict），
	// 此时引用还没动，提交链 +1 重试不会覆盖旧版本的引用（doc/74 §9.2）。
	res, err := finalizeImmutableResultLocked(g, task.ID, revision, workspaceProviderGit, result.Artifacts, workRoot, commit, nil, nil)
	if err != nil {
		return ImmutableResult{}, err
	}
	refName := gitResultRefName(task.ID, revision)
	if _, err := runGit(ctx, "", g.gitDirArg(), "update-ref", refName, commit); err != nil {
		return ImmutableResult{}, fmt.Errorf("写本组结果引用失败：%w", err)
	}
	// fetch 导出到原始仓库内部引用（对象随之进入原始仓，主分支与工作树不变）。
	if _, err := runGit(ctx, g.ProjectRoot, "fetch", "--no-tags", g.gitRepoDir(), refName+":"+refName); err != nil {
		return ImmutableResult{}, fmt.Errorf("导出结果引用到原始仓库失败：%w", err)
	}
	return res, nil
}

// requireCleanWorktree 核工作区干净且无进行中的 Git 操作（doc/74 §7.2 检查 2）。
func (p *gitProvider) requireCleanWorktree(ctx context.Context, workRoot string) error {
	for _, marker := range []string{"MERGE_HEAD", "REBASE_HEAD", "CHERRY_PICK_HEAD"} {
		if _, err := gitOut(ctx, workRoot, "rev-parse", "-q", "--verify", marker); err == nil {
			return fmt.Errorf("%w: 工作区有进行中的 Git 操作（%s），请先完成", errSubmitBad, marker)
		}
	}
	out, err := gitOut(ctx, workRoot, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("读工作区状态失败：%w", err)
	}
	for _, line := range splitNonEmptyLines(out) {
		if strings.HasPrefix(line, "??") {
			path := strings.TrimSpace(strings.TrimPrefix(line, "??"))
			if path == ".tako" || strings.HasPrefix(path, ".tako/") {
				continue // 运行时目录，不进结果
			}
			return fmt.Errorf("%w: 有未跟踪的非运行时文件：%s（请提交或移除）", errSubmitBad, path)
		}
		return fmt.Errorf("%w: 有未提交的已跟踪修改，请先 git commit", errSubmitBad)
	}
	return nil
}

// ── ReadArtifact ────────────────────────────────────────────────────────────

func (p *gitProvider) ReadArtifact(ctx context.Context, g *GroupHandle, ref ResultRef, path string) ([]byte, error) {
	return readResultArtifact(g, ref, path)
}

// ── PreparePublication / Publish / ReconcilePublication ─────────────────────

func (p *gitProvider) PreparePublication(ctx context.Context, g *GroupHandle, final ImmutableResult) (PublicationPlan, error) {
	rel, err := g.Locker.Lock(ctx, []string{g.groupLockKey()})
	if err != nil {
		return PublicationPlan{}, err
	}
	defer rel()
	st, err := readGitState(g)
	if err != nil {
		return PublicationPlan{}, err
	}
	if st == nil {
		return PublicationPlan{}, fmt.Errorf("Git 组尚未准备")
	}
	now := time.Now().UTC()
	pub := gitPublication{
		FormatVersion: 1, TaskID: final.TaskID, ResultRevision: final.ResultRevision,
		Commit: final.Reference, BaseBranch: st.BaseBranch,
		Status: publicationStatusPending, CreatedAt: now, UpdatedAt: now,
	}
	if err := writeGitPublication(g, publicationDraftPath(g), &pub); err != nil {
		return PublicationPlan{}, err
	}
	return PublicationPlan{Summary: fmt.Sprintf("fast-forward %s 到结果提交 %s", st.BaseBranch, shortCommit(final.Reference))}, nil
}

func shortCommit(c string) string {
	if len(c) > 12 {
		return c[:12]
	}
	return c
}

func writeGitPublication(g *GroupHandle, path string, pub *gitPublication) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	pub.UpdatedAt = time.Now().UTC()
	b, err := json.Marshal(pub)
	if err != nil {
		return err
	}
	return atomicWriteFile(path, b, 0o644)
}

func readGitPublication(g *GroupHandle, epoch int64) (*gitPublication, error) {
	b, err := os.ReadFile(g.publicationPath(epoch))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var pub gitPublication
	if err := json.Unmarshal(b, &pub); err != nil {
		return nil, fmt.Errorf("Git 发布记录损坏：%w", err)
	}
	return &pub, nil
}

func readGitPublicationDraft(g *GroupHandle) (*gitPublication, error) {
	b, err := os.ReadFile(publicationDraftPath(g))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var pub gitPublication
	if err := json.Unmarshal(b, &pub); err != nil {
		return nil, err
	}
	return &pub, nil
}

func (p *gitProvider) Publish(ctx context.Context, g *GroupHandle, final ImmutableResult, epoch int64) (PublishOutcome, error) {
	rel, err := g.Locker.Lock(ctx, []string{g.groupLockKey()})
	if err != nil {
		return PublishOutcome{}, err
	}
	defer rel()
	pub, err := readGitPublication(g, epoch)
	if err != nil {
		return PublishOutcome{}, err
	}
	if pub == nil {
		draft, err := readGitPublicationDraft(g)
		if err != nil || draft == nil {
			return PublishOutcome{}, fmt.Errorf("发布计划不存在，请先 PreparePublication")
		}
		draft.Epoch = epoch
		pub = draft
		if err := writeGitPublication(g, g.publicationPath(epoch), pub); err != nil {
			return PublishOutcome{}, err
		}
	}
	return p.applyPublication(ctx, g, final, pub, epoch)
}

func (p *gitProvider) ReconcilePublication(ctx context.Context, g *GroupHandle, epoch int64) (PublishOutcome, error) {
	rel, err := g.Locker.Lock(ctx, []string{g.groupLockKey()})
	if err != nil {
		return PublishOutcome{}, err
	}
	defer rel()
	pub, err := readGitPublication(g, epoch)
	if err != nil {
		return PublishOutcome{}, err
	}
	if pub == nil {
		return PublishOutcome{}, fmt.Errorf("Git 发布记录不存在，无法对账 epoch %d", epoch)
	}
	final, err := readImmutableResult(g, ResultRef{TaskID: pub.TaskID, ResultRevision: pub.ResultRevision})
	if err != nil {
		return PublishOutcome{}, err
	}
	// Publish 与 Reconcile 共用同一核现场核心：原始 HEAD 已等于结果提交则补 published（不再 merge），
	// 否则按当前现场判定 fast-forward / changed / dirty / recovery。
	return p.applyPublication(ctx, g, final, pub, epoch)
}

// applyPublication 核原始仓库现场并执行 fast-forward（doc/74 §7.3）。
func (p *gitProvider) applyPublication(ctx context.Context, g *GroupHandle, final ImmutableResult, pub *gitPublication, epoch int64) (PublishOutcome, error) {
	finish := func(out PublishOutcome) (PublishOutcome, error) {
		pub.Status, pub.ErrorCode, pub.Detail = out.Status, out.ErrorCode, out.Detail
		if out.Status == publishOutcomePublished {
			pub.Status = publicationStatusPublished
		}
		if err := writeGitPublication(g, g.publicationPath(epoch), pub); err != nil {
			return PublishOutcome{}, err
		}
		return out, nil
	}
	commit := final.Reference
	// 原始 HEAD 必须在 baseBranch 上（用户切了分支 = 现场已前进）。
	branch, err := gitOut(ctx, g.ProjectRoot, "symbolic-ref", "--short", "HEAD")
	if err != nil || branch == "" {
		return finish(PublishOutcome{Status: publishOutcomeBlocked, ErrorCode: taskErrPublicationRecovery, Detail: "原始仓库 HEAD 不在分支上，需人工检查"})
	}
	if branch != pub.BaseBranch {
		return finish(PublishOutcome{Status: publishOutcomeBlocked, ErrorCode: taskErrWorkspaceChanged, Detail: "原始仓库已切换到其他分支，旧结果不能覆盖"})
	}
	// 进行中的 Git 操作 = 无法解释。
	for _, marker := range []string{"MERGE_HEAD", "REBASE_HEAD", "CHERRY_PICK_HEAD"} {
		if _, err := gitOut(ctx, g.ProjectRoot, "rev-parse", "-q", "--verify", marker); err == nil {
			return finish(PublishOutcome{Status: publishOutcomeBlocked, ErrorCode: taskErrPublicationRecovery, Detail: "原始仓库有进行中的 Git 操作，需人工检查"})
		}
	}
	// 脏跟踪文件 = workspace_dirty（不自动 commit/stash/clean）。
	status, err := gitOut(ctx, g.ProjectRoot, "status", "--porcelain")
	if err != nil {
		return finish(PublishOutcome{Status: publishOutcomeBlocked, ErrorCode: taskErrPublicationRecovery, Detail: "无法读取原始仓库状态"})
	}
	for _, line := range splitNonEmptyLines(status) {
		if !strings.HasPrefix(line, "??") {
			return finish(PublishOutcome{Status: publishOutcomeBlocked, ErrorCode: taskErrWorkspaceDirty, Detail: "原始项目有未保存内容，请先处理"})
		}
	}
	// 结果提交对象必须已在原始仓库（接受结果时 fetch 导出过）。
	if _, err := gitOut(ctx, g.ProjectRoot, "cat-file", "-e", commit+"^{commit}"); err != nil {
		return finish(PublishOutcome{Status: publishOutcomeBlocked, ErrorCode: taskErrPublicationRecovery, Detail: "原始仓库缺少结果提交对象，需人工检查"})
	}
	origHead, err := gitOut(ctx, g.ProjectRoot, "rev-parse", "HEAD")
	if err != nil {
		return finish(PublishOutcome{Status: publishOutcomeBlocked, ErrorCode: taskErrPublicationRecovery, Detail: "无法读取原始仓库 HEAD"})
	}
	if origHead == commit {
		return finish(PublishOutcome{Status: publishOutcomePublished}) // 幂等（含崩溃后重入）
	}
	if _, err := runGit(ctx, g.ProjectRoot, "merge-base", "--is-ancestor", origHead, commit); err == nil {
		// 原始分支未前进 → fast-forward。git 自身会拒绝覆盖未跟踪文件（不会静默覆盖）。
		if _, err := runGit(ctx, g.ProjectRoot, "merge", "--ff-only", commit); err != nil {
			if strings.Contains(err.Error(), "would be overwritten by merge") {
				return finish(PublishOutcome{Status: publishOutcomeBlocked, ErrorCode: taskErrWorkspaceDirty, Detail: "原始项目有未跟踪文件会被覆盖，请先处理"})
			}
			return finish(PublishOutcome{Status: publishOutcomeBlocked, ErrorCode: taskErrPublicationRecovery, Detail: "fast-forward 失败，需人工检查"})
		}
		return finish(PublishOutcome{Status: publishOutcomePublished})
	}
	// 原始分支已前进：把当前 baseBranch fetch 进本组 refs/tako/base，供最终执行者重新合并。
	if _, err := runGit(ctx, "", g.gitDirArg(), "fetch", "--no-tags", g.ProjectRoot, "refs/heads/"+pub.BaseBranch+":"+gitBaseRef); err != nil {
		return finish(PublishOutcome{Status: publishOutcomeBlocked, ErrorCode: taskErrPublicationRecovery, Detail: "无法把新基线带入本组仓库"})
	}
	return finish(PublishOutcome{Status: publishOutcomeBlocked, ErrorCode: taskErrWorkspaceChanged, Detail: "原项目已前进，旧结果不能覆盖；新基线已带入本组仓库 refs/tako/base，请 git merge 该引用、裁决冲突后重新提交"})
}

// ── Cleanup ─────────────────────────────────────────────────────────────────

func (p *gitProvider) Cleanup(ctx context.Context, g *GroupHandle, now time.Time, archiveTTL time.Duration) (CleanupReport, error) {
	rel, err := g.Locker.Lock(ctx, []string{g.groupLockKey()})
	if err != nil {
		return CleanupReport{}, err
	}
	defer rel()
	var report CleanupReport
	if st, err := readWorkspaceGroupState(g.Dir); err == nil && st != nil {
		if now.Sub(st.UpdatedAt) < archiveTTL {
			report.Kept = append(report.Kept, "组工作区（未过清理窗口）")
			return report, nil
		}
	}
	unresolved, err := gitPublicationUnresolved(g)
	if err != nil {
		return CleanupReport{}, err
	}
	if unresolved {
		report.Kept = append(report.Kept, "组工作区（发布未收口）")
		return report, nil
	}
	// 任一 worktree 有未提交内容或进行中操作 = 无法证明安全 = 保留全部现场。
	if entries, err := os.ReadDir(filepath.Join(g.Dir, "work")); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			wr := filepath.Join(g.Dir, "work", e.Name())
			if _, err := os.Stat(filepath.Join(wr, ".git")); err != nil {
				continue
			}
			if err := p.requireCleanWorktree(ctx, wr); err != nil {
				report.Kept = append(report.Kept, "组工作区（worktree "+e.Name()+" 有未提交内容）")
				return report, nil
			}
		}
	}
	// 删 worktree 与中间仓库；保留不可变结果（artifacts/ 已存字节）、发布记录、验证证据、组状态。
	for _, target := range []struct{ path, label string }{
		{filepath.Join(g.Dir, "work"), "worktree"},
		{g.gitRepoDir(), "中间仓库"},
		{filepath.Join(g.providerDir(), "conflicts"), "冲突记录"},
		{filepath.Join(g.providerDir(), "parents"), "父提交记录"},
	} {
		if _, err := os.Stat(target.path); err != nil {
			continue
		}
		if err := os.RemoveAll(target.path); err != nil {
			report.Kept = append(report.Kept, target.label+"（删除失败："+err.Error()+"）")
			continue
		}
		report.Removed = append(report.Removed, target.label)
	}
	report.Kept = append(report.Kept, "不可变结果/发布记录/验证证据")
	return report, nil
}

// gitPublicationUnresolved 扫 publication/*.json，任一记录非 published = 未收口。
func gitPublicationUnresolved(g *GroupHandle) (bool, error) {
	dir := filepath.Join(g.Dir, "publication")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") && e.Name() != "draft.json" {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return true, nil
		}
		var pub gitPublication
		if err := json.Unmarshal(b, &pub); err != nil {
			return true, nil
		}
		if pub.Status != publicationStatusPublished {
			return true, nil
		}
	}
	return false, nil
}
