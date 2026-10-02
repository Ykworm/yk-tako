// 本文件：打印小工具（Clip）的单元测试。
package repl

import "testing"

// TestClip 验证多行文本被压成一行并按 rune 截断。
func TestClip(t *testing.T) {
	if got := Clip("一行\n二行", 100); got != "一行 二行" {
		t.Fatalf("换行应压成空格: %q", got)
	}
	if got := Clip("短的", 10); got != "短的" {
		t.Fatalf("短文本原样返回: %q", got)
	}
	got := Clip("一二三四五六七八九十", 5)
	if got != "一二三四五…" {
		t.Fatalf("按 rune 截断并加省略号: %q", got)
	}
}
