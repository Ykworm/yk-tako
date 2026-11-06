// tasktools.go 是任务看板（tasktracker）的第一方工具族（方案真源 = doc/41 §3.3 / §5.1）。
//
// 对外协议逐字继承 doc/01（旧线被验证过的部分，不重新发明）：
// 四把工具 TaskCreate / TaskUpdate / TaskList / TaskGet；状态词表
// pending → in_progress → completed，不再需要时 deleted（TaskList 不列出）。
//
// **存储改掉旧线的死因**：正本从「进程内内存 Store + eino session 镜像」（doc/01 §3，进程退出即清空）
// 改成 **PG 表 run_tasks**（doc/41 §5.1）→ 看板活得过崩溃，resume 后 TaskList 读到的就是真相。
//
// 约束写在 tool description，不写状态机（doc/01）：≥3 步或用户列了多件事才建表；一步任务不建；
// 动手前标 in_progress，同时尽量一条；做完立刻 completed；测试红/做了一半/未解报错不准 completed；
// 卡住就 Create 一条 blocker。
package runengine

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// RunTask 是看板正本（doc/41 §5.1）：一场 run 一块看板。
type RunTask struct {
	ID        string    `gorm:"primaryKey"`
	TenantID  string    `gorm:"not null;index:run_tasks_scope,priority:1"`
	RunID     RunID     `gorm:"not null;uniqueIndex:run_tasks_run_no,priority:1"`
	TaskNo    int       `gorm:"not null;uniqueIndex:run_tasks_run_no,priority:2"`
	SessionID SessionID `gorm:"not null;index:run_tasks_scope,priority:2"`
	Subject   string    `gorm:"not null"`
	// SkillName 是这条任务替哪个技能办的（doc/52 D32）：TaskCreate 时按本 run 最近一次
	// use_skill 盖戳，空串 = 不是技能任务。归属只由「建任务」决定——read_skill 不盖戳。
	SkillName   string `gorm:"not null;default:''"`
	Description string `gorm:""`
	ActiveForm  string `gorm:""`
	// Result 是这一步的产出（doc/64）：关键数字、结论、产物路径、跑过的命令。
	// 标 completed 时必填（闸在 UpdateTask 里），入库前截到 taskResultCapRunes。
	Result    string    `gorm:"not null;default:''"`
	Status    string    `gorm:"not null;default:pending"`
	CreatedAt time.Time `gorm:"not null"`
	UpdatedAt time.Time `gorm:"not null"`
}

func (RunTask) TableName() string { return "run_tasks" }

// 看板状态词表（doc/01 继承）。deleted 不进 TaskList。
const (
	taskStatusPending    = "pending"
	taskStatusInProgress = "in_progress"
	taskStatusCompleted  = "completed"
	taskStatusDeleted    = "deleted"
)

// taskStatusDoc 是状态词表的说明文案（写进工具描述，模型看得懂）。
const taskStatusDoc = "pending（还没动手）/ in_progress（正在做）/ completed（做完了）/ deleted（不要了）"

// taskResultRequiredText 是「标完成但没写结果」的拒写文本——模型唯一能看见的信号。
// 以「错误：」开头：toolErrorText 对已有前缀的消息不重复加，最终字节就是这一句。
const taskResultRequiredText = "错误：标 completed 必须带 result——写这一步的产出（关键数字、结论、产物路径、跑过的命令），不是「已完成」「已修复」。"

// taskResultGate 判定这次状态变更能不能写：把某行标 completed 时，该行的结果必须非空
// （本次传入优先；A2 起由调用方把行上已有的结果并进来再调它）。
// 只有这一条规则——闸保证「有字」，不判断写得好不好。
func taskResultGate(status, result string) (bool, string) {
	if status == taskStatusCompleted && strings.TrimSpace(result) == "" {
		return false, taskResultRequiredText
	}
	return true, ""
}

// 单条结果的口径（doc/64 §6 P2/P3）：300 字入库、超长截断标原长（不拒写，避免第二次涨停）；
// 模型连续拒写时放行并落固定串——空结果与「还没做」在库里必须能分开。
const (
	taskResultCapRunes      = 300
	taskResultUnrecordedTxt = "（未记录：连续 3 次没写结果）"
)

