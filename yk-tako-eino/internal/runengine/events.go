// events.go 是变更事件流的 Redis 半（方案真源 = doc/46 §3.3）。
//
// tk:events:<tid> = Stream：租户级易失通知设施——doc/46 §4 登记的第三个临时键。
// 删除/改名成功后 XADD 一条 {type,id,at}；SSE 端点（B2）XREAD 挂读推给在线 client。
// MAXLEN ~1000 有界截断、不带 TTL：事件被挤出或 Redis 重启即丢——这是设计不是缺陷
// （doc/46 §3.1：拉是正确性，丢了全量拉追回真相）。租户归档时 DEL（流键无 TTL，不清理成残留）。
//
// 与前两个临时键的分工（doc/16 §2c 待扩行）：runlive/runepoch 是「run 存活态」（随 run 生灭），
// 本键是「租户级易失通知」（随租户生灭），真源都是 PG。
package runengine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

// 事件 type 词表（doc/46 §7）：只发删除/改名/创建这种「别人该失效」的变更。
// skills_changed（doc/52 D24）在安装/删除成功后发——技能目录是每个 run 拼进 system prompt
// 的派生视图，别的标签页与别的人需要知道「该重拉了」。
const (
	eventSessionDeleted   = "session_deleted"
	eventWorkspaceDeleted = "workspace_deleted"
	eventWorkspaceRenamed = "workspace_renamed"
	eventSkillsChanged    = "skills_changed"
	// tools_changed（doc/52 D36）：工具根变了。与 skills_changed 分开是**故意的**——
	// 客户端按 type 决定重拉哪一页；合一个类型会让技能页为一次工具上传白拉一遍。
	eventToolsChanged = "tools_changed"
)

// eventsMaxLen：流的有界截断（doc/46 §7 开放问题第 3 条的拍定值）。
// 覆盖「断线几分钟重连」的窗口即可；溢出 = 最老的丢，client 全量拉追回。
const eventsMaxLen = 1000

// keyEvents：tk:events:<tid> —— 租户事件流。
func keyEvents(tenantID string) string { return keyPrefix + "events:" + tenantID }

// publishEvent 追加一条变更事件。失败只记日志——事件是优化不是真源（doc/46 §3.1），
// 删除/改名在 PG 早已提交，不能反过来让请求失败。
func (s *Server) publishEvent(ctx context.Context, tenantID, typ, id string) {
	s.store.PublishEvent(ctx, tenantID, typ, id)
}

// PublishEvent 是事件写入的真身：放 Store 上，因为**安装**（doc/52 C3）在端点、工具、
// 续跑对账三处都可能收尾，并不总有一个 *Server。失败只记日志。
func (st *Store) PublishEvent(ctx context.Context, tenantID, typ, id string) {
	if tenantID == "" || id == "" {
		return
	}
	if err := st.rdb.XAdd(ctx, &redis.XAddArgs{
		Stream: keyEvents(tenantID),
		MaxLen: eventsMaxLen,
		Approx: true,
		Values: map[string]any{
			"type": typ,
			"id":   id,
			"at":   time.Now().UTC().Format(time.RFC3339),
		},
	}).Err(); err != nil {
		log.Printf("publishEvent %s %s %s: %v", tenantID, typ, id, err)
	}
}

// deleteEvents 清租户事件流：租户归档（deleteTenant）时调用。
func (st *Store) deleteEvents(ctx context.Context, tenantID string) {
	if tenantID == "" {
		return
	}
	if err := st.rdb.Del(ctx, keyEvents(tenantID)).Err(); err != nil {
		log.Printf("deleteEvents %s: %v", tenantID, err)
	}
}

// streamEvent 是 SSE 帧的 JSON 载体：seq/type/id/at 键序固定（doc/46 §3.3 的字节）。
// 只通知、不带状态——客户端收到后让本地失效并永远跟一次重拉（doc/46 §3.1）。
type streamEvent struct {
	Seq  string `json:"seq"`
	Type string `json:"type"`
	ID   string `json:"id"`
	At   string `json:"at"`
}

// getEvents：账号级变更事件流（doc/46 Loop B）。SSE 帧面不进信封（doc/40 §6 的同款裁决）。
//
// 游标语义：新连接不带 Last-Event-ID = 从 `$` 挂起，只推**之后**的新事件——历史对刚 reconcile
// 完的客户端无意义（真源是重拉），推了反而让它做无用失效；带 Last-Event-ID = 从它之后续读，
// 断线重连不丢窗口内事件。挂读是 XREAD BLOCK：任何副本写的事件任何副本的连接都读得到
// （共享 Redis，doc/46 §3.3 的多副本理由）；BLOCK 超时就再挂一轮，无事件期间不写字节。
func (s *Server) getEvents(c *gin.Context) {
	p, ok := principalFrom(c)
	if !ok {
		writeEnvelope(c, http.StatusUnauthorized, codeMissingToken, "请先登录", nil)
		return
	}
	w := c.Writer
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeEnvelope(c, http.StatusInternalServerError, codeInternal, "服务暂时不可用，请再试一次", nil)
		return
	}
	ctx := c.Request.Context()
	key := keyEvents(p.TenantID)
	lastID := strings.TrimSpace(c.GetHeader("Last-Event-ID"))
	if lastID == "" {
		lastID = "$" // 只推新事件
	}
	for {
		res, err := s.store.rdb.XRead(ctx, &redis.XReadArgs{
			Streams: []string{key, lastID},
			Count:   100,
			Block:   10 * time.Second,
		}).Result()
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return // 客户端断开 / 请求收尾
			}
			if errors.Is(err, redis.Nil) {
				continue // BLOCK 超时：本轮无事件，继续挂读
			}
			log.Printf("GET /api/events XREAD: %v", err)
			return
		}
		for _, msg := range res {
			for _, e := range msg.Messages {
				b, mErr := json.Marshal(streamEvent{
					Seq:  e.ID,
					Type: fmt.Sprint(e.Values["type"]),
					ID:   fmt.Sprint(e.Values["id"]),
					At:   fmt.Sprint(e.Values["at"]),
				})
				if mErr != nil {
					continue
				}
				fmt.Fprintf(w, "id: %s\ndata: %s\n\n", e.ID, b)
				lastID = e.ID
			}
		}
		flusher.Flush()
	}
}
