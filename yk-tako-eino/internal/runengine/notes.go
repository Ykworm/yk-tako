// 本文件：笔记层（doc/17 §7）——memory 工具 + notes 表 + 主代理快照缝。
//
// 病灶：用户说过的规矩（「deck 放在 output/deck.html，以后改这个文件，不要重建」）只活在那一场
// run 的消息历史里——新开的会话没有这场历史，模型下一次把 deck 重建一遍；交给子代理的任务文字里
// 若没有这句，子代理同样当没有。笔记让这句话跨会话、跨代理活下来。
//
// 端到端链路（每环可点名）：用户说规矩 → 模型调 memory(action=add, quote=用户原句) → writeNote
// 在同一个 PG 事务里取 workspace 级锁、算上限、插入 notes 表 → 下一场 run 的 BeforeAgent 读未替代行、
// 渲染快照段冻进 noteBox → BeforeModelRewriteState 把 state.Messages[0] 重写为 Instruction + 快照段
// → 模型在系统提示里读到上次写下的原话和路径（新会话、子代理同一份——子代理侧缝在 agents.go）。
//
// 写入只有一个函数（writeNote）：字数上限（agent 2200 / user 1375 字）、非空原话（≤200 字）全在
// 函数里面；本版唯一调用方 = memory 工具。上限检查与插入同一 PG 事务 + workspace 级 advisory xact
// 锁（doc/17 §7 四件之 4）：笔记按 workspace 分、跨会话可并发写——「一会话同时只有一场 run」管不到
// 跨会话，闸必须在库里不在进程里（AGENTS.md 多副本纪律）。
//
// 快照冻结语义（planning §1-4）：每场 run 起手读一次、run 内冻结——工具写库不回写本场系统提示；
// 同一场后续动作要定位旧条目，只能拿工具结果清单里的 content 全文当凭据（清单恒带，planning §3-C）。
package runengine

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
	"github.com/cloudwego/eino/schema"
	"gorm.io/gorm"
)

// 容量上限（doc/17 §8，借 Hermes 内置笔记默认；字数 = rune 数，中文按字）。
// quote 超出拒绝、不截断（planning §1-5）。
const (
	noteCapAgent = 2200
	noteCapUser  = 1375
	noteQuoteCap = 200
)

// noteFailAdviseAfter 是劝停句的触发线（doc/17 §7 四件之 2）：同一场 run 里写入失败记到 3 次，
// 第 4 次的结果写明「停止再调、先回复用户」——否则上限满/定位失败时模型一直重试，用户等不到回复。
const noteFailAdviseAfter = 3

// 两类 kind 的取值（DDL 注释同词）。
const (
	noteKindAgent = "agent"
	noteKindUser  = "user"
)

// 写入失败分类（noteWriteResult.fail）：拒写路径的结果文本按它选原因句；
// M3 的三次记次也按它归类（超上限 / 定位失败（M2）/ 原话为空）。
const (
	noteFailAction     = "action"
	noteFailKind       = "kind"
	noteFailContent    = "content"
	noteFailQuoteEmpty = "quote_empty"
	noteFailQuoteLong  = "quote_long"
	noteFailOverCap    = "over_cap"
	noteFailLocate     = "locate"
	noteFailInvisible  = "invisible"
)

// ── 不可见字符检查（doc/17 §7 四件之 3；2026-10-15 十七修：句式词表裁除，只留本档）───────
//
// 病灶：笔记会进以后每一场的系统提示——一行夹带零宽 / bidi 控制字符的话，人审稿面看不见，
// 却随笔记永久排在系统提示里、每场模型原样读到（人审以为记的是句人话）。所以写入时拒、
// 存量毒行渲染占位。
//
// 为什么只剩这一档（19v6-90 十七次修订，用户 2026-10-15 裁定）：原句式神词表
// （Hermes threat_patterns.py strict 档 11 条）经真码探针逐条实测后裁除——纯中文写法
// （「禁用 system prompt」）一条不命中，锚词换中文、写全角都能绕，命中面只剩「句子里恰好
// 用了英文锚词」的窄缝，类别的清单列不尽；隐形字符清单有限、能枚举尽、与语言无关——拦的是
// 「能不能藏」不是「说了什么」；正经用途少（组合 emoji 的 ZWJ、波斯语 ZWNJ），误伤面小、
// 拒写时当场报码点（调用方可见、可改）。
const noteInvisibleRunes = "\u200b\u200c\u200d\u2060\u2062\u2063\u2064\ufeff\u202a\u202b\u202c\u202d\u202e\u2066\u2067\u2068\u2069"