// ErrTaskResultRequired 是闸的哨兵（存储层 → 工具层）。error 文本就是模型看到的那一句。
var ErrTaskResultRequired = errors.New(taskResultRequiredText)

// ErrTaskNotFound 是不存在的 task_no。
var ErrTaskNotFound = errors.New("task not found")

// taskBox 是看板的 run 身份（一场 run 一块看板）。挂 ctx，工具从这儿取 run/session。
// resultRejects 记本 run 因「标完成没写结果」被拒的次数（放行口用，随 run 死）。
type taskBox struct {
	mu            sync.Mutex
	session       SessionID
	run           RunID
	resultRejects int
}

func newTaskBox(session SessionID, run RunID) *taskBox {
	return &taskBox{session: session, run: run}
}

// noteResultReject 记一次拒写并返回累计次数（同 run 内）。
func (b *taskBox) noteResultReject() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.resultRejects++
	return b.resultRejects
}

type taskBoxKey struct{}

func withTaskBox(ctx context.Context, b *taskBox) context.Context {
	return context.WithValue(ctx, taskBoxKey{}, b)
}

func taskBoxFrom(ctx context.Context) *taskBox {
	b, _ := ctx.Value(taskBoxKey{}).(*taskBox)
	return b
}

// CreateTask 记一行（task_no = run 内自增）。返回 task_no（= 旧线的 taskId）。
// skillName 是盖在行上的技能归属（本 run 最近一次 use_skill；空串 = 不是技能任务）。
func (s *Store) CreateTask(ctx context.Context, tenantID string, runID RunID, sessionID SessionID,
	subject, description, activeForm, skillName string) (int, error) {
	var no int
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&RunTask{}).
			Where("run_id = ?", runID).
			Select("COALESCE(MAX(task_no), 0) + 1").Scan(&no).Error; err != nil {
			return err
		}
		id, err := newRowID("tk_")
		if err != nil {
			return err
		}
		row := RunTask{
			ID: id, TenantID: tenantID, RunID: runID, TaskNo: no, SessionID: sessionID,
			Subject: subject, SkillName: skillName, Description: description, ActiveForm: activeForm,
			Status: taskStatusPending, CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}
		return tx.Create(&row).Error
	})
	return no, err
}

// UpdateTask 改一行（按 run_id + task_no）。不存在的 task_no → ErrTaskNotFound。
// 闸：本次调用把行标 completed 且「行上没有结果」→ ErrTaskResultRequired，整笔回滚（状态与
// result 都不写，其它字段也不写——拒绝不留半截）。effective = 本次传入 || 行上已有的结果，
// 所以已经带结果的行再改别的字段不会被二次拦。
func (s *Store) UpdateTask(ctx context.Context, tenantID string, runID RunID, taskNo int,
	status, result, subject, description, activeForm string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row RunTask
		if err := tx.Where("run_id = ? AND task_no = ?", runID, taskNo).Take(&row).Error; err != nil {
			return ErrTaskNotFound
		}
		effective := result
		if strings.TrimSpace(effective) == "" {
			effective = row.Result
		}
		if ok, _ := taskResultGate(status, effective); !ok {
			return ErrTaskResultRequired
		}
		if status != "" {
			row.Status = status
		}
		if trimmed := strings.TrimSpace(result); trimmed != "" {
			row.Result = truncateRunes(trimmed, taskResultCapRunes)
		}
		if subject != "" {
			row.Subject = subject
		}
		if description != "" {
			row.Description = description
		}
		if activeForm != "" {
			row.ActiveForm = activeForm
		}
		row.UpdatedAt = time.Now()
		return tx.Save(&row).Error
	})
}

// ListTasks 列出这一场 run 的看板（deleted 不列），按 task_no 升序。
func (s *Store) ListTasks(ctx context.Context, runID RunID) ([]RunTask, error) {
	var rows []RunTask
	err := s.db.WithContext(ctx).
		Where("run_id = ? AND status <> ?", runID, taskStatusDeleted).
		Order("task_no").Find(&rows).Error
	return rows, err
}

