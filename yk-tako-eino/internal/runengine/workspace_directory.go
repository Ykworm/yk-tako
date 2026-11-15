// 本文件是 DirectoryWorkspaceProvider（方案真源 = doc/74 §8；doc/76 §4）：非 Git 项目的工作区。
//
// 它做什么：第一次准备时把原项目普通文件冻结成组基线（物理副本 + 清单）；isolated 每件 Task
// 从基线复制独立工作目录，按父 taskKey 顺序应用父结果；shared 整组共用一棵从基线复制的
// work/shared（父结果本来就在同一棵树上，不重放）。接受结果时与基线 diff 出变更集、验证、
// 固化成不可变结果；发布时逐路径比对原项目与基线，仍等于基线才原子写入，全程记录进度，
// 崩溃后按记录与现场对账。**不执行任何 Git 命令，不创建 .git。**
//
// 锁分层（与 filelock.go 一致）：PrepareGroup/PreparePublication/Publish/Cleanup 取组锁；
// PrepareTask/RecoverTaskWork 取 Task 锁；AcceptResult 由调用方（引擎提交链）持 Task 锁，
// 内部走 Locked 变体，不再取锁。所有文件操作都在 PG 事务外。
package runengine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// storageReserveFunc 是租户存储配额口（引擎装配时注入 Store.ReserveStorage）。
// adding 是即将落盘的字节；返回释放函数（成功或失败都要调）。nil = 不限额（测试）。
type storageReserveFunc func(ctx context.Context, tenantID string, adding int64) (func(), error)

// directoryProvider 是非 Git 项目的 WorkspaceProvider。
type directoryProvider struct {
	reserve storageReserveFunc
}

// 编译期断言：directoryProvider 满足 Provider 接口。
var _ WorkspaceProvider = (*directoryProvider)(nil)

// ── 清单与状态形状 ──────────────────────────────────────────────────────────

// directoryFileEntry 是基线/工作目录清单的一项。只收普通文件（类型由遍历不变量承担），
// Mode 是权限位（含执行位——脚本丢了 +x，verifyCommand 就跑不起来）。
type directoryFileEntry struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Mode   uint32 `json:"mode"`
	Size   int64  `json:"size"`
}

// directoryBaseline 是 provider/baseline.json：组基线清单（按 path 排序）。
type directoryBaseline struct {
	FormatVersion int                  `json:"formatVersion"`
	CreatedAt     time.Time            `json:"createdAt"`
	Files         []directoryFileEntry `json:"files"`
}

// directoryChange 是结果清单里新增/修改的一项；directoryDelete 是删除的一项。
type directoryChange struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Mode   uint32 `json:"mode"`
}
type directoryDelete struct {
	Path string `json:"path"`
}

// directoryChangeSet 是 results/<taskId>/<rev>/manifest.json（doc/74 §8.2）。
type directoryChangeSet struct {
	Added    []directoryChange `json:"added"`
	Modified []directoryChange `json:"modified"`
	Deleted  []directoryDelete `json:"deleted"`
}

// 发布记录每路径的状态与操作词表。
const (
	dirPubPending  = "pending"
	dirPubDone     = "done"
	dirPubConflict = "conflict"
	dirOpAdd       = "add"
	dirOpModify    = "modify"
	dirOpDelete    = "delete"
)

