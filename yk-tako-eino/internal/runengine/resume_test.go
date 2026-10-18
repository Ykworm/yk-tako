package runengine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestClaimResumeErrors 钉 A4a 的接管 CAS 四态与边界（doc/30 §8.2）：
// 行不存在 404 → 非 running 409 not_resumable → 租约仍有效 409 run_alive → 次数用尽 409
// resume_exhausted；成功路径验 epoch+1、租约续上、账本在事务内读出。
// 另外验 AbandonClaim 的补偿语义：租约立刻过期，下一发可接，epoch 不回退。
func TestClaimResumeErrors(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, testLease, testTTL)
	expire := func(rid RunID) {
		t.Helper()
		if err := s.db.WithContext(ctx).Exec(
			`UPDATE runs SET lease_until = now() - interval '1 second' WHERE id = ?`, rid).Error; err != nil {
			t.Fatalf("造过期租约 %s: %v", rid, err)
		}
	}

	// 1. 行不存在 → errRunNotFound（handler 映射 404）。
	if _, err := s.ClaimResume(ctx, "r_a4a_nope", 3); !errors.Is(err, errRunNotFound) {
		t.Fatalf("未知 run claim err = %v, want errRunNotFound", err)
	}

	// 2. 已定稿 → errNotResumable（handler 映射 409；终态没有可续的东西）。
	sidDone, ridDone := SessionID("s_a4a_done"), RunID("r_a4a_done")
	s.mustAcquire(t, sidDone, ridDone, "m_a4a_done", "hi")
	if _, _, ok, err := s.EndRun(ctx, sidDone, ridDone, 1, "complete", nil, nil, nil); err != nil || !ok {
		t.Fatalf("EndRun = (ok=%v, err=%v), want ok", ok, err)
	}
	if _, err := s.ClaimResume(ctx, ridDone, 3); !errors.Is(err, errNotResumable) {
		t.Fatalf("complete run claim err = %v, want errNotResumable", err)
	}

	// 3. 租约仍有效 → errRunAlive（handler 映射 409；抢会造出双执行者）。
	sid, rid := SessionID("s_a4a"), RunID("r_a4a")
	s.mustAcquire(t, sid, rid, "m_a4a", "hi")
	if _, err := s.ClaimResume(ctx, rid, 3); !errors.Is(err, errRunAlive) {
		t.Fatalf("活租约 claim err = %v, want errRunAlive", err)
	}
	var epochAfterAlive int64
	if err := s.db.WithContext(ctx).Raw(`SELECT lease_epoch FROM runs WHERE id = ?`, rid).Scan(&epochAfterAlive).Error; err != nil {
		t.Fatal(err)
	}
	if epochAfterAlive != 1 {
		t.Fatalf("run_alive 拒掉后 epoch = %d, want 1（不许烧令牌）", epochAfterAlive)
	}

	// 4. 成功：epoch 1→2、租约续回、账本随事务读出。
	expire(rid)
	if err := s.InsertJournal(ctx, RunJournal{RunID: rid, Epoch: 1, Role: "assistant", Text: "半场"}); err != nil {
		t.Fatalf("InsertJournal: %v", err)
	}
	claim, err := s.ClaimResume(ctx, rid, 3)
	if err != nil {
		t.Fatalf("ClaimResume: %v", err)
	}
	if claim.Epoch != 2 || claim.SessionID != sid {
		t.Fatalf("claim = {epoch %d, session %s}, want {2, %s}", claim.Epoch, claim.SessionID, sid)
	}
	if len(claim.Rows) != 1 || claim.Rows[0].Text != "半场" || claim.Rows[0].Epoch != 1 {
		t.Fatalf("claim.Rows = %+v, want 1 行 epoch1 原文", claim.Rows)
	}
	if !s.sqlBool(t, `SELECT status = 'running' AND lease_until > now() AND finished_at IS NULL
	                     AND lease_epoch = 2 FROM runs WHERE id = ?`, rid) {
		t.Fatal("claim 后应为 running + 续租 + finished_at=NULL + epoch=2")
	}

	// 5. 次数边界：epoch=2 → resumeCount=1 < 2 可接（→3）；epoch=3 → 2 >= 2 用尽。
	expire(rid)
	claim2, err := s.ClaimResume(ctx, rid, 2)
	if err != nil || claim2.Epoch != 3 {
		t.Fatalf("边界内 claim = (epoch %d, err %v), want (3, nil)", claim2.Epoch, err)
	}
	expire(rid)
	if _, err := s.ClaimResume(ctx, rid, 2); !errors.Is(err, errResumeExhausted) {
		t.Fatalf("次数用尽 claim err = %v, want errResumeExhausted", err)
	}

	// 6. AbandonClaim 补偿：租约置过期、epoch 不回退，下一发立刻可接。
	if err := s.AbandonClaim(ctx, rid, claim2.Epoch); err != nil {
		t.Fatalf("AbandonClaim: %v", err)
	}
	claim3, err := s.ClaimResume(ctx, rid, 5)
	if err != nil || claim3.Epoch != 4 {
		t.Fatalf("补偿后 claim = (epoch %d, err %v), want (4, nil)", claim3.Epoch, err)
	}
}

// TestResumeHandlerErrorMapping 钉 handler 的两条早退映射（每一条都必须在 claim 前/claim 里
// 干净拒掉，不开 worker、不动 Redis）：未知 run 404、已定稿 409 not_resumable。
// 另两态（run_alive / resume_exhausted）在 TestClaimResumeErrors 里按哨兵验。
func TestResumeHandlerErrorMapping(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t, testLease, testTTL)
	srv := &Server{store: s, resumeMax: 3}
	call := func(rid RunID) (*httptest.ResponseRecorder, map[string]any) {
		t.Helper()
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/runs/"+string(rid)+"/resume", nil)
		c.Params = gin.Params{{Key: "runId", Value: string(rid)}}
		srv.resumeRun(c)
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("响应不是 JSON: %q", w.Body.String())
		}
		return w, body
	}

	// 404：未知 run（账本读空，落到 claim 的 errRunNotFound）。
	if w, body := call("r_a5_missing"); w.Code != http.StatusNotFound || body["error"] != "run_not_found" {
		t.Fatalf("未知 run = %d %v, want 404 run_not_found", w.Code, body)
	}

	// 409 not_resumable：已定稿的 run 没有可续的东西；行状态原样不动。
	sid, rid := SessionID("s_a5_map"), RunID("r_a5_map")
	s.mustAcquire(t, sid, rid, "m_a5_map", "hi")
	if _, _, ok, err := s.EndRun(ctx, sid, rid, 1, "complete", nil, nil, nil); err != nil || !ok {
		t.Fatalf("EndRun = (ok=%v, err=%v), want ok", ok, err)
	}
	if w, body := call(rid); w.Code != http.StatusConflict || body["error"] != "not_resumable" {
		t.Fatalf("complete run resume = %d %v, want 409 not_resumable", w.Code, body)
	}
	var epoch int64
	if err := s.db.WithContext(ctx).Raw(`SELECT lease_epoch FROM runs WHERE id = ?`, rid).Scan(&epoch).Error; err != nil {
		t.Fatal(err)
	}
	if epoch != 1 {
		t.Fatalf("拒掉后 epoch = %d, want 1", epoch)
	}
}
