package tools

import "testing"

// TestEval 验证四则运算结果和白名单之外字符的拒绝。
func TestEval(t *testing.T) {
	got, err := Eval("1234*5678")
	if err != nil || got != "7006652" {
		t.Fatalf("1234*5678 => %q %v", got, err)
	}
	got, err = Eval("(100+200)/3")
	if err != nil || got != "100" {
		t.Fatalf("(100+200)/3 => %q %v", got, err)
	}
	if _, err := Eval("rm -rf /"); err == nil {
		t.Fatal("expected rejection")
	}
}