// directoryPubEntry 是 publication/<epoch>.json 里的一路径记录（doc/74 §8.3）。
type directoryPubEntry struct {
	Path        string    `json:"path"`
	Op          string    `json:"op"`
	BaselineSHA string    `json:"baselineSha"` // add 为空
	TargetSHA   string    `json:"targetSha"`   // delete 为空
	Mode        uint32    `json:"mode"`
	Status      string    `json:"status"`
	Error       string    `json:"error"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// directoryPublication 是发布进度记录：崩溃后对账的依据。
type directoryPublication struct {
	FormatVersion  int                 `json:"formatVersion"`
	Epoch          int64               `json:"epoch"`
	TaskID         string              `json:"taskId"`
	ResultRevision int                 `json:"resultRevision"`
	CreatedAt      time.Time           `json:"createdAt"`
	UpdatedAt      time.Time           `json:"updatedAt"`
	Entries        []directoryPubEntry `json:"entries"`
}

// directoryConflict 是父子交接冲突（交当前子任务裁决，不自动合并）。
type directoryConflict struct {
	Path           string `json:"path"`
	ParentTaskID   string `json:"parentTaskId"`
	ParentRevision int    `json:"parentRevision"`
	Kind           string `json:"kind"` // content / delete
	WorkSHA        string `json:"workSha"`
	ParentSHA      string `json:"parentSha"`
}

// ── 文件遍历与哈希 ──────────────────────────────────────────────────────────

// hashFile 读普通文件算 SHA-256 与大小。
func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// walkDirectoryFiles 遍历 root 的普通文件（doc/74 §8.1）：排除 .tako 运行时目录、
// socket/device 等特殊文件与越界软链；目录软链不递归。界内文件软链按目标内容当普通文件收。
// 返回按 path 排序的清单。
func walkDirectoryFiles(root string) ([]directoryFileEntry, error) {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	var files []directoryFileEntry
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		if d.IsDir() {
			if rel == ".tako" {
				return filepath.SkipDir
			}
			return nil
		}
		target := p
		if d.Type()&fs.ModeSymlink != 0 {
			real, e := filepath.EvalSymlinks(p)
			if e != nil || !pathWithin(realRoot, real) {
				return nil // 断链 / 越界软链：排除
			}
			target = real
		} else if !d.Type().IsRegular() {
			return nil // 特殊文件：排除
		}
		info, e := os.Stat(target)
		if e != nil || !info.Mode().IsRegular() {
			return nil // 目录软链 / 特殊目标：排除
		}
		sha, size, e := hashFile(target)
		if e != nil {
			return e
		}
		files = append(files, directoryFileEntry{
			Path: rel, SHA256: sha, Mode: uint32(info.Mode().Perm()), Size: size,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

// baselineDir 是组基线物理副本目录。
func (g *GroupHandle) baselineDir() string { return filepath.Join(g.providerDir(), "baseline") }
func (g *GroupHandle) baselinePath() string {
	return filepath.Join(g.providerDir(), "baseline.json")
}
func (g *GroupHandle) conflictsPath(taskID string) string {
	return filepath.Join(g.providerDir(), "conflicts", taskID+".json")
}

// readBaseline 读组基线清单；不存在 = (nil, nil)。
func readBaseline(g *GroupHandle) (*directoryBaseline, error) {
	b, err := os.ReadFile(g.baselinePath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var bl directoryBaseline
	if err := json.Unmarshal(b, &bl); err != nil {
		return nil, fmt.Errorf("基线清单损坏：%w", err)
	}
	return &bl, nil
}

// ── PrepareGroup：冻结基线 ──────────────────────────────────────────────────

func (p *directoryProvider) PrepareGroup(ctx context.Context, g *GroupHandle) error {
	if err := ensureGroupState(ctx, g, workspaceProviderDirectory); err != nil {
		return err
	}
	rel, err := g.Locker.Lock(ctx, []string{g.groupLockKey()})
	if err != nil {
		return err
	}
	defer rel()
	// 幂等：清单与副本都在 = 已准备。
	if bl, err := readBaseline(g); err != nil {
		return err
	} else if bl != nil {
		if _, err := os.Stat(g.baselineDir()); err == nil {
			return nil
		}
	}
	// 半成残留（清单或副本缺一方）：组锁内清掉重建。基线可由原项目重算，重建安全。
	_ = os.RemoveAll(g.baselineDir())
	entries, err := walkDirectoryFiles(g.ProjectRoot)
	if err != nil {
		return fmt.Errorf("遍历原项目失败：%w", err)
	}
	var total int64
	for _, e := range entries {
		total += e.Size
	}
	if p.reserve != nil {
		release, err := p.reserve(ctx, g.TenantID, total)
		if err != nil {
			return err
		}
		defer release()
	}
	if err := os.MkdirAll(g.providerDir(), 0o755); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(g.providerDir(), ".staging-baseline-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	for _, e := range entries {
		dst := filepath.Join(staging, e.Path)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := copyFilePerm(filepath.Join(g.ProjectRoot, e.Path), dst, os.FileMode(e.Mode)); err != nil {
			return err
		}
	}
	if err := os.Rename(staging, g.baselineDir()); err != nil {
		return fmt.Errorf("固化基线失败：%w", err)
	}
	bl := directoryBaseline{FormatVersion: 1, CreatedAt: time.Now().UTC(), Files: entries}
	b, err := json.Marshal(bl)
	if err != nil {
		return err
	}
	return atomicWriteFile(g.baselinePath(), b, 0o644)
}

// ── PrepareTask / RecoverTaskWork：工作现场 ──────────────────────────────────

func (p *directoryProvider) PrepareTask(ctx context.Context, g *GroupHandle, task AgentTask, parents []ResultRef) (TaskWork, error) {
	// shared 的现场是整组共用的一棵目录树：建它必须走组锁（不同 Task 的 Task 锁互不挡）。
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
	if _, err := os.Stat(workRoot); err == nil {
		// 已准备（幂等 / 崩溃重试）：直接返回，不重新应用父结果（会撞上模型已做的修改）。
		return p.taskWorkFromExisting(g, task)
	}
	bl, err := readBaseline(g)
	if err != nil {
		return TaskWork{}, err
	}
	if bl == nil {
		return TaskWork{}, fmt.Errorf("组基线尚未准备，无法领取 Task %s", task.ID)
	}
	var total int64
	for _, e := range bl.Files {
		total += e.Size
	}
	if p.reserve != nil {
		release, err := p.reserve(ctx, g.TenantID, total)
		if err != nil {
			return TaskWork{}, err
		}
		defer release()
	}
	if err := os.MkdirAll(filepath.Dir(workRoot), 0o755); err != nil {
		return TaskWork{}, err
	}
	staging, err := os.MkdirTemp(filepath.Dir(workRoot), ".staging-"+task.ID+"-")
	if err != nil {
		return TaskWork{}, err
	}
	defer os.RemoveAll(staging)
	for _, e := range bl.Files {
		dst := filepath.Join(staging, e.Path)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return TaskWork{}, err
		}
		if err := copyFilePerm(filepath.Join(g.baselineDir(), e.Path), dst, os.FileMode(e.Mode)); err != nil {
			return TaskWork{}, err
		}
	}
	if err := os.Rename(staging, workRoot); err != nil {
		return TaskWork{}, fmt.Errorf("准备工作目录失败：%w", err)
	}
	// shared：父任务与当前任务在同一棵目录树上，父的修改本来就在现场里，不再按结果重放。
	var conflicts []directoryConflict
	if !g.sharedMode() {
		for _, ref := range parents {
			cs, err := p.applyParentResult(g, workRoot, bl, ref)
			if err != nil {
				return TaskWork{}, err
			}
			conflicts = append(conflicts, cs...)
		}
		if err := p.saveConflicts(g, task.ID, conflicts); err != nil {
			return TaskWork{}, err
		}
	}
	// 继任组的无父任务：把创建时固化的复用结果带进现场（doc/75 §7.4）。
	// 只做一次——工作现场已存在时上面的幂等分支已经返回，不会重放。
	reuseNote := ""
	reuseConflicts := 0
	if len(parents) == 0 {
		app, err := applyGroupReuse(g, workRoot)
		if err != nil {
			return TaskWork{}, err
		}
		reuseNote = app.Note
		reuseConflicts = len(app.Conflicts)
	}
	return TaskWork{Root: workRoot, RuntimeRoot: g.Dir, PromptOverlay: directoryOverlay(g.sharedMode(), conflicts) + reuseNote, Conflicts: len(conflicts) + reuseConflicts}, nil
}

func (p *directoryProvider) RecoverTaskWork(ctx context.Context, g *GroupHandle, task AgentTask, revision int) (TaskWork, error) {
	lockKey := g.taskLockKey(task.ID)
	if g.sharedMode() {
		lockKey = g.groupLockKey()
	}
	rel, err := g.Locker.Lock(ctx, []string{lockKey})
	if err != nil {
		return TaskWork{}, err
	}
	defer rel()
	if _, err := os.Stat(g.workDir(task.ID)); err != nil {
		return TaskWork{}, fmt.Errorf("Task %s 的工作现场不存在，无法恢复", task.ID)
	}
	return p.taskWorkFromExisting(g, task)
}

// taskWorkFromExisting 读已存在工作现场的冲突记录，重建 prompt 补充。
func (p *directoryProvider) taskWorkFromExisting(g *GroupHandle, task AgentTask) (TaskWork, error) {
	conflicts, err := p.loadConflicts(g, task.ID)
	if err != nil {
		return TaskWork{}, err
	}
	return TaskWork{Root: g.workDir(task.ID), RuntimeRoot: g.Dir, PromptOverlay: directoryOverlay(g.sharedMode(), conflicts), Conflicts: len(conflicts)}, nil
}

// applyParentResult 把一份父结果按路径应用进子工作目录（doc/74 §8.2）：
// 同路径同哈希幂等；不同哈希或删除/修改冲突交当前子任务（不覆盖，父版本放进 .tako/conflicts）。
// 应用前校验父清单与产物实际哈希，损坏拒绝。
func (p *directoryProvider) applyParentResult(g *GroupHandle, workRoot string, bl *directoryBaseline, ref ResultRef) ([]directoryConflict, error) {
	manifest, err := readResultManifest(g, ref)
	if err != nil {
		return nil, fmt.Errorf("父结果 %s/%d 清单不可读：%w", ref.TaskID, ref.ResultRevision, err)
	}
	artRoot := filepath.Join(g.resultDir(ref.TaskID, ref.ResultRevision), "artifacts")
	// 校验父清单哈希与产物实际字节一致（损坏拒绝，不把错误输入交给模型）。
	for _, c := range append(append([]directoryChange{}, manifest.Added...), manifest.Modified...) {
		sha, _, err := hashFile(filepath.Join(artRoot, c.Path))
		if err != nil || sha != c.SHA256 {
			return nil, fmt.Errorf("父结果 %s/%d 产物 %s 哈希不符，已损坏", ref.TaskID, ref.ResultRevision, c.Path)
		}
	}
	base := map[string]directoryFileEntry{}
	for _, e := range bl.Files {
		base[e.Path] = e
	}
	var conflicts []directoryConflict
	apply := func(c directoryChange, op string) error {
		workPath := filepath.Join(workRoot, c.Path)
		curSHA, exists, err := hashFileIfExists(workPath)
		if err != nil {
			return err
		}
		if exists && curSHA == c.SHA256 {
			return nil // 幂等
		}
		baselineSHA := ""
		if be, ok := base[c.Path]; ok {
			baselineSHA = be.SHA256
		}
		// add 只在路径不存在时带入；modify 只在当前内容仍等于基线时应用。
		// 其余（含 modify 撞上已被前一父删除的路径）= 冲突，交子任务裁决。
		applicable := (op == dirOpAdd && !exists) || (op == dirOpModify && exists && curSHA == baselineSHA)
		if applicable {
			if err := os.MkdirAll(filepath.Dir(workPath), 0o755); err != nil {
				return err
			}
			_ = os.Remove(workPath)
			if err := copyFilePerm(filepath.Join(artRoot, c.Path), workPath, os.FileMode(c.Mode)); err != nil {
				return err
			}
			return nil
		}
		// 冲突：保留子目录现状，父版本放进 .tako/conflicts 供模型裁决。
		conflicts = append(conflicts, directoryConflict{
			Path: c.Path, ParentTaskID: ref.TaskID, ParentRevision: ref.ResultRevision,
			Kind: "content", WorkSHA: curSHA, ParentSHA: c.SHA256,
		})
		dst := filepath.Join(workRoot, ".tako", "conflicts", ref.TaskID, c.Path)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		_ = os.Remove(dst)
		return copyFilePerm(filepath.Join(artRoot, c.Path), dst, os.FileMode(c.Mode))
	}
	for _, c := range manifest.Added {
		if err := apply(c, dirOpAdd); err != nil {
			return nil, err
		}
	}
	for _, c := range manifest.Modified {
		if err := apply(c, dirOpModify); err != nil {
			return nil, err
		}
	}
	for _, d := range manifest.Deleted {
		workPath := filepath.Join(workRoot, d.Path)
		curSHA, exists, err := hashFileIfExists(workPath)
		if err != nil {
			return nil, err
		}
		if !exists {
			continue // 幂等
		}
		baselineSHA := ""
		if be, ok := base[d.Path]; ok {
			baselineSHA = be.SHA256
		}
		if curSHA == baselineSHA {
			if err := os.Remove(workPath); err != nil {
				return nil, err
			}
			continue
		}
		conflicts = append(conflicts, directoryConflict{
			Path: d.Path, ParentTaskID: ref.TaskID, ParentRevision: ref.ResultRevision,
			Kind: "delete", WorkSHA: curSHA,
		})
	}
	return conflicts, nil
}

// hashFileIfExists 返回 (sha, exists, err)；不存在 = ("", false, nil)；非普通文件按错误。
func hashFileIfExists(path string) (string, bool, error) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !info.Mode().IsRegular() {
		return "", true, fmt.Errorf("路径不是普通文件：%s", path)
	}
	sha, _, err := hashFile(path)
	return sha, true, err
}

func (p *directoryProvider) saveConflicts(g *GroupHandle, taskID string, conflicts []directoryConflict) error {
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

func (p *directoryProvider) loadConflicts(g *GroupHandle, taskID string) ([]directoryConflict, error) {
	b, err := os.ReadFile(g.conflictsPath(taskID))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cs []directoryConflict
	if err := json.Unmarshal(b, &cs); err != nil {
		return nil, err
	}
	return cs, nil
}

// directoryOverlay 是工作现场的 prompt 补充（只指导模型，程序判定不依赖它）。
func directoryOverlay(shared bool, conflicts []directoryConflict) string {
	var b strings.Builder
	if shared {
		b.WriteString("你在本编排组共用的工作目录中工作，其他任务也会读写同一目录；原项目不会被直接改动。")
	} else {
		b.WriteString("你在一个隔离工作目录中工作，原项目不会被直接改动。")
	}
	b.WriteString("完成后用 submit_task_result 提交结果，")
	b.WriteString("artifacts 声明本次新增或修改的交付文件（相对路径）。")
	if len(conflicts) > 0 {
		b.WriteString("\n\n父任务结果与本目录存在冲突，需要你裁决（程序不会自动合并）：")
		for _, c := range conflicts {
			if c.Kind == "delete" {
				fmt.Fprintf(&b, "\n- %s：父任务 %s 删除了它，但本目录中它被修改过；请决定保留还是删除。", c.Path, c.ParentTaskID)
			} else {
				fmt.Fprintf(&b, "\n- %s：与父任务 %s 的版本冲突；父版本已放在 .tako/conflicts/%s/%s，请裁决后保留正确内容。", c.Path, c.ParentTaskID, c.ParentTaskID, c.Path)
			}
		}
		b.WriteString("\n（.tako 目录下的内容不会进入结果。）")
	}
	return b.String()
}

// ── AcceptResult：diff + 验证 + 固化 ─────────────────────────────────────────

func (p *directoryProvider) AcceptResult(ctx context.Context, g *GroupHandle, task AgentTask, revision int, result TaskResult, work TaskWork) (ImmutableResult, error) {
	workRoot := g.workDir(task.ID)
	if work.Root != workRoot {
		return ImmutableResult{}, fmt.Errorf("工作现场绑定不符：%s != %s", work.Root, workRoot)
	}
	bl, err := readBaseline(g)
	if err != nil {
		return ImmutableResult{}, err
	}
	if bl == nil {
		return ImmutableResult{}, fmt.Errorf("组基线尚未准备")
	}
	current, err := walkDirectoryFiles(workRoot)
	if err != nil {
		return ImmutableResult{}, err
	}
	cs := diffBaseline(bl, current)
	// 校验声明 artifact：必须是本次新增/修改的文件，且字节与清单一致（程序判定）。
	changes := map[string]directoryChange{}
	for _, c := range cs.Added {
		changes[c.Path] = c
	}
	for _, c := range cs.Modified {
		changes[c.Path] = c
	}
	for _, a := range result.Artifacts {
		clean, err := resolveArtifactPath(workRoot, a.Path)
		if err != nil {
			return ImmutableResult{}, err
		}
		c, ok := changes[clean]
		if !ok {
			return ImmutableResult{}, fmt.Errorf("%w: artifact 必须是本次新增或修改的文件：%s", errSubmitBad, clean)
		}
		sha, _, err := hashFile(filepath.Join(workRoot, clean))
		if err != nil || sha != c.SHA256 {
			return ImmutableResult{}, fmt.Errorf("%w: artifact 字节与清单不符：%s", errSubmitBad, clean)
		}
	}
	if g.VerifyCommand != "" {
		if err := runVerifyCommand(ctx, g, task.ID, workRoot, g.VerifyCommand); err != nil {
			return ImmutableResult{}, err
		}
	}
	manifestBytes, err := json.Marshal(cs)
	if err != nil {
		return ImmutableResult{}, err
	}
	sum := sha256.Sum256(manifestBytes)
	reference := hex.EncodeToString(sum[:])
	var total int64
	for _, c := range cs.Added {
		total += fileSizeOrZero(filepath.Join(workRoot, c.Path))
	}
	for _, c := range cs.Modified {
		total += fileSizeOrZero(filepath.Join(workRoot, c.Path))
	}
	if p.reserve != nil {
		release, err := p.reserve(ctx, g.TenantID, total)
		if err != nil {
			return ImmutableResult{}, err
		}
		defer release()
	}
	// artifacts/ 固化**全部**新增/修改文件的字节（父结果应用与发布都要逐路径取字节）；
	// result.json 清单只列模型声明的交付物（artifact 端点读面子集）。
	fullFiles := make([]TaskArtifact, 0, len(cs.Added)+len(cs.Modified))
	for _, c := range cs.Added {
		fullFiles = append(fullFiles, TaskArtifact{Path: c.Path, Kind: "file"})
	}
	for _, c := range cs.Modified {
		fullFiles = append(fullFiles, TaskArtifact{Path: c.Path, Kind: "file"})
	}
	return finalizeImmutableResultLocked(g, task.ID, revision, workspaceProviderDirectory, fullFiles, workRoot, reference, map[string][]byte{"manifest.json": manifestBytes}, result.Artifacts)
}

func fileSizeOrZero(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

// diffBaseline 比对基线与当前清单，得出新增/修改/删除（各按 path 排序）。
// 修改 = 内容哈希或权限位变化（仅改权限也要能发布）。
func diffBaseline(bl *directoryBaseline, current []directoryFileEntry) directoryChangeSet {
	base := map[string]directoryFileEntry{}
	for _, e := range bl.Files {
		base[e.Path] = e
	}
	cur := map[string]directoryFileEntry{}
	for _, e := range current {
		cur[e.Path] = e
	}
	var cs directoryChangeSet
	for _, e := range current {
		be, ok := base[e.Path]
		if !ok {
			cs.Added = append(cs.Added, directoryChange{Path: e.Path, SHA256: e.SHA256, Mode: e.Mode})
		} else if be.SHA256 != e.SHA256 || be.Mode != e.Mode {
			cs.Modified = append(cs.Modified, directoryChange{Path: e.Path, SHA256: e.SHA256, Mode: e.Mode})
		}
	}
	for _, e := range bl.Files {
		if _, ok := cur[e.Path]; !ok {
			cs.Deleted = append(cs.Deleted, directoryDelete{Path: e.Path})
		}
	}
	return cs
}

// readResultManifest 读不可变结果里的变更清单。
func readResultManifest(g *GroupHandle, ref ResultRef) (*directoryChangeSet, error) {
	b, err := os.ReadFile(filepath.Join(g.resultDir(ref.TaskID, ref.ResultRevision), "manifest.json"))
	if err != nil {
		return nil, err
	}
	var cs directoryChangeSet
	if err := json.Unmarshal(b, &cs); err != nil {
		return nil, fmt.Errorf("结果清单损坏：%w", err)
	}
	return &cs, nil
}

// ── ReadArtifact ────────────────────────────────────────────────────────────

func (p *directoryProvider) ReadArtifact(ctx context.Context, g *GroupHandle, ref ResultRef, path string) ([]byte, error) {
	return readResultArtifact(g, ref, path)
}

// ── PreparePublication / Publish / ReconcilePublication ──────────────────────

func (p *directoryProvider) PreparePublication(ctx context.Context, g *GroupHandle, final ImmutableResult) (PublicationPlan, error) {
	rel, err := g.Locker.Lock(ctx, []string{g.groupLockKey()})
	if err != nil {
		return PublicationPlan{}, err
	}
	defer rel()
	cs, err := readResultManifest(g, ResultRef{TaskID: final.TaskID, ResultRevision: final.ResultRevision})
	if err != nil {
		return PublicationPlan{}, err
	}
	bl, err := readBaseline(g)
	if err != nil {
		return PublicationPlan{}, err
	}
	base := map[string]directoryFileEntry{}
	if bl != nil {
		for _, e := range bl.Files {
			base[e.Path] = e
		}
	}
	now := time.Now().UTC()
	pub := directoryPublication{
		FormatVersion: 1, TaskID: final.TaskID, ResultRevision: final.ResultRevision,
		CreatedAt: now, UpdatedAt: now,
	}
	addEntry := func(path, op, targetSHA string, mode uint32) {
		baselineSHA := ""
		if be, ok := base[path]; ok {
			baselineSHA = be.SHA256
		}
		status := classifyProjectPath(filepath.Join(g.ProjectRoot, path), op, baselineSHA, targetSHA)
		pub.Entries = append(pub.Entries, directoryPubEntry{
			Path: path, Op: op, BaselineSHA: baselineSHA, TargetSHA: targetSHA, Mode: mode,
			Status: status, UpdatedAt: now,
		})
	}
	for _, c := range cs.Added {
		addEntry(c.Path, dirOpAdd, c.SHA256, c.Mode)
	}
	for _, c := range cs.Modified {
		addEntry(c.Path, dirOpModify, c.SHA256, c.Mode)
	}
	for _, d := range cs.Deleted {
		addEntry(d.Path, dirOpDelete, "", 0)
	}
	// epoch 由引擎在调用 Publish 时给定；PreparePublication 先落一份无 epoch 的计划草稿，
	// Publish 会把它绑到具体 epoch。这里用 final 的 revision 暂存于 publication/draft.json。
	if err := writePublicationRecord(g, publicationDraftPath(g), &pub); err != nil {
		return PublicationPlan{}, err
	}
	summary := fmt.Sprintf("%d 项变更：%d 新增 %d 修改 %d 删除", len(pub.Entries), len(cs.Added), len(cs.Modified), len(cs.Deleted))
	return PublicationPlan{Summary: summary}, nil
}

// classifyProjectPath 按原项目当前内容判定一路径的初始发布状态。
func classifyProjectPath(projectPath, op, baselineSHA, targetSHA string) string {
	curSHA, exists, err := hashFileIfExists(projectPath)
	if err != nil {
		return dirPubConflict
	}
	switch op {
	case dirOpAdd:
		if !exists {
			return dirPubPending
		}
		if curSHA == targetSHA {
			return dirPubDone
		}
	case dirOpModify:
		if exists && curSHA == targetSHA {
			return dirPubDone
		}
		if exists && curSHA == baselineSHA {
			return dirPubPending
		}
	case dirOpDelete:
		if !exists {
			return dirPubDone
		}
		if curSHA == baselineSHA {
			return dirPubPending
		}
	}
	return dirPubConflict
}

func publicationDraftPath(g *GroupHandle) string {
	return filepath.Join(g.Dir, "publication", "draft.json")
}

func writePublicationRecord(g *GroupHandle, path string, pub *directoryPublication) error {
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

func readPublicationRecord(g *GroupHandle, epoch int64) (*directoryPublication, error) {
	b, err := os.ReadFile(g.publicationPath(epoch))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var pub directoryPublication
	if err := json.Unmarshal(b, &pub); err != nil {
		return nil, fmt.Errorf("发布记录损坏：%w", err)
	}
	return &pub, nil
}

func (p *directoryProvider) Publish(ctx context.Context, g *GroupHandle, final ImmutableResult, epoch int64) (PublishOutcome, error) {
	rel, err := g.Locker.Lock(ctx, []string{g.groupLockKey()})
	if err != nil {
		return PublishOutcome{}, err
	}
	defer rel()
	pub, err := readPublicationRecord(g, epoch)
	if err != nil {
		return PublishOutcome{}, err
	}
	if pub == nil {
		// 首次发布：把 PreparePublication 落的草稿绑到本 epoch。
		draft, err := readPublicationDraft(g)
		if err != nil || draft == nil {
			return PublishOutcome{}, fmt.Errorf("发布计划不存在，请先 PreparePublication")
		}
		draft.Epoch = epoch
		pub = draft
		if err := writePublicationRecord(g, g.publicationPath(epoch), pub); err != nil {
			return PublishOutcome{}, err
		}
	}
	return p.applyPublication(g, final, pub, epoch)
}

func (p *directoryProvider) ReconcilePublication(ctx context.Context, g *GroupHandle, epoch int64) (PublishOutcome, error) {
	rel, err := g.Locker.Lock(ctx, []string{g.groupLockKey()})
	if err != nil {
		return PublishOutcome{}, err
	}
	defer rel()
	pub, err := readPublicationRecord(g, epoch)
	if err != nil {
		return PublishOutcome{}, err
	}
	if pub == nil {
		return PublishOutcome{}, fmt.Errorf("发布记录不存在，无法对账 epoch %d", epoch)
	}
	final, err := readImmutableResult(g, ResultRef{TaskID: pub.TaskID, ResultRevision: pub.ResultRevision})
	if err != nil {
		return PublishOutcome{}, err
	}
	// Directory 的 Publish 与 Reconcile 共用同一逐路径核现场 + 应用核心：
	// done 核相符、pending 核基线后继续、计划外内容受阻。崩溃后调本口即可续。
	return p.applyPublication(g, final, pub, epoch)
}

func readPublicationDraft(g *GroupHandle) (*directoryPublication, error) {
	b, err := os.ReadFile(publicationDraftPath(g))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var pub directoryPublication
	if err := json.Unmarshal(b, &pub); err != nil {
		return nil, err
	}
	return &pub, nil
}

// applyPublication 逐路径核现场并应用（doc/74 §8.3）：每路径完成后原子更新记录，
// 崩溃后能分辨实际进度。部分发布如实记录，组不能 succeeded。
func (p *directoryProvider) applyPublication(g *GroupHandle, final ImmutableResult, pub *directoryPublication, epoch int64) (PublishOutcome, error) {
	artRoot := filepath.Join(g.resultDir(final.TaskID, final.ResultRevision), "artifacts")
	changed, recovery := false, false
	for i := range pub.Entries {
		e := &pub.Entries[i]
		switch e.Status {
		case dirPubDone:
			if !projectMatchesTarget(filepath.Join(g.ProjectRoot, e.Path), e) {
				e.Status, e.Error = dirPubConflict, taskErrPublicationRecovery
				recovery = true
			}
		case dirPubConflict:
			if e.Error == taskErrPublicationRecovery {
				recovery = true
			} else {
				changed = true
			}
		case dirPubPending:
			status, errCode := p.applyPubEntry(g, artRoot, e)
			e.Status, e.Error = status, errCode
			if status == dirPubConflict {
				if errCode == taskErrPublicationRecovery {
					recovery = true
				} else {
					changed = true
				}
			}
		}
		e.UpdatedAt = time.Now().UTC()
		if err := writePublicationRecord(g, g.publicationPath(epoch), pub); err != nil {
			return PublishOutcome{}, err
		}
	}
	switch {
	case recovery:
		return PublishOutcome{Status: publishOutcomeBlocked, ErrorCode: taskErrPublicationRecovery, Detail: "发布现场与记录不符，需人工检查"}, nil
	case changed:
		return PublishOutcome{Status: publishOutcomeBlocked, ErrorCode: taskErrWorkspaceChanged, Detail: "原项目已前进，旧结果不能覆盖"}, nil
	default:
		return PublishOutcome{Status: publishOutcomePublished}, nil
	}
}

// projectMatchesTarget 核原项目当前内容是否等于该路径的目标（delete = 不存在）。
func projectMatchesTarget(projectPath string, e *directoryPubEntry) bool {
	curSHA, exists, err := hashFileIfExists(projectPath)
	if err != nil {
		return false
	}
	if e.Op == dirOpDelete {
		return !exists
	}
	return exists && curSHA == e.TargetSHA
}

// applyPubEntry 应用一条 pending 记录：再核基线 → 原子写/删 → 返回新状态。
func (p *directoryProvider) applyPubEntry(g *GroupHandle, artRoot string, e *directoryPubEntry) (string, string) {
	projectPath := filepath.Join(g.ProjectRoot, e.Path)
	curSHA, exists, err := hashFileIfExists(projectPath)
	if err != nil {
		return dirPubConflict, taskErrPublicationRecovery
	}
	switch e.Op {
	case dirOpDelete:
		if !exists {
			return dirPubDone, ""
		}
		if curSHA != e.BaselineSHA {
			return dirPubConflict, taskErrWorkspaceChanged
		}
		if err := os.Remove(projectPath); err != nil {
			return dirPubConflict, taskErrPublicationRecovery
		}
		return dirPubDone, ""
	case dirOpAdd, dirOpModify:
		if exists && curSHA == e.TargetSHA {
			return dirPubDone, "" // 幂等（含崩溃后重入）
		}
		if e.Op == dirOpAdd && exists {
			return dirPubConflict, taskErrWorkspaceChanged
		}
		if e.Op == dirOpModify && (!exists || curSHA != e.BaselineSHA) {
			return dirPubConflict, taskErrWorkspaceChanged
		}
		src := filepath.Join(artRoot, e.Path)
		content, err := os.ReadFile(src)
		if err != nil {
			return dirPubConflict, taskErrPublicationRecovery
		}
		if err := os.MkdirAll(filepath.Dir(projectPath), 0o755); err != nil {
			return dirPubConflict, taskErrPublicationRecovery
		}
		if err := atomicWriteFile(projectPath, content, os.FileMode(e.Mode)); err != nil {
			return dirPubConflict, taskErrPublicationRecovery
		}
		if err := os.Chmod(projectPath, os.FileMode(e.Mode)); err != nil {
			return dirPubConflict, taskErrPublicationRecovery
		}
		return dirPubDone, ""
	}
	return dirPubConflict, taskErrPublicationRecovery
}

// ── Cleanup ─────────────────────────────────────────────────────────────────

func (p *directoryProvider) Cleanup(ctx context.Context, g *GroupHandle, now time.Time, archiveTTL time.Duration) (CleanupReport, error) {
	rel, err := g.Locker.Lock(ctx, []string{g.groupLockKey()})
	if err != nil {
		return CleanupReport{}, err
	}
	defer rel()
	var report CleanupReport
	// 过窗判定：state.json 的 updatedAt 距今不足 archiveTTL = 保留。
	if st, err := readWorkspaceGroupState(g.Dir); err == nil && st != nil {
		if now.Sub(st.UpdatedAt) < archiveTTL {
			report.Kept = append(report.Kept, "组工作区（未过清理窗口）")
			return report, nil
		}
	}
	// 部分发布未收口（任一记录有 pending/conflict）= 保留全部现场。
	unresolved, err := publicationUnresolved(g)
	if err != nil {
		return CleanupReport{}, err
	}
	if unresolved {
		report.Kept = append(report.Kept, "组工作区（发布未收口）")
		return report, nil
	}
	// 删大体积工作现场与基线；保留不可变结果、发布记录与验证证据（交付定位与排查要用）。
	for _, target := range []struct{ path, label string }{
		{filepath.Join(g.Dir, "work"), "工作目录"},
		{g.baselineDir(), "基线副本"},
		{filepath.Join(g.providerDir(), "conflicts"), "冲突记录"},
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

// publicationUnresolved 扫 publication/*.json，任一记录有非 done 路径 = 未收口。
func publicationUnresolved(g *GroupHandle) (bool, error) {
	dir := filepath.Join(g.Dir, "publication")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") || e.Name() == "draft.json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return true, nil // 读不了 = 无法证明安全 = 保留
		}
		var pub directoryPublication
		if err := json.Unmarshal(b, &pub); err != nil {
			return true, nil
		}
		for _, en := range pub.Entries {
			if en.Status != dirPubDone {
				return true, nil
			}
		}
	}
	return false, nil
}