// GetTask 取一行。
func (s *Store) GetTask(ctx context.Context, runID RunID, taskNo int) (RunTask, error) {
	var row RunTask
	err := s.db.WithContext(ctx).Where("run_id = ? AND task_no = ?", runID, taskNo).Take(&row).Error
	return row, err
}

// PreviousBoardRun 找本会话里最近一个（排除当前 run）还有未删除任务的 run（doc/52 D34/D35）。
// 走 (tenant_id, session_id) 复合索引，不加新列；只要「有任务」，不要求还有没做完的——
// 全完成的上一场由调用方渲染成一行提示。
func (s *Store) PreviousBoardRun(ctx context.Context, tenantID string, sessionID SessionID, current RunID) (RunID, error) {
	var id string
	err := s.db.WithContext(ctx).Model(&RunTask{}).
		Where("tenant_id = ? AND session_id = ? AND run_id <> ? AND status <> ?",
			tenantID, sessionID, current, taskStatusDeleted).
		Order("created_at DESC").Limit(1).Pluck("run_id", &id).Error
	return RunID(id), err
}

// ListOpenTasks 列一场 run 未完成的行（pending/in_progress），按 task_no 升序。
func (s *Store) ListOpenTasks(ctx context.Context, runID RunID) ([]RunTask, error) {
	var rows []RunTask
	err := s.db.WithContext(ctx).
		Where("run_id = ? AND status IN ?", runID, []string{taskStatusPending, taskStatusInProgress}).
		Order("task_no").Find(&rows).Error
	return rows, err
}

// ListCompletedResults 列一场 run 里 completed 且带结果的行（doc/64：跨 run 取结果的口）。
// 空结果 = 本方案之前完成的行或放行前的旧数据，不列出、也不打标。
func (s *Store) ListCompletedResults(ctx context.Context, runID RunID) ([]RunTask, error) {
	var rows []RunTask
	err := s.db.WithContext(ctx).
		Where("run_id = ? AND status = ? AND result <> ''", runID, taskStatusCompleted).
		Order("task_no").Find(&rows).Error
	return rows, err
}

// TaskSkillNames 列一场 run 里建过任务、且任务未删除的技能名（doc/52 D35 执行权恢复用）。
func (s *Store) TaskSkillNames(ctx context.Context, tenantID string, runID RunID) ([]string, error) {
	var names []string
	err := s.db.WithContext(ctx).Model(&RunTask{}).
		Where("tenant_id = ? AND run_id = ? AND skill_name <> '' AND status <> ?",
			tenantID, runID, taskStatusDeleted).
		Distinct().Order("skill_name").Pluck("skill_name", &names).Error
	return names, err
}

// taskBoardView 是看板的紧凑视图：模型的眼睛（也是 task 帧的 text）。
// 每行「#序号 [状态] 主题」，属于某个技能的行再带「（技能：名字）」——哪几条任务替哪个技能办，
// 看板自己回答（D32）；末尾一句催更块。
func taskBoardView(rows []RunTask) string {
	if len(rows) == 0 {
		return "看板：（空）"
	}
	var b strings.Builder
	b.WriteString("看板：\n")
	for _, r := range rows {
		b.WriteString(taskLine(r) + "\n")
	}
	b.WriteString(taskBoardTail(rows))
	return strings.TrimRight(b.String(), "\n")
}

// taskLine 是一行的结构态（状态 + 主题 + 技能归属），不含结果。
func taskLine(r RunTask) string {
	line := fmt.Sprintf("#%d [%s] %s", r.TaskNo, r.Status, r.Subject)
	if r.SkillName != "" {
		line += fmt.Sprintf("（技能：%s）", r.SkillName)
	}
	return line
}

