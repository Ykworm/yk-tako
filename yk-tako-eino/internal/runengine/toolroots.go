// toolroots.go 是租户工具根的上传写面（doc/52 §6.1、D36，F 段）。
//
// 与 tools.go 不是一回事：那个是工具环的第一个工具（get_time）与统一错误出口，
// 本文件管的是「租户自己补一个可执行文件」的写面。
//
// 工具根 = `<tenant>/tools/`，与 `skills/` 平级。它与技能根方向相反：
// 技能根按「用过才给」的证据闸收窄，工具根**按租户常开**（F2 把整棵根加进
// 只读白名单并前置进 PATH）——缺一个 `ffmpeg`，团队自己补，不必等运维。
//
// 这一拍只有三个端点，且**不进表**：列表就是 `os.ReadDir`。表会引入
// 「盘与行漂移、rescan 对账」那一整套问题，而工具目录里没有需要解析的正文。
//
// 落盘五步（与技能安装同一套形状）：收 → 占额度 → 落 `.staging` → 锁内
// 「旧目录进 `.trash` + rename」→ 报。中途失败时正式目录一个字节没动：
// staging 在 defer 里删掉，rename 是原子的。
package runengine

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// toolMaxBytes：单文件上限。整包前缀（解释器那类几万个文件）走运维同步或软链，
// 不走上传——多文件是方案内后置阶段（doc/52 §10 D36）。
const toolMaxBytes = 128 << 20

// toolNamePattern：工具名变成路径的一段，所以只认小写字母、数字与连字符。
// 首字符限字母数字（`-foo` 会被当成命令行选项）；64 字符封顶。
var toolNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// reservedToolNames：这些名字会被子进程直接解析到，租户传一个同名的会把
// 整个租户的脚本换成「他以为的 bash」。不是洁癖，是劫持面。
var reservedToolNames = map[string]bool{"bash": true, "sh": true, "env": true}

// tenantToolsRoot 是工具根：`<workspace>/tenants/<tid>/tools`。
func tenantToolsRoot(workspaceRoot, tenantID string) string {
	return filepath.Join(tenantStorageRoot(workspaceRoot, tenantID), "tools")
}

// toolBody 是列表与上传响应里的一行。字段序是契约（doc/52 §6.1）：
// name → exe → path → bytes → updatedAt。
//
// exe 是**现算**的（`bin/<name>` 存在且有执行位），不进任何表——工具没有行。
// path 指向可执行文件本身（上传物就是它）；前缀里没有同名可执行文件时 exe=false，
// 但 path 仍写那个约定的位置，前端不必自己拼。
type toolBody struct {
	Name      string `json:"name"`
	Exe       bool   `json:"exe"`
	Path      string `json:"path"`
	Bytes     int64  `json:"bytes"`
	UpdatedAt string `json:"updatedAt"`
}

// toolExecPath 是目录约定：一个工具一个前缀，`<tools>/<name>/bin/<name>`。
func toolExecPath(root, name string) string {
	return filepath.Join(root, name, "bin", name)
}

// toolNameProblem 校验用户给的 name（**不是上传的文件名**——浏览器可能把路径带进来）。
// 返回空串表示通过；否则是给人看的拒绝原因。
func toolNameProblem(name string) string {
	if name == "" {
		return "工具名不能为空"
	}
	if !toolNamePattern.MatchString(name) {
		return "工具名只能是小写字母、数字和连字符（64 字符以内）"
	}
	if reservedToolNames[name] {
		return fmt.Sprintf("%q 是保留名，不能用作工具名", name)
	}
	return ""
}

// readToolEntry 把一个前缀目录读成响应行。目录不存在返回 false。
func readToolEntry(root, name string) (toolBody, bool) {
	prefix := filepath.Join(root, name)
	info, err := os.Stat(prefix)
	if err != nil || !info.IsDir() {
		return toolBody{}, false
	}
	exe := toolExecPath(root, name)
	exeInfo, err := os.Stat(exe)
	hasExe := err == nil && exeInfo.Mode().IsRegular() && exeInfo.Mode().Perm()&0o111 != 0
	mod := info.ModTime()
	if hasExe {
		mod = exeInfo.ModTime()
	}
	return toolBody{
		Name:      name,
		Exe:       hasExe,
		Path:      exe,
		Bytes:     dirBytes(prefix),
		UpdatedAt: mod.UTC().Format(time.RFC3339),
	}, true
}

// listTools 扫工具根。跳过 `.` 开头的目录不是洁癖：`.staging` 里在途上传的
// `bin/<name>` 如果被列出来，就等于半成品抢先被使用（F2 的 PATH 前置同理）。
func listTools(root string) []toolBody {
	out := []toolBody{}
	entries, err := os.ReadDir(root)
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if b, ok := readToolEntry(root, e.Name()); ok {
			out = append(out, b)
		}
	}
	return out
}

// getTools 是 GET /api/tools：列表只读，所有成员可读（含 viewer）。
func (s *Server) getTools(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	root := tenantToolsRoot(s.mediaRoot, p.TenantID)
	writeEnvelope(c, http.StatusOK, codeOK, "", gin.H{"tools": listTools(root)})
}

