package planexec

import "testing"

func TestClassifyPlan(t *testing.T) {
	kind, steps, response := Classify(`{"steps":["查现在时间","算 1234*5678","写总结"]}`)
	if kind != KindPlan {
		t.Fatalf("kind = %v, want KindPlan", kind)
	}
	if len(steps) != 3 || steps[0] != "查现在时间" {
		t.Fatalf("steps = %v", steps)
	}
	if response != "" {
		t.Fatalf("response = %q, want empty", response)
	}
}

func TestClassifyRespond(t *testing.T) {
	kind, steps, response := Classify(`{"response":"现在是 2026 年，乘积是 7006652。"}`)
	if kind != KindRespond {
		t.Fatalf("kind = %v, want KindRespond", kind)
	}
	if response == "" {
		t.Fatal("response is empty")
	}
	if steps != nil {
		t.Fatalf("steps = %v, want nil", steps)
	}
}

func TestClassifyText(t *testing.T) {
	cases := []string{
		"现在是下午三点。",         // 普通中文文本
		"not json at all",  // 普通英文文本
		`{"steps":[]}`,     // 空计划不当计划
		`{}`,               // 空对象
		`{"other":1}`,      // 无关 JSON
		`{"steps":"不是数组"}`, // 形状不符
		"123",              // 非对象 JSON
		"",                 // 空串
	}
	for _, c := range cases {
		if kind, _, _ := Classify(c); kind != KindText {
			t.Errorf("Classify(%q) kind = %v, want KindText", c, kind)
		}
	}
}