// taskBoardTail 是视图末尾的催更块（两个视图共用）。
func taskBoardTail(rows []RunTask) string {
	open := 0
	for _, r := range rows {
		if r.Status == taskStatusPending || r.Status == taskStatusInProgress {
			open++
		}
	}
	if open > 0 {
		return fmt.Sprintf("（还有 %d 项没完成：动手前把它标 in_progress，做完立刻标 completed；卡住就建一条 blocker 记录）", open)
	}
	return "（都完成了）"
}

// taskResultPreviewRunes 是列表与上一场段里结果的预览上限（全文走 TaskGet）。
const taskResultPreviewRunes = 100

// taskListView 是 TaskList 专用的视图（doc/64 §2.3）：在结构态基础上，给 completed 且有结果的行
// 多一行预览。**写回执与 kind=task 帧仍走 taskBoardView**——不让每张回执重付所有历史结果
// （doc/52 D34 同款取舍）。
func taskListView(rows []RunTask) string {
	if len(rows) == 0 {
		return "看板：（空）"
	}
	var b strings.Builder
	b.WriteString("看板：\n")
	for _, r := range rows {
		b.WriteString(taskLine(r) + "\n")
		if r.Status == taskStatusCompleted {
			if res := strings.TrimSpace(r.Result); res != "" {
				b.WriteString("  结果：" + truncateRunes(res, taskResultPreviewRunes) + "\n")
			}
		}
	}
	b.WriteString(taskBoardTail(rows))
	return strings.TrimRight(b.String(), "\n")
}

// taskReceipt 是写操作的回执：操作回执 + 看板紧凑视图 + 催更块（doc/41 §3.3）。
func taskReceipt(op string, rows []RunTask) string {
	return op + "\n" + taskBoardView(rows)
}

// previousBoardView 渲染 TaskList 独有的「上一场」段（doc/52 D34）：列出未完成行的
// #N、主题与说明；全完成时只给一行提示。doc/64 起再带「已完成的结果」子段——跨 run 续做时，
// 上一场做完的步骤能拿到产出，不必重跑。**写操作回执与 kind=task 帧不走这里**——
// 那段说明只在显式 TaskList 时付一次 token（§2.5 成本边界）。
func previousBoardView(prev RunID, open, done []RunTask) string {
	if prev == "" {
		return ""
	}
	var b strings.Builder
	if len(open) == 0 {
		fmt.Fprintf(&b, "上一场（%s）已全部完成", prev)
	} else {
		fmt.Fprintf(&b, "上一场（%s）还有 %d 项没完成：", prev, len(open))
		for _, r := range open {
			fmt.Fprintf(&b, "\n#%d [%s] %s", r.TaskNo, r.Status, r.Subject)
			if r.Description != "" {
				fmt.Fprintf(&b, "\n说明：%s", r.Description)
			}
		}
	}
	if len(done) > 0 {
		b.WriteString("\n已完成的结果：")
		for _, r := range done {
			fmt.Fprintf(&b, "\n#%d %s——结果：%s", r.TaskNo, r.Subject,
				truncateRunes(strings.TrimSpace(r.Result), taskResultPreviewRunes))
		}
	}
	return b.String()
}

// taskTenantOf 取当前租户（看板是租户内可见的资源）。
func taskTenantOf(ctx context.Context) string {
	if p, ok := principalFromCtx(ctx); ok {
		return p.TenantID
	}
	return ""
}

// emitTaskFrame 发一条 kind=task 帧（C2，doc/41 §5.2）：看板的结构态。
// text = 当前全量看板的紧凑视图（与工具回执里的那段同源），name = 触发它的工具名。
// skillName = 触发这次写入的那条任务所属技能（逐行归属看 text；这个字段只给页面标「这次写的是
// 哪个技能的活」，空串省略）。写不出看板（表还没建、DB 故障）时只记日志、不杀 run——看板是观测面，
// 不是主流程。
func (w *Worker) emitTaskFrame(ctx context.Context, runID RunID, epoch int64, name, model string) {
	rows, err := w.store.ListTasks(ctx, runID)
	if err != nil {
		log.Printf("task frame %s: %v", runID, err)
		return
	}
	// 帧字面量写成 `Frame{Kind: "task", …}` 单行——契约快闸的词表测试按这个形状扫
	// （`Frame\{Kind:\s*"([a-z_]+)"`，换行就扫不到 → 加一种 kind 而不同步词表会假绿）。
	accepted, err := w.store.AppendFrame(ctx, runID, epoch, Frame{Kind: "task", Agent: "", Model: model, Name: name, SkillName: touchedSkill(rows), Text: taskBoardView(rows)})
	if err != nil {
		log.Printf("task frame %s: %v", runID, err)
		return
	}
	if accepted {
		// 帧计数跟着走（与 tool_result 同口径：多一帧就多一次 reconcile 的游标推进）。
		_ = accepted
	}
}