// scanNoteInvisible 扫一段文字里的隐形字符（noteInvisibleRunes 17 码点），返回命中说明
// （"invisible_unicode_U+XXXX"——拒写文案与占位文案照报，调用方/用户当场看见是哪个码点；
// 空串 = 无命中）。
func scanNoteInvisible(text string) string {
	for _, r := range text {
		if strings.ContainsRune(noteInvisibleRunes, r) {
			return fmt.Sprintf("invisible_unicode_U+%04X", r)
		}
	}
	return ""
}

// noteBlockPlaceholder 是快照里毒行的占位行（doc/17 §7 四件之 3 的字面：不展示、写明用 remove 删）。
// 快照打占位、工具结果清单留原文（Hermes 同款两端策略，实读确认：清单里删掉原文 = 静默藏起攻击，
// 谁也删不掉它；原文只在模型主动调 memory 的那一轮经工具结果进对话，不进跨 run 的系统提示）。
func noteBlockPlaceholder(id string) string {
	return fmt.Sprintf("[BLOCKED: 此条笔记含人眼不可见字符（%s），不进入系统提示；请用 memory 的 remove 删掉它。]", id)
}

// Note = `notes` 表（planning §4 DDL）：按 workspace 分区的一行笔记。
// superseded_by 非空 = 已被替代：旧行指向替代它的新行（replace 只填这个指针，content/quote 不动）；
// 快照/清单只读 NULL 行。content/quote 一经写入不再改写——改口 = 新插一行 + 旧行打 superseded_by，
// 旧行原样保留（对质入口：两周后问「我说过不要重建吗」，打开的是旧行 quote 与 source 位置）。
type Note struct {
	ID           string    `gorm:"primaryKey"` // n_<ULID>
	Workspace    string    `gorm:"not null;index:notes_live,where:superseded_by IS NULL,priority:1"`
	Kind         string    `gorm:"not null;index:notes_live,priority:2"` // agent | user
	Content      string    `gorm:"not null"`                             // 模型整理句
	Quote        string    // 用户原句（当前写入路径恒非空；NULL 形态留给以后无原话的写入方）
	SourceRunID  RunID     `gorm:"not null"` // 写入那场 run（配合 source_seq 找回原话现场）
	SourceSeq    int64     `gorm:"not null"` // 该消息在 runlog 的 seq
	SupersededBy *string   // 非空 = 已被替代（replace 时旧行指向新行；remove 直接删行不留指针）
	TenantID     string    `gorm:"not null;default:'';index:notes_scope,priority:1"`
	UserID       string    `gorm:"not null;default:'';index:notes_scope,priority:3"`
	Scope        string    `gorm:"not null;default:user;index:notes_scope,priority:2"` // user | tenant
	CreatedAt    time.Time `gorm:"not null"`
}

func (Note) TableName() string { return "notes" }

// noteEntry 是清单/快照里的一条有效条目（未替代行）。
type noteEntry struct {
	Kind    string
	Content string
	Quote   string
}

// noteSums 是两类未替代 content 的 rune 合计（上限检查与「已用」行的数字，planning §3-C）。
type noteSums struct{ Agent, User int }

func (s noteSums) of(kind string) int {
	if kind == noteKindUser {
		return s.User
	}
	return s.Agent
}

// plus 返回加上 delta 后的合计副本（replace 的投影合计用它拼）。
func (s noteSums) plus(kind string, delta int) noteSums {
	if kind == noteKindUser {
		s.User += delta
	} else {
		s.Agent += delta
	}
	return s
}

// noteCap 按 kind 取上限。
func noteCap(kind string) int {
	if kind == noteKindUser {
		return noteCapUser
	}
	return noteCapAgent
}

// noteWrite 是 writeNote 的输入（工具参数原样携带，trim 与校验在写入函数里）。
type noteWrite struct {
	Action     string
	Kind       string
	Content    string
	Quote      string
	OldContent string // replace / remove 的定位凭据（未替代行 content 全文）
}

