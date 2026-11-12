// 本文件是不可变结果的固化与读取（方案真源 = doc/74 §5.2/§9.2）。
//
// 跨存储顺序（doc/74 §9.2）：引擎算候选 resultRevision → 本文件把结果完整写进共享卷
//（staging → 原子 rename 成 final）→ PG 再校验 run/epoch 并保存通用结果。
// final 已存在但 PG 未保存时，同候选 revision 重试核验内容后**复用**，不重复制造版本；
// 内容不同则拒绝——同一版本永远只有一份字节。
//
// 读侧（artifact 端点与交付定位）只认 result.json 清单：path 逐字命中清单才读，
// 目录、绝对路径、..、软链越界一律拒绝。结果缺失/不可读是独立的错误类别（API 409 117110）。
package runengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// errImmutableResultMissing：结果目录或 result.json 不存在/产物文件缺失。
// 与「内容损坏」分开：缺失可能是尚未固化（重试会补上），损坏必须人工介入。
var errImmutableResultMissing = errors.New("不可变结果不存在")

// errImmutableIdentityConflict：候选 revision 已固化且身份不同（内容或引用变了）。
// 提交链收到它就把 revision +1 重试——同一版本永远只有一份字节（doc/74 §9.2）。
var errImmutableIdentityConflict = errors.New("不可变结果身份不一致")

// finalizeImmutableResult 把已验证的产物固化成不可变结果（doc/74 §9.2 的 Provider 半）。
// 自取 Task 锁的独立入口（测试与旁路用）；引擎提交链已持 Task 锁时走 Locked 变体。
// declared 为 nil 时清单即 artifacts（Git 与拍2 测试）；非 nil 时见 Locked 变体说明。
func finalizeImmutableResult(ctx context.Context, g *GroupHandle, taskID string, revision int, provider string, artifacts []TaskArtifact, srcRoot, reference string, extraFiles map[string][]byte, declared []TaskArtifact) (ImmutableResult, error) {
	rel, err := g.Locker.Lock(ctx, []string{g.taskLockKey(taskID)})
	if err != nil {
		return ImmutableResult{}, err
	}
	defer rel()
	return finalizeImmutableResultLocked(g, taskID, revision, provider, artifacts, srcRoot, reference, extraFiles, declared)
}

// finalizeImmutableResultLocked 是固化核心，**调用方必须已持有该 Task 的共享锁**
//（doc/74 §9.2：验证、固化与 PG 保存在同一把 Task 锁内串行）。
//
// artifacts 是**要逐字复制进 artifacts/ 的全部文件**；declared 是**写进 result.json 清单、
// 供 artifact 端点读取的子集**。两者分开是因为 Directory 的父结果应用与发布需要每个
// 改动路径的字节（artifacts 全量），而端点只暴露模型声明的交付物（declared 子集）。
// declared 为 nil → 清单即 artifacts 全量（Git 与拍2 测试沿用此路）；非 nil → 校验它是
// artifacts 的子集后作为清单。extraFiles 是 Provider 随结果一起固化的内部状态文件
//（如 Directory 的 manifest.json），键是结果目录内相对路径。
// final 已存在且身份一致 = 复用（返回原 createdAt），不一致 = 拒绝。
func finalizeImmutableResultLocked(g *GroupHandle, taskID string, revision int, provider string, artifacts []TaskArtifact, srcRoot, reference string, extraFiles map[string][]byte, declared []TaskArtifact) (ImmutableResult, error) {
	cleaned := make([]TaskArtifact, 0, len(artifacts))
	copiedPaths := make(map[string]bool, len(artifacts))
	for _, a := range artifacts {
		clean, err := resolveArtifactPath(srcRoot, a.Path)
		if err != nil {
			return ImmutableResult{}, err
		}
		cleaned = append(cleaned, TaskArtifact{Path: clean, Kind: a.Kind})
		copiedPaths[clean] = true
	}
	manifest := cleaned
	if declared != nil {
		manifest = make([]TaskArtifact, 0, len(declared))
		for _, a := range declared {
			clean, err := resolveArtifactPath(srcRoot, a.Path)
			if err != nil {
				return ImmutableResult{}, err
			}
			if !copiedPaths[clean] {
				return ImmutableResult{}, fmt.Errorf("%w: 声明的交付物不在改动集里：%s", errArtifactBad, a.Path)
			}
			manifest = append(manifest, TaskArtifact{Path: clean, Kind: a.Kind})
		}
	}

	ref := ResultRef{TaskID: taskID, ResultRevision: revision}
	if existing, err := readImmutableResult(g, ref); err == nil {
		if sameImmutableIdentity(existing, provider, reference, manifest) {
			return existing, nil
		}
		return ImmutableResult{}, fmt.Errorf("%w: 结果 %s/%d 已固化且内容不同，拒绝覆盖", errImmutableIdentityConflict, taskID, revision)
	} else if !errors.Is(err, errImmutableResultMissing) {
		return ImmutableResult{}, err
	}

	parent := filepath.Dir(g.resultDir(taskID, revision))
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return ImmutableResult{}, err
	}
	staging, err := os.MkdirTemp(parent, ".staging-")
	if err != nil {
		return ImmutableResult{}, err
	}
	defer os.RemoveAll(staging) // 任何失败路径都不留半个结果；成功后 staging 已 rename 走，RemoveAll 落空

	for _, a := range artifacts {
		clean, err := resolveArtifactPath(srcRoot, a.Path)
		if err != nil {
			return ImmutableResult{}, err
		}
		dst := filepath.Join(staging, "artifacts", clean)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return ImmutableResult{}, err
		}
		if err := copyFileInto(filepath.Join(srcRoot, clean), dst); err != nil {
			return ImmutableResult{}, err
		}
	}
	for name, content := range extraFiles {
		if name == "" || name == "result.json" || filepath.IsAbs(name) || strings.Contains(name, "..") ||
			name == "artifacts" || strings.HasPrefix(name, "artifacts/") {
			return ImmutableResult{}, fmt.Errorf("%w: 结果内部文件名不合法：%s", errArtifactBad, name)
		}
		dst := filepath.Join(staging, name)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return ImmutableResult{}, err
		}
		if err := atomicWriteFile(dst, content, 0o644); err != nil {
			return ImmutableResult{}, err
		}
	}
	res := ImmutableResult{
		TaskID:         taskID,
		ResultRevision: revision,
		Provider:      provider,
		Artifacts:      manifest,
		CreatedAt:      time.Now().UTC(),
		Reference:      reference,
	}
	b, err := json.Marshal(res)
	if err != nil {
		return ImmutableResult{}, err
	}
	if err := atomicWriteFile(filepath.Join(staging, "result.json"), b, 0o644); err != nil {
		return ImmutableResult{}, err
	}
	if err := os.Rename(staging, g.resultDir(taskID, revision)); err != nil {
		return ImmutableResult{}, fmt.Errorf("固化结果失败：%w", err)
	}
	syncDir(parent)
	return res, nil
}