// touchedSkill 是 task 帧附带的那一个技能名：触发这帧的写操作刚更新过的那一行
// （UpdatedAt 最新；同一时刻按 task_no 取大）。返回空串表示那条任务不是技能任务。
func touchedSkill(rows []RunTask) string {
	var latest *RunTask
	for i := range rows {
		r := &rows[i]
		if latest == nil || r.UpdatedAt.After(latest.UpdatedAt) ||
			(r.UpdatedAt.Equal(latest.UpdatedAt) && r.TaskNo > latest.TaskNo) {
			latest = r
		}
	}
	if latest == nil {
		return ""
	}
	return latest.SkillName
}

// taskBoardPrompt 是看板的使用约定，静态拼进系统提示（doc/41 §3.3 继承 doc/01 的约束）。
const taskBoardPrompt = `## 任务看板

活儿多的时候建一张看板，跟踪进度：≥3 步、或用户列了多件事时才建；一步任务不要建。
计划模式里也可以建/改看板——它是方案的一部分，不算工作区修改；先建看板再 submit_plan。
- TaskCreate 建条目（主题写成祈使句一句话，如「列出工作区文件」）。
- description 写「这一步具体做什么」（输入、要跑的脚本、产物）——下一场的你只能靠它续上。
- 动手前 TaskUpdate 把那条标 in_progress；同时进行的只留一条。
- 做完立刻标 completed；测试红、做了一半、报错没解，**不准**标 completed。
- 做完标 completed 时，result 写这一步的产出（数字、结论、路径）——下一场的你只能靠它续上，不要重跑。
- 卡住就 TaskCreate 建一条 blocker 记录，写清卡在哪。
- TaskList 随时看当前进度（动手前后各看一次），TaskGet 看细节。`

// —— 四把工具 ——

type taskCreateInput struct {
	Subject     string `json:"subject" jsonschema:"description=任务主题（一句话，祈使句，如「列出工作区文件」）"`
	Description string `json:"description,omitempty" jsonschema:"description=这一步具体做什么。下一场（新 run）只能靠它续上：写清输入、要跑的脚本/命令与产物，别省步骤"`
	ActiveForm  string `json:"active_form,omitempty" jsonschema:"description=可选：进行中的现在时短语（如「正在列出文件」）"`
}

type taskUpdateInput struct {
	TaskID      int    `json:"task_id" jsonschema:"description=任务号（TaskCreate 返回的那个整数）"`
	Status      string `json:"status,omitempty" jsonschema:"description=状态：pending / in_progress / completed / deleted"`
	Result      string `json:"result,omitempty" jsonschema:"description=标 completed 时必填：这一步的产出（关键数字、结论、产物路径、跑过的命令）。下一场的你只能靠它，不要重跑"`
	Subject     string `json:"subject,omitempty" jsonschema:"description=可选：改主题"`
	Description string `json:"description,omitempty" jsonschema:"description=可选：改说明（给下一场看的那份，写清输入、脚本与产物）"`
	ActiveForm  string `json:"active_form,omitempty" jsonschema:"description=可选：改现在时短语"`
}

type taskGetInput struct {
	TaskID int `json:"task_id" jsonschema:"description=任务号（TaskCreate 返回的那个整数）"`
}

type taskNoInput struct{}