// noteWriteResult 是 writeNote 的产物：fail 空 = 成功（op 给出动作）；否则失败分类 + 数字。
// entries/sums 恒带（成功 = 写后有效集，失败 = 当前有效集）——工具结果文本的清单与「已用」行；
// 清单是本场定位旧条目的唯一来源（快照冻结，规划 §1-4）。
type noteWriteResult struct {
	fail           string
	op             string // add / replace / remove
	kind           string // 目标 kind（超上限原因句取它）
	entries        []noteEntry
	sums           noteSums
	itemRunes      int    // 本条 content 的 rune 数
	quoteRunes     int    // 本条 quote 的 rune 数
	locateHits     int    // 定位结果：-1 未带 old_content；0 无命中；>1 多行命中；1 唯一命中
	oldRunes       int    // replace 中被替代行的 rune 数（超上限原因句带数字）
	projRunes      int    // replace 超上限时的替换后合计（同上）
	invisibleField string // 不可见字符拒写时：content / quote
	invisibleID    string // 不可见字符拒写时：命中的码点（invisible_unicode_U+XXXX）
}

// writeNote 是笔记的唯一写入函数（doc/17 §7 四件之 4）：BEGIN → 取 workspace 级 advisory xact 锁
// → 读未替代行算两类合计 → 按 action 走 add / replace / remove → COMMIT。锁随事务结束自动放，
// 没有「忘解锁」；两个会话同时各写一笔，必有一笔看见另一笔（锁在库不在进程，多副本同样成立）。
// 三种动作的写入语义（doc/17 §7）：
//   - add：插一行；
//   - replace：old_content 全文定位唯一一条未替代行 → 插新行 + 旧行 superseded_by 指新行
//     （旧行 content/quote 一字不改——对质证据链）；超上限检查按「旧行退出后的合计」算；
//   - remove：old_content 定位唯一一条未替代行 → 物理删除（用户明说删掉，不留指针）。
//
// 业务拒写（超上限/缺原话/定位不到/字段非法）不是 Go error——以 fail 分类回到调用方格式化成结果文本；
// 返回的 error 只承载底座故障（DB 断）。
func (s *Store) writeNote(ctx context.Context, workspace string, sourceRunID RunID, sourceSeq int64, in noteWrite) (noteWriteResult, error) {
	action := strings.TrimSpace(in.Action)
	kind := strings.TrimSpace(in.Kind)
	content := strings.TrimSpace(in.Content)
	quote := strings.TrimSpace(in.Quote)
	oldContent := strings.TrimSpace(in.OldContent)
	in = noteWrite{Action: action, Kind: kind, Content: content, Quote: quote, OldContent: oldContent}
	res := noteWriteResult{op: action, kind: kind}
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		sc := scopeFrom(ctx)
		lockKey := workspace
		if scopeActive(sc) {
			lockKey = sc.TenantID + "|" + sc.Scope + "|" + sc.UserID + "|" + workspace
		}
		if err := tx.Exec(`SELECT pg_advisory_xact_lock(hashtext(?))`, lockKey).Error; err != nil {
			return err
		}
		// 当前有效集先读出：字段/超上限/定位拒绝时结果文本也要带清单与「已用」行（planning §3-C）。
		entries, sums, err := liveNotesFrom(tx, workspace, sc)
		if err != nil {
			return err
		}
		res.entries, res.sums = entries, sums
		res.fail, res.itemRunes, res.quoteRunes = validateNoteWrite(in)
		// 不可见字符检查在字段校验之后、写库动作之前（content 与 quote 都查——两者都会进
		// 系统提示侧；remove 不写文字，跳过）。
		if res.fail == "" && action != "remove" {
			if id := scanNoteInvisible(content); id != "" {
				res.fail, res.invisibleField, res.invisibleID = noteFailInvisible, "content", id
			} else if id := scanNoteInvisible(quote); id != "" {
				res.fail, res.invisibleField, res.invisibleID = noteFailInvisible, "quote", id
			}
		}
		if res.fail != "" {
			if res.fail == noteFailLocate {
				res.locateHits = -1 // 未带 old_content（0 = 找不到、>1 = 多行命中的区分在定位后）
			}
			return nil
		}
		switch action {
		case "remove":
			old, hits, err := locateLiveNote(tx, workspace, oldContent, sc)
			if err != nil {
				return err
			}
			res.locateHits = hits
			if hits != 1 {
				res.fail = noteFailLocate
				return nil
			}
			if err := tx.Exec(`DELETE FROM notes WHERE id = ?`, old.ID).Error; err != nil {
				return err
			}
		case "replace":
			old, hits, err := locateLiveNote(tx, workspace, oldContent, sc)
			if err != nil {
				return err
			}
			res.locateHits = hits
			if hits != 1 {
				res.fail = noteFailLocate
				return nil
			}
			res.oldRunes = utf8.RuneCountInString(old.Content)
			proj := sums.plus(old.Kind, -res.oldRunes).plus(kind, res.itemRunes)
			if proj.of(kind) > noteCap(kind) {
				res.fail, res.projRunes = noteFailOverCap, proj.of(kind)
				return nil
			}
			id, err := newRowID("n_")
			if err != nil {
				return err
			}
			if err := tx.Exec(
				`INSERT INTO notes (id, workspace, kind, content, quote, source_run_id, source_seq, tenant_id, user_id, scope, created_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, now())`,
				id, workspace, kind, content, quote, sourceRunID, sourceSeq, sc.TenantID, sc.UserID, noteScopeName(sc)).Error; err != nil {
				return err
			}
			if err := tx.Exec(
				`UPDATE notes SET superseded_by = ? WHERE id = ?`, id, old.ID).Error; err != nil {
				return err
			}
		default: // add（validateNoteWrite 已把 action 收窄到三个值）
			if sums.of(kind)+res.itemRunes > noteCap(kind) {
				res.fail = noteFailOverCap
				return nil
			}
			id, err := newRowID("n_")
			if err != nil {
				return err
			}
			if err := tx.Exec(
				`INSERT INTO notes (id, workspace, kind, content, quote, source_run_id, source_seq, tenant_id, user_id, scope, created_at)
				 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, now())`,
				id, workspace, kind, content, quote, sourceRunID, sourceSeq, sc.TenantID, sc.UserID, noteScopeName(sc)).Error; err != nil {
				return err
			}
		}
		// 写后有效集（清单必须含新行——它是下一轮 replace/remove 的定位凭据）。
		entries, sums, err = liveNotesFrom(tx, workspace, sc)
		if err != nil {
			return err
		}
		res.entries, res.sums = entries, sums
		return nil
	})
	if err != nil {
		return noteWriteResult{}, err
	}
	return res, nil
}

