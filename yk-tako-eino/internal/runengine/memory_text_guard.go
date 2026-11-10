package runengine

// 本文件是「要写进记忆的文本」的准入检查（方案真源 = doc/65 §5.3；施工包 §8 / §11.3）。
//
// 一份实现两个消费侧：
//
//	notes.go        —— pipeline 模式的常驻记忆段与观测口渲染（原 notes 表线，已改读新表）
//	memory_markdown.go —— markdown 模式的文件条目
//
// 两条线共用同一套准入：正文都会进**以后每一场**的系统提示或常驻文件，
// 标准不该有两份。
//
// 两类检查各防一件事：
//
//	隐形字符（零宽 / bidi 控制符）：这一行会进**以后每一场**的系统提示。人眼在页面上
//	  看不见，模型却每场原样读到——把一句「忽略之前所有指令」藏在记忆里，比藏在对话里
//	  更难被发现。所以写入时拒，并当场报出码点（调用方看得见是哪个字符，能改）。
//	PII（密钥 / 证件）：密钥类**整条拒收**（记忆要可信的完整事实，密钥不该长期留在盘上）；
//	  证件类打码后放行（可复用、不可直接利用）——与 pii.go 的既有口径一致。
//
// 清单裁剪的依据（2026-10-15 十七修，用户裁定）：原句式神词表经真码探针实测后裁除
// （纯中文写法一条不命中，锚词换中文、写全角都能绕）；隐形字符清单有限、能枚举尽、
// 与语言无关——拦的是「能不能藏」不是「说了什么」。

import (
	"fmt"
	"strings"
)

// memoryInvisibleRunes 是隐形字符清单（17 个码点）。
const memoryInvisibleRunes = "\u200b\u200c\u200d\u2060\u2062\u2063\u2064\ufeff\u202a\u202b\u202c\u202d\u202e\u2066\u2067\u2068\u2069"

// scanMemoryInvisible 扫一段文字里的隐形字符，返回命中说明
// （"invisible_unicode_U+XXXX"——拒写文案与占位文案照报，调用方/用户当场看见是哪个码点；
// 空串 = 无命中）。
func scanMemoryInvisible(text string) string {
	for _, r := range text {
		if strings.ContainsRune(memoryInvisibleRunes, r) {
			return fmt.Sprintf("invisible_unicode_U+%04X", r)
		}
	}
	return ""
}

// memoryTextGuard 是一次准入检查的结果。
type memoryTextGuard struct {
	// Invisible 是命中的隐形字符码点（空 = 无）。有它 = 拒写。
	Invisible string
	// Secret 报「命中密钥类 PII」= 整条拒收。
	Secret bool
	// Text 是通过后的文本（证件类已打码）。
	Text string
	// Masked 报「原文里有证件类 PII，已打码」。
	Masked bool
}

// guardMemoryText 对一条要写进记忆的文本做准入检查。
//
// 顺序：隐形字符 → 密钥类 PII → 其余 PII 打码。
// 顺序有讲究：隐形字符先查，因为它是**藏**（模型读得到、人看不见），比证件号严重；
// 密钥类拒收，因为它不该以任何形态留在长期存储里。
func guardMemoryText(text string) memoryTextGuard {
	g := memoryTextGuard{Text: text}
	if id := scanMemoryInvisible(text); id != "" {
		g.Invisible = id
		return g
	}
	masked, hits := MaskPII(text)
	if hasSecretPII(hits) {
		g.Secret = true
		return g
	}
	g.Text = masked
	g.Masked = masked != text
	return g
}
