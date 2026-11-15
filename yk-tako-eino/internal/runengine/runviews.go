// runviews.go 是对话页右侧工作区的两个只读读口（方案真源 = doc/78 §4）。
//
// 两个端点都建立在已有的存储与判定之上，不另起真源：
//
//	GET /api/runs/{runId}/changed-files  从 runlog 的 tool_call 行派生「这场 run 写过/改过哪些文件」
//	GET /api/runs/{runId}/tasks          读 run_tasks 看板（deleted 不列）
//
// 「改过哪些文件」的判定与压缩时的 fileOps 提取同源（firstPartyFileEffects + classifyToolCall +
// pathsFromArgs）：模型写在回复里的文字不参与，程序只认调用记录。派生后还要过两道过滤——
// 路径归一（不许越出 content/、不许 .tako/ 与 .media/）与磁盘校验（当前得是普通文件），
// 因为回执的用途是「点了能打开」，列一条打不开的路径不如不列。
package runengine

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// runChangedFile 是回执里的一行：路径相对 content/，op ∈ write|edit。
type runChangedFile struct {
	Path string `json:"path"`
	Op   string `json:"op"`
}

type runChangedFilesData struct {
	RunID RunID            `json:"runId"`
	Files []runChangedFile `json:"files"`
}

// runTaskRow 是 Todo 标签的一行；字段序即字节序（doc/78 §4.2）。
type runTaskRow struct {
	No          int       `json:"no"`
	Subject     string    `json:"subject"`
	Status      string    `json:"status"`
	Description string    `json:"description"`
	Result      string    `json:"result"`
	SkillName   string    `json:"skillName"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type runTasksData struct {
	RunID RunID        `json:"runId"`
	Rows  []runTaskRow `json:"rows"`
	Total int          `json:"total"`
	Done  int          `json:"done"`
}

// RunToolCalls 读一场 run 的工具调用行，按 seq 升序（= 模型调用的发生顺序）。
func (s *Store) RunToolCalls(ctx context.Context, runID RunID) ([]RunlogEntry, error) {
	var rows []RunlogEntry
	err := s.db.WithContext(ctx).
		Where("run_id = ? AND role = 'tool_call'", runID).
		Order("seq").Find(&rows).Error
	return rows, err
}

// changedFilesFromEntries 只做「桶判定 + 路径归一」，不做 stat；返回顺序即首次出现顺序。
// 同一路径再次出现不换位置、op 取最后一次（后面那次调用才是文件的最终状态）。
func changedFilesFromEntries(entries []RunlogEntry, registry map[string]string, contentDir string) []runChangedFile {
	files := make([]runChangedFile, 0)
	index := make(map[string]int)
	for _, e := range entries {
		var p toolCallPayload
		if err := json.Unmarshal([]byte(e.Payload), &p); err != nil {
			log.Printf("runviews: runlog 行 %s 的 payload 非法 JSON，跳过: %v", e.ID, err)
			continue
		}
		bucket, paths := classifyToolCall(p.Name, p.Args, registry)
		op := ""
		switch bucket {
		case "written":
			op = "write"
		case "edited":
			op = "edit"
		default:
			continue
		}
		for _, raw := range paths {
			rel, ok := normalizeContentRelPath(raw, contentDir)
			if !ok {
				continue
			}
			if i, seen := index[rel]; seen {
				files[i].Op = op
				continue
			}
			index[rel] = len(files)
			files = append(files, runChangedFile{Path: rel, Op: op})
		}
	}
	return files
}

// normalizeContentRelPath 把调用参数里的路径收成正斜杠相对路径（doc/78 §4.1 第 1–4 步）。
// 绝对路径只在落在 contentDir 内时才收；.tako/ 与 .media/ 是运行资源区，不是用户文件。
func normalizeContentRelPath(p, contentDir string) (string, bool) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", false
	}
	if filepath.IsAbs(p) {
		rel, err := filepath.Rel(contentDir, filepath.Clean(p))
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", false
		}
		p = rel
	}
	rel := path.Clean(filepath.ToSlash(p))
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return "", false
		}
	}
	if rel == ".tako" || strings.HasPrefix(rel, ".tako/") ||
		rel == ".media" || strings.HasPrefix(rel, ".media/") {
		return "", false
	}
	return rel, true
}

// pruneMissingChangedFiles 只留当前能在 content/ 里打开的文件：
// 越界锁与工具层同一把（resolveInWorkspace），目录与已删除的路径都丢掉。
func pruneMissingChangedFiles(contentDir string, files []runChangedFile) []runChangedFile {
	kept := make([]runChangedFile, 0, len(files))
	for _, f := range files {
		full, err := resolveInWorkspace(contentDir, f.Path)
		if err != nil {
			continue
		}
		info, err := os.Stat(full)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		kept = append(kept, f)
	}
	return kept
}

// getRunChangedFiles = GET /api/runs/{runId}/changed-files（doc/78 §4.1）。
//
//	200 {"code":0,"message":"","data":{"runId":"r_…","files":[{"path":"a.py","op":"write"}]}}
//	401 {"code":111102,"message":"请先登录","data":null}
//	404 {"code":112002,"message":"找不到这次运行","data":null}
//	409 {"code":117116,"message":"这个项目还需要迁移目录后才能使用","data":null}
//
// 鉴权与 GET /api/runs/{runId}/skills 同一套：看不见与不存在同字节。
func (s *Server) getRunChangedFiles(c *gin.Context) {
	runID := RunID(c.Param("runId"))
	v := s.viewRun(c, runID)
	if denyRunRead(c, v) {
		return
	}
	empty := runChangedFilesData{RunID: runID, Files: []runChangedFile{}}
	wid, err := s.store.SessionWorkspaceID(c.Request.Context(), v.SID)
	if err != nil {
		internal(c, err)
		return
	}
	if wid == "" || s.mediaRoot == "" {
		writeEnvelope(c, http.StatusOK, codeOK, "", empty)
		return
	}
	needs, err := projectLayoutNeedsMigration(s.mediaRoot, v.P.TenantID, wid)
	if err != nil {
		internal(c, err)
		return
	}
	if needs {
		writeLayoutUnmigrated(c)
		return
	}
	contentDir, _, err := ensureProjectLayout(s.mediaRoot, v.P.TenantID, wid)
	if err != nil {
		internal(c, err)
		return
	}
	entries, err := s.store.RunToolCalls(c.Request.Context(), runID)
	if err != nil {
		internal(c, err)
		return
	}
	files := pruneMissingChangedFiles(contentDir, changedFilesFromEntries(entries, firstPartyFileEffects, contentDir))
	writeEnvelope(c, http.StatusOK, codeOK, "", runChangedFilesData{RunID: runID, Files: files})
}

// getRunTasks = GET /api/runs/{runId}/tasks（doc/78 §4.2）：这场 run 的看板。
//
//	200 {"code":0,"message":"","data":{"runId":"r_…","rows":[…],"total":2,"done":1}}
//	401 {"code":111102,"message":"请先登录","data":null}
//	404 {"code":112002,"message":"找不到这次运行","data":null}
//
// 数据直接读 run_tasks（Store.ListTasks）：deleted 不列，task_no 升序。
// 不读盘，因此不判旧布局（没有 409）。
func (s *Server) getRunTasks(c *gin.Context) {
	runID := RunID(c.Param("runId"))
	v := s.viewRun(c, runID)
	if denyRunRead(c, v) {
		return
	}
	rows, err := s.store.ListTasks(c.Request.Context(), runID)
	if err != nil {
		internal(c, err)
		return
	}
	out := make([]runTaskRow, 0, len(rows))
	done := 0
	for _, r := range rows {
		if r.Status == taskStatusCompleted {
			done++
		}
		out = append(out, runTaskRow{
			No:          r.TaskNo,
			Subject:     r.Subject,
			Status:      r.Status,
			Description: r.Description,
			Result:      r.Result,
			SkillName:   r.SkillName,
			UpdatedAt:   r.UpdatedAt,
		})
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", runTasksData{
		RunID: runID,
		Rows:  out,
		Total: len(out),
		Done:  done,
	})
}
