// pii_mount_test.go 是 PII 脱敏**三个挂载点**的钉子网（方案真源 = doc/41 §3.2）。
//
// 挂载点（两个代码位置，三个消费侧）：
//
//	① 工具统一出口（wrapToolErrors）——自家工具、media/pdf、tool_search、**MCP 工具**都走它。
//	② 模型输入的唯一转换口（toSchemaMessages）——首轮装配、mid-run fold、救援采用、guard 重试都走它。
//	③ 记忆抽取入库前（memory.go 的 writeMemoryJobResult）——密钥类整条拒收，证件类打码后入库。
package runengine

import (
	"context"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/components/tool/utils"
)

// TestPIIMountToolOutput：① 工具结果里的密钥在**返回给模型之前**就被打码。
// （这层罩住所有工具——MCP 工具的装配点也是 wrapToolErrors，doc/33 头注记着围栏后仍需打码。）
func TestPIIMountToolOutput(t *testing.T) {
	raw, err := utils.InferTool("probe", "探针", func(ctx context.Context, in struct {
		Text string `json:"text"`
	}) (string, error) {
		return "读到的内容：sk-proj-AbCdEfGhIjKlMnOpQrSt", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	wrapped, ok := wrapToolErrors(raw).(tool.InvokableTool)
	if !ok {
		t.Fatal("wrapToolErrors 应返回可调用工具")
	}
	out, err := wrapped.InvokableRun(context.Background(), `{"text":"x"}`)
	if err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	if strings.Contains(out, "EfGhIjKlMnOpQrSt") {
		t.Fatalf("工具结果未打码: %q", out)
	}
	if !strings.Contains(out, "sk-p") || !strings.Contains(out, "QrSt") {
		t.Fatalf("打码形状不对（应保留头 4 尾 4）: %q", out)
	}
}

// TestPIIMountModelInput：② 模型输入里的 PII 在**发给模型之前**被打码；
// 而 mh 行本身**不被改**（页面仍显示用户原话）。
func TestPIIMountModelInput(t *testing.T) {
	row := ModelMessage{RunID: "r_1", Seq: 1, Role: "user", Text: "我的身份证是 110101199003077777"}
	msgs := toSchemaMessages([]ModelMessage{row})
	if len(msgs) != 1 {
		t.Fatalf("msgs = %d 条, want 1", len(msgs))
	}
	if strings.Contains(msgs[0].Content, "110101199003077777") {
		t.Fatalf("模型输入未打码: %q", msgs[0].Content)
	}
	if !strings.Contains(msgs[0].Content, "110101") || !strings.Contains(msgs[0].Content, "7777") {
		t.Fatalf("打码形状不对: %q", msgs[0].Content)
	}
	if row.Text != "我的身份证是 110101199003077777" {
		t.Fatalf("mh 行被就地改了（页面要显示原话）: %q", row.Text)
	}
}

// TestPIIMountFoldPathSurvives：③ 长对话折叠后 PII 不复活。
// 折叠的原料是 mh 原文（user 行按设计存原文）→ 折叠重建走 toSchemaMessages → 出来的是打码后的。
func TestPIIMountFoldPathSurvives(t *testing.T) {
	// mh 里存的是原文（AcquireRun 写的）
	rows := []ModelMessage{
		{RunID: "r_1", Seq: 1, Role: "user", Text: "手机 13812345678"},
		{RunID: "r_1", Seq: 2, Role: "assistant", Text: "好的，记下了"},
	}
	// 折叠重建（midfold.go 的三处之一）与首轮装配走的是同一个转换口
	folded := toSchemaMessages(rows)
	for _, m := range folded {
		if strings.Contains(m.Content, "13812345678") {
			t.Fatalf("折叠重建后 PII 复活: %q", m.Content)
		}
	}
	if !strings.Contains(folded[0].Content, "138") || !strings.Contains(folded[0].Content, "5678") {
		t.Fatalf("折叠后应保留头 3 尾 4: %q", folded[0].Content)
	}
}

// TestPIIMemoryRejectsSecret：③ 记忆入库——密钥类整条拒收（不进库），证件类打码后入库。
func TestPIIMemoryRejectsSecret(t *testing.T) {
	ctx := context.Background()
	s := newMemoryStore(t)
	job := &claimedMemoryJob{ID: "mj_1", RunID: "r_1", SessionID: "s_1",
		TenantID: testOwner.TenantID, UserID: testOwner.UserID, Workspace: "/tmp/ws-test", Attempts: 1}

	stats, err := s.writeMemoryJobResult(ctx, job, 1, "这一场聊到了配置", []extractedItem{
		{Kind: "fact", Topic: "api.key", Content: "我的密钥是 sk-proj-AbCdEfGhIjKlMnOpQrSt"},
		{Kind: "fact", Topic: "user.idcard", Content: "我的身份证是 110101199003077777"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Rejected != 1 {
		t.Errorf("Rejected = %d, want 1（密钥类整条拒收）", stats.Rejected)
	}
	if stats.Items != 1 {
		t.Errorf("Items = %d, want 1（证件类打码后入库）", stats.Items)
	}
	// 库里不能有密钥
	if got := s.count(t, &MemoryItem{}, "content LIKE ?", "%EfGhIjKlMnOpQrSt%"); got != 0 {
		t.Errorf("密钥进了记忆库（%d 条）", got)
	}
	// 证件类要已打码
	var content string
	if err := s.db.Raw(`SELECT content FROM memory_items WHERE topic = ?`, "user.idcard").Row().Scan(&content); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(content, "110101199003077777") {
		t.Errorf("证件号未打码: %q", content)
	}
	if !strings.Contains(content, "110101") || !strings.Contains(content, "7777") {
		t.Errorf("打码形状不对: %q", content)
	}
}