func newTaskCreateTool(store *Store) (tool.BaseTool, error) {
	return utils.InferTool("TaskCreate",
		"建一条任务并加进看板，返回任务号（run 内自增的整数）。"+
			"**建表约定**：≥3 步、或用户列了多件事时才建看板；一步任务不建。主题用祈使句一句话。"+
			"description 写「这一步具体做什么」——下一场（新 run）只能靠它续上，别省步骤。",
		func(ctx context.Context, in taskCreateInput) (string, error) {
			box := taskBoxFrom(ctx)
			if box == nil {
				return "", fmt.Errorf("看板不可用（没有 run 身份）")
			}
			no, err := store.CreateTask(ctx, taskTenantOf(ctx), box.run, box.session,
				in.Subject, in.Description, in.ActiveForm, usedSkillName(ctx))
			if err != nil {
				return "", err
			}
			rows, _ := store.ListTasks(ctx, box.run)
			return taskReceipt(fmt.Sprintf("已建任务 #%d：%s", no, in.Subject), rows), nil
		})
}

func newTaskUpdateTool(store *Store) (tool.BaseTool, error) {
	return utils.InferTool("TaskUpdate",
		"改看板里的一条任务，任务号来自 TaskCreate 的返回值。状态取 "+taskStatusDoc+"。"+
			"**约定**：动手前标 in_progress；做完立刻标 completed；测试红/做了一半/没解报错**不准**标 completed。"+
			"标 completed 时必须带 result，写这一步的产出（数字、结论、路径），不是「已完成」。"+
			"卡住就 TaskCreate 建一条 blocker 记录。",
		func(ctx context.Context, in taskUpdateInput) (string, error) {
			box := taskBoxFrom(ctx)
			if box == nil {
				return "", fmt.Errorf("看板不可用（没有 run 身份）")
			}
			err := store.UpdateTask(ctx, taskTenantOf(ctx), box.run, in.TaskID,
				in.Status, in.Result, in.Subject, in.Description, in.ActiveForm)
			// 放行口（doc/64 §6 P3）：同 run 第 3 次因「没写结果」被拒起，落固定串放行——
			// 模型固执时不在这个调用上无限撞墙；回执照常，不追加任何催促。
			if errors.Is(err, ErrTaskResultRequired) && box.noteResultReject() >= 3 {
				err = store.UpdateTask(ctx, taskTenantOf(ctx), box.run, in.TaskID,
					in.Status, taskResultUnrecordedTxt, in.Subject, in.Description, in.ActiveForm)
			}
			if err != nil {
				if errors.Is(err, ErrTaskNotFound) {
					return "", fmt.Errorf("没有这条任务：#%d（用 TaskList 看有哪些）", in.TaskID)
				}
				return "", err
			}
			rows, _ := store.ListTasks(ctx, box.run)
			return taskReceipt(fmt.Sprintf("已更新任务 #%d", in.TaskID), rows), nil
		})
}

// renderTaskList 是 TaskList 工具与 dev 探针共用的渲染：本场视图 + 「上一场」段。
// sessionID 为空时只给本场（探针不带会话时用）。
func renderTaskList(ctx context.Context, store *Store, runID RunID, sessionID SessionID) string {
	rows, _ := store.ListTasks(ctx, runID)
	view := taskListView(rows)
	// D34：只在这一处追加「上一场」段；写回执与 task 帧不带（共用体不加参数）。
	if tenantID := taskTenantOf(ctx); tenantID != "" && sessionID != "" {
		prev, err := store.PreviousBoardRun(ctx, tenantID, sessionID, runID)
		if err != nil {
			log.Printf("task list %s: 上一场查询失败: %v", runID, err)
			return view
		}
		if prev == "" {
			return view
		}
		open, err := store.ListOpenTasks(ctx, prev)
		if err != nil {
			log.Printf("task list %s: 上一场读行失败: %v", runID, err)
			return view
		}
		done, err := store.ListCompletedResults(ctx, prev)
		if err != nil {
			log.Printf("task list %s: 上一场结果读取失败: %v", runID, err)
		}
		if seg := previousBoardView(prev, open, done); seg != "" {
			view += "\n\n" + seg
		}
	}
	return view
}