// validateNoteWrite 跑字段级检查（不碰库的部分），命中即返回分类；通过返回 ""。
// itemRunes/quoteRunes 供原因句与超上限检查带数字。missing old_content 也归 noteFailLocate
// （调用方据此把 locateHits 置 -1，与「找不到 / 多行命中」的消息区分开）。
func validateNoteWrite(in noteWrite) (fail string, itemRunes, quoteRunes int) {
	switch in.Action {
	case "add", "replace":
		if in.Kind != noteKindAgent && in.Kind != noteKindUser {
			return noteFailKind, 0, 0
		}
		if in.Content == "" {
			return noteFailContent, 0, 0
		}
		itemRunes = utf8.RuneCountInString(in.Content)
		if in.Quote == "" {
			return noteFailQuoteEmpty, itemRunes, 0
		}
		quoteRunes = utf8.RuneCountInString(in.Quote)
		if quoteRunes > noteQuoteCap {
			return noteFailQuoteLong, itemRunes, quoteRunes
		}
		if in.Action == "replace" && in.OldContent == "" {
			return noteFailLocate, itemRunes, quoteRunes
		}
	case "remove":
		if in.OldContent == "" {
			return noteFailLocate, 0, 0
		}
	default:
		return noteFailAction, 0, 0
	}
	return "", itemRunes, quoteRunes
}

// locateLiveNote 按 content 全文在未替代行里定位（doc/17 §7：old_content 必须等于未替代行的
// content 全文）。返回首个命中与命中数；调用方只在命中数 == 1 时才允许写。
func noteScopeName(sc dataScope) string {
	if sc.Scope == "tenant" {
		return "tenant"
	}
	return "user"
}

func applyNoteScope(db *gorm.DB, sc dataScope) *gorm.DB {
	if !scopeActive(sc) {
		return db
	}
	name := noteScopeName(sc)
	db = db.Where("tenant_id = ? AND scope = ?", sc.TenantID, name)
	if name != "tenant" {
		db = db.Where("user_id = ?", sc.UserID)
	}
	return db
}

func locateLiveNote(db *gorm.DB, workspace, content string, sc dataScope) (Note, int, error) {
	var rows []Note
	q := applyNoteScope(db, sc).Where("workspace = ? AND superseded_by IS NULL AND content = ?", workspace, content)
	if err := q.Find(&rows).Error; err != nil {
		return Note{}, 0, err
	}
	if len(rows) == 0 {
		return Note{}, 0, nil
	}
	return rows[0], len(rows), nil
}