// sameImmutableIdentity 比对重试与已固化结果的身份：Provider、不透明引用、产物清单。
// createdAt 不参与——复用返回的就是原结果，时间当然不同。
func sameImmutableIdentity(existing ImmutableResult, provider, reference string, artifacts []TaskArtifact) bool {
	if existing.Provider != provider || existing.Reference != reference || len(existing.Artifacts) != len(artifacts) {
		return false
	}
	for i, a := range existing.Artifacts {
		if a != artifacts[i] {
			return false
		}
	}
	return true
}

// copyFileInto 把 src 的字节复制进 dst（新文件，0644）。调用方已保证 src 是普通文件。
func copyFileInto(src, dst string) error { return copyFilePerm(src, dst, 0o644) }

// copyFilePerm 复制字节并按 perm 设权限（新文件，O_EXCL 不覆盖）。
// 工作目录复制要保留基线权限——脚本的执行位丢了，verifyCommand 就跑不起来。
func copyFilePerm(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// readImmutableResult 读 result.json。缺失 = errImmutableResultMissing；损坏 = 普通错误。
func readImmutableResult(g *GroupHandle, ref ResultRef) (ImmutableResult, error) {
	b, err := os.ReadFile(filepath.Join(g.resultDir(ref.TaskID, ref.ResultRevision), "result.json"))
	if os.IsNotExist(err) {
		return ImmutableResult{}, errImmutableResultMissing
	}
	if err != nil {
		return ImmutableResult{}, err
	}
	var res ImmutableResult
	if err := json.Unmarshal(b, &res); err != nil {
		return ImmutableResult{}, fmt.Errorf("结果文件损坏：%w", err)
	}
	if res.TaskID != ref.TaskID || res.ResultRevision != ref.ResultRevision {
		return ImmutableResult{}, fmt.Errorf("结果文件身份不符：%s/%d", ref.TaskID, ref.ResultRevision)
	}
	return res, nil
}

// readResultArtifact 读结果产物字节（doc/74 §4.4 的存储半）：
// path 必须**逐字**命中当前结果版本的清单（不做 trim/clean 后再匹配——清单里是什么就是什么）；
// 命中后再做真实路径包含检查，软链越界拒绝。文件缺失与结果缺失同类（API 409 117110）。
func readResultArtifact(g *GroupHandle, ref ResultRef, path string) ([]byte, error) {
	res, err := readImmutableResult(g, ref)
	if err != nil {
		return nil, err
	}
	hit := false
	for _, a := range res.Artifacts {
		if a.Path == path {
			hit = true
			break
		}
	}
	if !hit {
		return nil, fmt.Errorf("%w: artifact 不在结果清单里：%s", errArtifactBad, path)
	}
	root := filepath.Join(g.resultDir(ref.TaskID, ref.ResultRevision), "artifacts")
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, errImmutableResultMissing
	}
	real, err := filepath.EvalSymlinks(filepath.Join(realRoot, path))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errImmutableResultMissing
		}
		return nil, err
	}
	if real != realRoot && !pathWithin(realRoot, real) {
		return nil, fmt.Errorf("%w: artifact 越出结果目录：%s", errArtifactBad, path)
	}
	info, err := os.Stat(real)
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w: artifact 不是普通文件：%s", errArtifactBad, path)
	}
	return os.ReadFile(real)
}

// writeVerificationEvidence 写验证证据（verification/<taskId>.json）：
// 自取 Task 锁的独立入口。读面（workspaceSummary）取 status，收尾时带退出码与输出位置。
func writeVerificationEvidence(ctx context.Context, g *GroupHandle, ev workspaceVerificationEvidence) error {
	rel, err := g.Locker.Lock(ctx, []string{g.taskLockKey(ev.TaskID)})
	if err != nil {
		return err
	}
	defer rel()
	return writeVerificationEvidenceLocked(g, ev)
}

// writeVerificationEvidenceLocked 是证据写入核心，调用方必须已持有该 Task 的共享锁。
func writeVerificationEvidenceLocked(g *GroupHandle, ev workspaceVerificationEvidence) error {
	if err := os.MkdirAll(filepath.Dir(g.verificationPath(ev.TaskID)), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	return atomicWriteFile(g.verificationPath(ev.TaskID), b, 0o644)
}