func newTaskListTool(store *Store) (tool.BaseTool, error) {
	return utils.InferTool("TaskList",
		"列出本场 run 的看板（每条：#序号 [状态] 主题；completed 且写了结果的，下面多一行「结果：」）。"+
			"用它在动手前后自校准进度。"+
			"上一场（同会话的上一轮）还有没做完的任务时，输出末尾会多一段「上一场」：带主题、说明与已完成的结果，"+
			"用户说的「第 N 步」通常指那里。",
		func(ctx context.Context, in taskNoInput) (string, error) {
			box := taskBoxFrom(ctx)
			if box == nil {
				return "", fmt.Errorf("看板不可用（没有 run 身份）")
			}
			return renderTaskList(ctx, store, box.run, box.session), nil
		})
}

func newTaskGetTool(store *Store) (tool.BaseTool, error) {
	return utils.InferTool("TaskGet",
		"取看板里的一条任务（详情：主题、说明、状态、结果）。",
		func(ctx context.Context, in taskGetInput) (string, error) {
			box := taskBoxFrom(ctx)
			if box == nil {
				return "", fmt.Errorf("看板不可用（没有 run 身份）")
			}
			row, err := store.GetTask(ctx, box.run, in.TaskID)
			if err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return "", fmt.Errorf("没有这条任务：#%d", in.TaskID)
				}
				return "", err
			}
			out := fmt.Sprintf("#%d [%s] %s\n说明：%s\n现在时：%s",
				row.TaskNo, row.Status, row.Subject, row.Description, row.ActiveForm)
			if row.Result != "" {
				out += "\n结果：" + row.Result
			}
			if row.SkillName != "" {
				out += "\n技能：" + row.SkillName
			}
			return out, nil
		})
}

// —— C3 的存储层证明放在 tasktools_test.go（生产文件里不写测试）——

// devTaskCheck 是看板读口的探针（脚手架已按 doc/64 §4 的转正点转正为只读）。
// create / update 两个写动作已删：闸在真工具与单测上验完，探针不再提供第二条写库路径。
//
//	POST /api/dev/task-check {"action":"list","run_id":"r_probe_a"}
//	200 {"code":0,"message":"","data":{"view":"…","rows":[{taskNo,status,subject,result}]}}
func (s *Server) devTaskCheck(c *gin.Context) {
	p, ok := s.requirePrincipal(c)
	if !ok {
		return
	}
	var req struct {
		Action    string `json:"action"`
		RunID     string `json:"run_id"`
		SessionID string `json:"session_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeEnvelope(c, http.StatusBadRequest, codeBadJSON, "请求格式不对", nil)
		return
	}
	if req.Action != "list" {
		writeEnvelope(c, http.StatusBadRequest, codeBadInput, "这个探针只读：action 只能是 list", nil)
		return
	}
	if req.RunID == "" {
		writeEnvelope(c, http.StatusBadRequest, codeBadInput, "run_id 必填", nil)
		return
	}
	// 身份要进 request ctx：renderTaskList 的租户取 principalFromCtx，中间件只写进了 gin context。
	ctx := withPrincipal(c.Request.Context(), p)
	rows, err := s.store.ListTasks(ctx, RunID(req.RunID))
	if err != nil {
		internal(c, err)
		return
	}
	out := make([]struct {
		TaskNo  int    `json:"taskNo"`
		Status  string `json:"status"`
		Subject string `json:"subject"`
		Result  string `json:"result"`
	}, 0, len(rows))
	for _, r := range rows {
		out = append(out, struct {
			TaskNo  int    `json:"taskNo"`
			Status  string `json:"status"`
			Subject string `json:"subject"`
			Result  string `json:"result"`
		}{r.TaskNo, r.Status, r.Subject, r.Result})
	}
	writeEnvelope(c, http.StatusOK, codeOK, "", struct {
		View string `json:"view"`
		Rows any    `json:"rows"`
	}{View: renderTaskList(ctx, s.store, RunID(req.RunID), SessionID(req.SessionID)), Rows: out})
}