// liveNotesFrom 从给定连接读未替代行并算合计（写事务内与读口共用同一条查询）。
// 排序 kind, id：agent 先于 user、各自按 ULID 创建序——快照与清单在两种读路径下逐字节一致。
func liveNotesFrom(db *gorm.DB, workspace string, sc dataScope) ([]noteEntry, noteSums, error) {
	var rows []Note
	q := applyNoteScope(db, sc).Where("workspace = ? AND superseded_by IS NULL", workspace)
	if err := q.Order("kind, id").Find(&rows).Error; err != nil {
		return nil, noteSums{}, err
	}
	entries := make([]noteEntry, 0, len(rows))
	var sums noteSums
	for _, r := range rows {
		entries = append(entries, noteEntry{Kind: r.Kind, Content: r.Content, Quote: r.Quote})
		if r.Kind == noteKindUser {
			sums.User += utf8.RuneCountInString(r.Content)
		} else {
			sums.Agent += utf8.RuneCountInString(r.Content)
		}
	}
	return entries, sums, nil
}

// listLiveNotes 是有效集的读口（快照 handler 的 BeforeAgent 用；M5 子代理装载处同用）。
func (s *Store) listLiveNotes(ctx context.Context, workspace string) ([]noteEntry, error) {
	sc := scopeFrom(ctx)
	if !scopeActive(sc) {
		entries, _, err := liveNotesFrom(s.db.WithContext(ctx), workspace, dataScope{})
		return entries, err
	}
	userScope := sc
	userScope.Scope = "user"
	entries, _, err := liveNotesFrom(s.db.WithContext(ctx), workspace, userScope)
	if err != nil {
		return nil, err
	}
	teamScope := dataScope{TenantID: sc.TenantID, Scope: "tenant"}
	team, _, err := liveNotesFrom(s.db.WithContext(ctx), workspace, teamScope)
	if err != nil {
		return nil, err
	}
	return append(team, entries...), nil
}

// renderNoteEntry 渲染清单/快照里的一条：content 全文 + 缩进的原话行。
// quote 空则省略原话行（防御性：DDL 允许空）。
func renderNoteEntry(e noteEntry) string {
	if e.Quote == "" {
		return "- " + e.Content
	}
	return "- " + e.Content + "\n  原话：「" + e.Quote + "」"
}

// renderNoteEntryForPrompt 是快照路径的一条渲染：content 或 quote 任一会命中不可见字符检查即占位
// （原文不进跨 run 的系统提示；工具结果清单走 renderNoteEntry 原文照出，模型才拿得到
// old_content 把它 remove 掉）。
func renderNoteEntryForPrompt(e noteEntry) string {
	if id := scanNoteInvisible(e.Content); id != "" {
		return "- " + noteBlockPlaceholder(id)
	}
	if id := scanNoteInvisible(e.Quote); id != "" {
		return "- " + noteBlockPlaceholder(id)
	}
	return renderNoteEntry(e)
}

// renderNotesSnapshot 渲染系统提示末尾的快照段（两种形态，逐字照 planning §3-B）。
// run 起手渲染一次、run 内冻结：表头明说本场中途的写入下一场才可见——模型据此理解
// 为何工具结果里的清单比快照新（清单恒带全文，replace 定位靠它）。毒行走占位分支。
func renderNotesSnapshot(entries []noteEntry) string {
	if len(entries) == 0 {
		return "笔记：（空）"
	}
	var b strings.Builder
	b.WriteString("笔记（本场开始时读取，本场中途的写入要到下一场才进入这里）：")
	for _, e := range entries {
		b.WriteString("\n" + renderNoteEntryForPrompt(e))
	}
	return b.String()
}

// renderRunNotes 是一场 run 起手真正塞进系统提示的笔记段。没有身份时字节与 renderNotesSnapshot 相同。
// 有身份时分成团队记忆和个人记忆：模型读得到团队规矩，写入口仍然只开个人。
func renderRunNotes(ctx context.Context, store *Store, workspace string) (string, error) {
	sc := scopeFrom(ctx)
	db := store.db.WithContext(ctx)
	if !scopeActive(sc) {
		entries, _, err := liveNotesFrom(db, workspace, dataScope{})
		if err != nil {
			return "", err
		}
		return renderNotesSnapshot(entries), nil
	}
	teamScope := dataScope{TenantID: sc.TenantID, Scope: "tenant"}
	userScope := sc
	userScope.Scope = "user"
	team, _, err := liveNotesFrom(db, workspace, teamScope)
	if err != nil {
		return "", err
	}
	user, _, err := liveNotesFrom(db, workspace, userScope)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("团队记忆：")
	writeNoteSection(&b, team)
	b.WriteString("\n个人记忆：")
	writeNoteSection(&b, user)
	return b.String(), nil
}

