// 本文件：内存事件列表。两个读者各取各的起点，列表不删；装库之后按序号可读。
package webui

import "testing"

func seqs(evs []sseEvent) []int64 {
	out := make([]int64, len(evs))
	for i, ev := range evs {
		out[i] = ev.Seq
	}
	return out
}

func TestEventMemReadersDoNotConsume(t *testing.T) {
	m := newEventMem()
	for i := int64(1); i <= 20; i++ {
		m.publish(sseEvent{Run: "r", Seq: i, Kind: "message"})
	}
	at18, ok := m.after("r", 17, 50)
	if !ok {
		t.Fatal("seq=1 已发布，列表应可读")
	}
	at14, _ := m.after("r", 13, 50)
	if got, want := len(at18), 3; got != want {
		t.Fatalf("从 18 起有 %d 条，want %d，seqs=%v", got, want, seqs(at18))
	}
	if at18[0].Seq != 18 || at18[2].Seq != 20 {
		t.Fatalf("从 18 起 = %v", seqs(at18))
	}
	if at14[0].Seq != 14 || at14[len(at14)-1].Seq != 20 {
		t.Fatalf("从 14 起 = %v", seqs(at14))
	}
	again, _ := m.after("r", 0, 50)
	if len(again) != 20 {
		t.Fatalf("读过之后列表应仍是 20 条，实际 %d", len(again))
	}
}

func TestEventMemInstallKeepsLaterPublish(t *testing.T) {
	m := newEventMem()
	m.publish(sseEvent{Run: "r", Seq: 4, Kind: "message", Text: "新"})
	m.install("r", []sseEvent{
		{Run: "r", Seq: 1, Kind: "start"},
		{Run: "r", Seq: 2, Kind: "message"},
		{Run: "r", Seq: 3, Kind: "message"},
	})
	got, ok := m.after("r", 0, 50)
	if !ok || len(got) != 4 || got[3].Text != "新" {
		t.Fatalf("装库后 = %+v ok=%v", got, ok)
	}
	m.install("r", []sseEvent{{Run: "r", Seq: 1, Kind: "start", Text: "不该覆盖"}})
	got, _ = m.after("r", 0, 50)
	if len(got) != 4 || got[0].Text != "" {
		t.Fatalf("第二次装载不应覆盖，实际 %+v", got)
	}
}
