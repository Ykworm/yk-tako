// Package taskboard 是任务看板：TaskCreate/TaskUpdate/TaskList/TaskGet 四个记账工具、
// middleware 注入与看板投影。本文件是数据模型与存储正本（Store）；
// 工具与输出格式见 tools.go，middleware 与提示词约定见 middleware.go。
//
// 三层分离（doc/01）：Plan 是给人审的文档，Tasks 是带 taskId 的施工看板，
// Actions 是干活工具。本包只管 Tasks 层。
package taskboard

import (
	"encoding/gob"
	"fmt"
	"sync"
)

// init 把快照类型注册给 gob：session.Values 是 map[string]any，interrupt 时 eino 把整个
// runContext（含 session）gob 进 checkpoint，接口字段背后的具体类型必须注册
// （eino 自己注册 []byte 同款，adk/interrupt.go init）。
func init() {
	gob.Register([]Task{})
}

// SessionKey 是看板镜像在 session 里的键。每次变更后全量快照写入；
// interrupt 时随 runContext 落盘（adk/runctx.go 的 runSession 有
// "CheckpointSchema: persisted via serialization.RunCtx (gob)" 标注）。
const SessionKey = "taskboard_session_key_tasks"

// Status 是任务状态：pending → in_progress → completed，不再需要时用 deleted。
type Status string

// 四个合法状态值，与 eino plantask / Claude Tasks 对齐。
const (
	StatusPending    Status = "pending"
	StatusInProgress Status = "in_progress"
	StatusCompleted  Status = "completed"
	StatusDeleted    Status = "deleted"
)

// validStatus 校验 TaskUpdate 传入的 status 字符串。
func validStatus(s string) bool {
	switch Status(s) {
	case StatusPending, StatusInProgress, StatusCompleted, StatusDeleted:
		return true
	}
	return false
}

// Task 是一条看板行。taskId 只寻址看板行：与 plan 条目、tool_call 都无外键。
type Task struct {
	ID          string `json:"id"`
	Subject     string `json:"subject"`
	Description string `json:"description,omitempty"`
	ActiveForm  string `json:"activeForm,omitempty"`
	Status      Status `json:"status"`
}

// Patch 是 TaskUpdate 的可改字段。空串表示「不动」（所以字段无法清成空串，是有意取舍）。
type Patch struct {
	Subject     string
	Description string
	ActiveForm  string
	Status      string
}

// Store 是看板正本：middleware 自持，agent 建一次，跨 REPL 轮次存活。
// 所有方法加锁，同轮并行的工具调用也写不坏。
type Store struct {
	mu     sync.Mutex
	tasks  map[string]*Task
	order  []string // 创建顺序，看板按它排
	nextID int
}

// NewStore 建一个空看板，id 从 "1" 开始自增。
func NewStore() *Store {
	return &Store{tasks: map[string]*Task{}, nextID: 1}
}

// Create 建一条 pending 任务，返回副本（调用方改不到内部）。
func (s *Store) Create(subject, description, activeForm string) Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := fmt.Sprintf("%d", s.nextID)
	s.nextID++
	t := &Task{ID: id, Subject: subject, Description: description, ActiveForm: activeForm, Status: StatusPending}
	s.tasks[id] = t
	s.order = append(s.order, id)
	return *t
}

// Update 按 id patch；id 不存在或 status 非法时报错。
func (s *Store) Update(id string, p Patch) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return Task{}, fmt.Errorf("任务 #%s 不存在", id)
	}
	if p.Status != "" {
		if !validStatus(p.Status) {
			return Task{}, fmt.Errorf("非法 status %q：只能是 pending/in_progress/completed/deleted", p.Status)
		}
		t.Status = Status(p.Status)
	}
	if p.Subject != "" {
		t.Subject = p.Subject
	}
	if p.Description != "" {
		t.Description = p.Description
	}
	if p.ActiveForm != "" {
		t.ActiveForm = p.ActiveForm
	}
	return *t, nil
}

// Get 读单条；不存在报错。
func (s *Store) Get(id string) (Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return Task{}, fmt.Errorf("任务 #%s 不存在", id)
	}
	return *t, nil
}

// List 返回未 deleted 的任务，按创建序；返回副本切片。
func (s *Store) List() []Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Task, 0, len(s.order))
	for _, id := range s.order {
		if t := s.tasks[id]; t.Status != StatusDeleted {
			out = append(out, *t)
		}
	}
	return out
}

// Snapshot 返回全量任务（含 deleted），给 session 镜像用。
func (s *Store) Snapshot() []Task {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Task, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, *s.tasks[id])
	}
	return out
}