func writeNoteSection(b *strings.Builder, entries []noteEntry) {
	if len(entries) == 0 {
		b.WriteString("\n（空）")
		return
	}
	for _, e := range entries {
		b.WriteString("\n" + renderNoteEntryForPrompt(e))
	}
}

// memoryToolDesc 是 memory 工具的说明文案（逐字照 planning §3-A，不改写）。
const memoryToolDesc = `把以后每一场 run 都要遵守的事实写进笔记。笔记会进以后每一场的系统提示，所以只写短句。
action=add 是新的一条，必须带 quote，quote 是用户原句。
action=replace 是用户改了口，用快照里那条 content 全文定位旧行，并带上新的 quote。
不写：任务进度、刚做完的步骤、从文件里能再读到的内容。`

// memoryInput 是 memory 的工具入参。全部字段 omitempty（schema 层非必填）：
// 条件必填（add/replace 带 quote、replace/remove 带 old_content）在描述与写入函数里管——
// schema 层必填会让 remove 这类不带 quote 的调用被 provider 直接拒（strict 档）。
type memoryInput struct {
	Action     string `json:"action,omitempty" jsonschema:"description=add=新记一条（必须带 quote）；replace=用户改了口（用 old_content 定位旧行）；remove=删掉一条（用 old_content 定位）"`
	Kind       string `json:"kind,omitempty" jsonschema:"description=agent 或 user：agent=以后每场 run 都要遵守的事实；user=用户偏好；add / replace 必填，remove 不用"`
	Content    string `json:"content,omitempty" jsonschema:"description=要记下的一句短句（add / replace 必填）"`
	Quote      string `json:"quote,omitempty" jsonschema:"description=用户原句（≤200 字）；add / replace 必填——没有原话就不记"`
	OldContent string `json:"old_content,omitempty" jsonschema:"description=replace / remove 用：要定位的那条旧内容，必须与工具结果清单里那条 content 全文逐字一致"`
}

// newMemoryTool 造 memory 工具：笔记的唯一工具入口（写入逻辑全在 writeNote）。
// 业务拒写（超上限/缺原话/定位失败/字段非法）格式化成结果文本回注模型（planning §1-7）——不经 Go error 通道；
// 底座故障（DB 断）上抛给 wrapToolErrors 兜底（装配点在 NewWorker，同其余第一方工具）。
// 写入失败 tick noteBox 的每 run 计数器（M3）：只失败调用 tick，成功既不加也不清——第 4 次起结果里
// 追加劝停句（memoryStopAdvice），计数随 run 死（noteBox 每 run 新建）。
func newMemoryTool(store *Store, workspace string) (tool.BaseTool, error) {
	return utils.InferTool("memory", memoryToolDesc, func(ctx context.Context, in memoryInput) (string, error) {
		box := noteBoxFrom(ctx)
		if box == nil {
			return "", errors.New("memory 工具缺 run 上下文（noteBox 无 source run/seq）")
		}
		// 模型只能写个人笔记。团队笔记走管理接口，不从工具参数里取 scope。
		if sc := scopeFrom(ctx); scopeActive(sc) {
			sc.Scope = "user"
			ctx = withScope(ctx, sc)
		}
		res, err := store.writeNote(ctx, workspace, box.run, box.userSeq, noteWrite{
			Action: in.Action, Kind: in.Kind, Content: in.Content, Quote: in.Quote, OldContent: in.OldContent,
		})
		if err != nil {
			return "", err
		}
		advise := ""
		if res.fail != "" {
			advise = box.tickFailure()
		}
		return memoryResultText(res, advise), nil
	})
}

// memoryResultText 渲染工具结果（planning §3-C）：第一行（成功 = 已记下。/ 失败 = 原因句）
// + 有效条目清单 + 劝停句（advise 非空时，第 4 次起）+ 末行已用。
// 清单不能省——本场里它是定位旧条目的唯一凭据（快照冻结）；已用恒为末行（劝停句插在它前面）。
func memoryResultText(res noteWriteResult, advise string) string {
	var b strings.Builder
	if res.fail == "" {
		switch res.op {
		case "replace":
			b.WriteString("已替换。")
		case "remove":
			b.WriteString("已删除。")
		default:
			b.WriteString("已记下。")
		}
	} else {
		b.WriteString(noteFailText(res))
	}
	for _, e := range res.entries {
		b.WriteString("\n" + renderNoteEntry(e))
	}
	if advise != "" {
		b.WriteString("\n" + advise)
	}
	fmt.Fprintf(&b, "\n已用：agent %d/%d user %d/%d", res.sums.Agent, noteCapAgent, res.sums.User, noteCapUser)
	return b.String()
}