// postTools 是 POST /api/tools：multipart `name` + `file`，同名即覆盖。
//
// 额度用 ReserveStorage 先占再落盘（调用形状与媒体上传一致）：两个并发上传
// 各自「算一遍目录再写」会同时通过。release 在返回前调——字节已经在目录里。
func (s *Server) postTools(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	ctx := withScope(c.Request.Context(), dataScope{TenantID: p.TenantID, UserID: p.UserID, Scope: "tenant"})
	// 先把请求体卡死，再谈参数：不卡体积，一个大文件能顶爆 PVC。
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, toolMaxBytes+(1<<20))
	fh, err := c.FormFile("file")
	if err != nil {
		if strings.Contains(err.Error(), "too large") {
			writeEnvelope(c, http.StatusRequestEntityTooLarge, codeToolTooLarge,
				fmt.Sprintf("工具文件超过上限（%d 字节）", toolMaxBytes), nil)
			return
		}
		writeEnvelope(c, http.StatusBadRequest, codeToolBadInput, "请用 multipart 上传 name 与 file", nil)
		return
	}
	name := strings.TrimSpace(c.PostForm("name"))
	if msg := toolNameProblem(name); msg != "" {
		writeEnvelope(c, http.StatusBadRequest, codeToolBadInput, msg, nil)
		return
	}
	if fh.Size > toolMaxBytes {
		writeEnvelope(c, http.StatusRequestEntityTooLarge, codeToolTooLarge,
			fmt.Sprintf("工具文件超过上限（%d 字节）", toolMaxBytes), nil)
		return
	}
	f, err := fh.Open()
	if err != nil {
		internal(c, err)
		return
	}
	data, err := io.ReadAll(io.LimitReader(f, toolMaxBytes+1))
	_ = f.Close()
	if err != nil {
		internal(c, err)
		return
	}
	if len(data) > toolMaxBytes {
		writeEnvelope(c, http.StatusRequestEntityTooLarge, codeToolTooLarge,
			fmt.Sprintf("工具文件超过上限（%d 字节）", toolMaxBytes), nil)
		return
	}
	if len(data) == 0 {
		writeEnvelope(c, http.StatusBadRequest, codeToolBadInput, "上传的文件是空的", nil)
		return
	}
	_, _, _, maxBytes, err := s.store.TenantQuota(ctx, p.TenantID)
	if err != nil {
		internal(c, err)
		return
	}
	release, err := s.store.ReserveStorage(ctx, p.TenantID, int64(len(data)), maxBytes, func() int64 {
		return directoryBytes(tenantStorageRoot(s.mediaRoot, p.TenantID))
	})
	if errors.Is(err, errStorageQuota) {
		writeEnvelope(c, http.StatusTooManyRequests, codeQuota, "这个工作区的额度用完了", nil)
		return
	}
	if err != nil {
		internal(c, err)
		return
	}
	defer release()

	// 落 staging：直接写目标目录 = 某场 run 可能 exec 到写了一半的文件。
	root := tenantToolsRoot(s.mediaRoot, p.TenantID)
	stageID, err := newRowID("")
	if err != nil {
		internal(c, err)
		return
	}
	stage := filepath.Join(root, ".staging", stageID)
	execPath := filepath.Join(stage, "bin", name)
	if err := os.MkdirAll(filepath.Dir(execPath), 0o755); err != nil {
		internal(c, err)
		return
	}
	defer os.RemoveAll(stage)
	if err := os.WriteFile(execPath, data, 0o755); err != nil {
		internal(c, err)
		return
	}
	// 锁内只搬目录：旧前缀进 `.trash/<name>-<ts>`，再把 staging rename 到位。
	// 与技能安装排同一队（普通写档），不另造一套并发规则。
	err = s.store.withSkillLock(ctx, p.TenantID, false, func() error {
		// 本次上传的 staging 豁免：它就是下面 rename 的源，删了自己就没得搬。
		sweepTempDirsKeeping(root, time.Now(), stagingKeep(root, stage))
		if _, err := moveAsideToTrash(root, name); err != nil {
			return err
		}
		return os.Rename(stage, filepath.Join(root, name))
	})
	if err != nil {
		internal(c, err)
		return
	}
	s.publishEvent(ctx, p.TenantID, eventToolsChanged, name)
	body, _ := readToolEntry(root, name)
	writeEnvelope(c, http.StatusOK, codeOK, "", gin.H{"tool": body})
}

// deleteTool 是 DELETE /api/tools/:name：前缀目录移进 `.trash`。
//
// 名字先过同一套白名单：`..` 之类的参数不能进路径拼接——`filepath.Join(root, "..")`
// 是租户根，移走它就是移走这个租户的全部东西。不合规则与「不存在」同字节（116119）。
func (s *Server) deleteTool(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	name := c.Param("name")
	ctx := withScope(c.Request.Context(), dataScope{TenantID: p.TenantID, UserID: p.UserID, Scope: "tenant"})
	root := tenantToolsRoot(s.mediaRoot, p.TenantID)
	err := s.store.withSkillLock(ctx, p.TenantID, false, func() error {
		sweepTempDirs(root, time.Now())
		if toolNameProblem(name) != "" {
			return skillWriteError(http.StatusNotFound, codeToolMissing, "找不到这个工具")
		}
		if _, ok := readToolEntry(root, name); !ok {
			return skillWriteError(http.StatusNotFound, codeToolMissing, "找不到这个工具")
		}
		_, err := moveAsideToTrash(root, name)
		return err
	})
	if err != nil {
		writeSkillWriteError(c, err)
		return
	}
	s.publishEvent(ctx, p.TenantID, eventToolsChanged, name)
	writeEnvelope(c, http.StatusOK, codeOK, "", gin.H{"status": "deleted"})
}