// memoryStopAdvice 是第 4 次写入失败起追加的劝停句（M3 钉死的固定句，doc/17 §7 四件之 2 的字面：
// 停止再调 memory，先回复用户）。带次数，模型能看出这是累积而非偶发。
func memoryStopAdvice(n int) string {
	return fmt.Sprintf("警告：本场 run 内已第 %d 次写入失败——停止再调 memory，先回复用户。", n)
}

// noteFailText 把失败分类翻成给模型的原因句（带数字，模型才知道要不要先腾空间）。
func noteFailText(res noteWriteResult) string {
	switch res.fail {
	case noteFailAction:
		return "错误：action 只能是 add / replace / remove。"
	case noteFailKind:
		return "错误：kind 只能是 agent 或 user。"
	case noteFailContent:
		return "错误：content 是空的——没有要记的字。"
	case noteFailQuoteEmpty:
		return "错误：add / replace 必须带 quote（用户原句）——没有原话就不记。"
	case noteFailQuoteLong:
		return fmt.Sprintf("错误：quote %d 字超过 %d 字上限——只贴用户说的那句，不要转述。", res.quoteRunes, noteQuoteCap)
	case noteFailLocate:
		switch {
		case res.locateHits < 0:
			return "错误：replace / remove 必须带 old_content——它是工具结果清单里那条 content 的全文。"
		case res.locateHits == 0:
			return "错误：old_content 与任何有效条目的 content 都不完全一致——请用工具结果清单里那条 content 的全文（逐字）。"
		default:
			return fmt.Sprintf("错误：old_content 命中 %d 条有效条目，无法确定是哪条——先核对清单，把要改/删的那条改成唯一命中。", res.locateHits)
		}
	case noteFailInvisible:
		return fmt.Sprintf("错误：%s 含人眼不可见字符（%s）——零宽 / bidi 控制符在页面上看不见、会随笔记进以后每一场的系统提示；删掉该字符或用可见写法再记。",
			res.invisibleField, res.invisibleID)
	case noteFailOverCap:
		if res.op == "replace" {
			return fmt.Sprintf("错误：%s 类替换后 %d/%d 字（新条目 %d 字，被替换的旧条目 %d 字）——还得先删掉别的旧条目。",
				res.kind, res.projRunes, noteCap(res.kind), res.itemRunes, res.oldRunes)
		}
		return fmt.Sprintf("错误：%s 类笔记已用 %d/%d 字，本条 %d 字放不下——先改口或删掉旧条目再记。",
			res.kind, res.sums.of(res.kind), noteCap(res.kind), res.itemRunes)
	default:
		return "错误：笔记写入失败（未知原因）。"
	}
}

// noteBox 是 run 级的笔记容器（ctx 携带，比照 foldBox / agentModelTable 先例）：
// worker.Run 建（带 run id 与 user 行 runlog seq），快照 handler 的 BeforeAgent 填渲染快照、
// BMR 钩子读快照重写 system，memory 工具读 run/seq 填 source 两列、累计写入失败次数（M3）。
type noteBox struct {
	mu      sync.RWMutex
	session SessionID
	run     RunID
	userSeq int64  // 本 run user 行的 runlog seq（source_seq）
	snap    string // 渲染后的快照段：run 起手填一次，run 内冻结
	fails   int    // 本 run 写入失败次数（只由 tickFailure 增；随 run 死）
	spills  int    // 本 run 截断落盘计数器（只由 tickSpill 增；文件名顺编用，随 run 死）
	bashes  int    // 本 run bash 落盘计数器（只由 tickBash 增；stdout/stderr 各占一个顺号，随 run 死）
}

func newNoteBox(session SessionID, run RunID, userSeq int64) *noteBox {
	return &noteBox{session: session, run: run, userSeq: userSeq}
}

// tickFailure 记一次写入失败并返回劝停句（空串 = 还没到第 4 次）。只失败调用 tick：
// 成功的调用不加也不清（计数语义 = 本 run 累计失败数），第 4 次起每次失败都带劝停句。
func (b *noteBox) tickFailure() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fails++
	if b.fails <= noteFailAdviseAfter {
		return ""
	}
	return memoryStopAdvice(b.fails)
}

// tickSpill 记一次截断落盘、返回本 run 内的顺编号（从 1 起）。主代理与子代理共用同一份
// noteBox（ctx 流到整棵执行树）= 同序列顺编、天然不撞（doc/21 §3.2「截断落盘」）。
func (b *noteBox) tickSpill() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.spills++
	return b.spills
}

// tickBash 记一次 bash 调用、返回本 run 内的顺编号（从 1 起）。一次 bash 调用占一个号：
// stdout 与 stderr 各写一个文件（`.out` / `.err`），同号不同名，不会互相盖。
func (b *noteBox) tickBash() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.bashes++
	return b.bashes
}

func (b *noteBox) setSnapshot(s string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.snap = s
}

func (b *noteBox) snapshot() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.snap
}

// noteBoxKey 是 ctx 携带笔记容器的私有 key 类型（防撞车惯例同 foldBoxKey）。
type noteBoxKey struct{}

func withNoteBox(ctx context.Context, b *noteBox) context.Context {
	return context.WithValue(ctx, noteBoxKey{}, b)
}

// noteBoxFrom 取回容器；缺失返回 nil（旁路调用没有容器，调用方各自显式处理）。
func noteBoxFrom(ctx context.Context) *noteBox {
	b, _ := ctx.Value(noteBoxKey{}).(*noteBox)
	return b
}

// 编译期锚：接口一缺，编译不过（其余方法由 Base 兜底）。
var _ adk.ChatModelAgentMiddleware = (*noteSnapshotHandler)(nil)

// noteSnapshotHandler 是笔记快照缝（主代理侧，planning §5）：BeforeAgent 在 run 起手读未替代行、
// 渲染快照段冻进 noteBox（+ dev 日志行——观测面，转正点登记在 planning §7）；
// BeforeModelRewriteState 每轮把 state.Messages[0] 重写为 Instruction + "\n\n" + 快照段（幂等）。
// 挂载序 = 装载 handler 后、midfold 前：midfold 的 convertState 从 state 取 system 计数/折叠，
// 数到的就是模型实见的（含快照）。子代理不走本 handler（其快照缝在 agents.go 装载处）。
type noteSnapshotHandler struct {
	*adk.BaseChatModelAgentMiddleware
	store     *Store
	workspace string
	system    string // Instruction 原文：重写 system 时拼它 + "\n\n" + 快照段
}

func newNoteSnapshotHandler(store *Store, workspace, system string) *noteSnapshotHandler {
	return &noteSnapshotHandler{BaseChatModelAgentMiddleware: &adk.BaseChatModelAgentMiddleware{},
		store: store, workspace: workspace, system: system}
}

// BeforeAgent：run 起手渲染一次快照（每 Run 调一次，先于任何模型调用）。读库失败 = run 走 error：
// 快照读不到，笔记层不可信，静默空快照会让模型按无规矩行事。无 noteBox = 旁路（直测 handler）。
func (h *noteSnapshotHandler) BeforeAgent(ctx context.Context, runCtx *adk.ChatModelAgentContext) (context.Context, *adk.ChatModelAgentContext, error) {
	box := noteBoxFrom(ctx)
	if box == nil {
		return ctx, runCtx, nil
	}
	snap, err := renderRunNotes(ctx, h.store, h.workspace)
	if err != nil {
		return ctx, runCtx, err
	}
	box.setSnapshot(snap)
	log.Printf("note snapshot %s/%s:\n%s", box.session, box.run, snap)
	return ctx, runCtx, nil
}

// BeforeModelRewriteState：每次模型调用前重写 system（Messages[0]）为 Instruction + 快照段。
// 幂等（重写 = 赋值，不是追加）；midfold 折叠后 system 原样留队首（midfold.go），
// 本 handler 在链上先跑、重写后再交 midfold 计数。断言违例（首条非 system）返回错误、不 panic。
func (h *noteSnapshotHandler) BeforeModelRewriteState(ctx context.Context, state *adk.ChatModelAgentState, mc *adk.ModelContext) (context.Context, *adk.ChatModelAgentState, error) {
	box := noteBoxFrom(ctx)
	if box == nil {
		return ctx, state, nil
	}
	snap := box.snapshot()
	if snap == "" || len(state.Messages) == 0 {
		return ctx, state, nil // 未渲染（旁路直测）或缺消息：保持原样
	}
	if state.Messages[0].Role != schema.System {
		return ctx, state, errors.New("note snapshot: state 首条不是 system（ADK 输入形状违例）")
	}
	// 这场 run 绑了助手时，系统提示是那一行的 spec，不是进程启动时冻进 handler 的那份。
	sys := h.system
	if s, ok := instructionFrom(ctx); ok {
		sys = s
	}
	state.Messages[0].Content = sys + "\n\n" + snap
	return ctx, state, nil
}
