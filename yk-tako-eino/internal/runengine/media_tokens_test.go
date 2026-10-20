package runengine

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestDeepSeekImageTokens(t *testing.T) {
	cases := []struct{ w, h, want int }{
		{100, 100, 184},
		{544, 544, 184},
		{640, 480, 206},
		{800, 800, 422},
		{1024, 768, 496},
		{1300, 1300, 994},
		{2000, 2000, 994},
		{300, 50, 200},
		{8192, 100, 593},
		{16, 8192, 590},
		{12, 1123, 380},
		{89, 2076, 254},
		{9000, 1, 1024},
		{1, 9000, 1024},
	}
	for _, tc := range cases {
		if got := deepSeekImageTokens(tc.w, tc.h); got != tc.want {
			t.Errorf("%d×%d = %d, want %d", tc.w, tc.h, got, tc.want)
		}
	}
	if got := deepSeekImageTokens(0, 10); got != 0 {
		t.Errorf("非正尺寸 = %d, want 0", got)
	}
}

func TestImageDimensions(t *testing.T) {
	dir := t.TempDir()
	rgba := func(w, h int) *image.RGBA {
		return image.NewRGBA(image.Rect(0, 0, w, h))
	}
	write := func(name string, encode func(path string) error, w, h int) {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := encode(path); err != nil {
			t.Fatal(err)
		}
		gotW, gotH, err := imageDimensions(path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if gotW != w || gotH != h {
			t.Fatalf("%s = %d×%d, want %d×%d", name, gotW, gotH, w, h)
		}
	}
	write("a.png", func(path string) error {
		f, err := os.Create(path)
		if err != nil {
			return err
		}
		defer f.Close()
		return png.Encode(f, rgba(6, 7))
	}, 6, 7)
	write("a.jpg", func(path string) error {
		f, err := os.Create(path)
		if err != nil {
			return err
		}
		defer f.Close()
		return jpeg.Encode(f, rgba(4, 3), nil)
	}, 4, 3)
	write("a.gif", func(path string) error {
		f, err := os.Create(path)
		if err != nil {
			return err
		}
		defer f.Close()
		pal := color.Palette{color.Black, color.White}
		img := image.NewPaletted(image.Rect(0, 0, 5, 2), pal)
		return gif.Encode(f, img, nil)
	}, 5, 2)
	webpPath := filepath.Join(dir, "a.webp")
	if err := os.WriteFile(webpPath, tinyWebP, 0o644); err != nil {
		t.Fatal(err)
	}
	w, h, err := imageDimensions(webpPath)
	if err != nil {
		t.Fatalf("webp: %v", err)
	}
	if w != 2 || h != 3 {
		t.Fatalf("webp = %d×%d, want 2×3", w, h)
	}
}

// tinyWebP 是 cwebp -lossless 压出来的 2×3 WebP。Go 的 webp 解码器认这版头。
var tinyWebP = []byte{
	0x52, 0x49, 0x46, 0x46, 0x1c, 0x00, 0x00, 0x00, 0x57, 0x45, 0x42, 0x50,
	0x56, 0x50, 0x38, 0x4c, 0x0f, 0x00, 0x00, 0x00, 0x2f, 0x01, 0x80, 0x00,
	0x00, 0x07, 0x10, 0xfd, 0x8f, 0xfe, 0x07, 0x22, 0xa2, 0xff, 0x01, 0x00,
}

func TestCountInputMediaTokens(t *testing.T) {
	counter := fakeCounter{f: func(s string) int { return len([]rune(s)) }}
	row := ModelMessage{Role: "user", Text: "hello"}
	base := countInput(counter, "sys", []ModelMessage{row})
	row.MediaTokens = 422
	if got := countInput(counter, "sys", []ModelMessage{row}); got != base+422 {
		t.Fatalf("countInput = %d, want %d", got, base+422)
	}
	raw, err := json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("MediaTokens")) || bytes.Contains(raw, []byte("422")) {
		t.Fatalf("图片 token 进了 JSON: %s", raw)
	}
}

func TestAnnotateMediaTokens(t *testing.T) {
	raw := testPNG(t, 800, 800)
	if deepSeekImageTokens(800, 800) != 422 {
		t.Fatal("800×800 应是 422，后面的断言都靠这个数")
	}
	rel := "a3-red-800.png"
	full := filepath.Join(t.TempDir(), rel)
	if err := os.WriteFile(full, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	userLine := mediaPlaceholder("a.png", 1, rel, "image/png", len(raw), shaOf(raw))
	toolLine := mediaReceiptPlaceholder(rel, "image/png", len(raw), shaOf(raw))
	var reads int
	resolve := func(context.Context, string) (string, error) {
		reads++
		return full, nil
	}

	t.Run("只算最后一条 user 和未提交的 tool", func(t *testing.T) {
		reads = 0
		box := newFoldBox("s1", "r1")
		box.noteToolMediaPending("call_done")
		box.commitMediaSend()
		rows := []ModelMessage{
			{Role: "user", Text: "旧\n" + userLine},
			{Role: "tool", ToolCallID: "call_old", Text: toolLine},
			{Role: "assistant", Text: "中间"},
			{Role: "user", Text: "看\n" + userLine + "\n<memory-context>\n" + userLine},
			{Role: "tool", ToolCallID: "call_new", Text: toolLine},
			{Role: "tool", ToolCallID: "call_done", Text: toolLine},
		}
		ctx := withRunProfile(withFoldBox(context.Background(), box), &RunProfile{Vision: true, Provider: "deepseek"})
		annotateMediaTokens(ctx, rows, box, resolve)
		if rows[0].MediaTokens != 0 || rows[1].MediaTokens != 0 || rows[2].MediaTokens != 0 {
			t.Fatalf("历史 user / 历史 tool / assistant 不应计价: %+v", rows[:3])
		}
		if rows[3].MediaTokens != 422 {
			t.Fatalf("最后一条 user = %d, want 422（召回块里的占位不算）", rows[3].MediaTokens)
		}
		if rows[4].MediaTokens != 422 {
			t.Fatalf("未提交 tool = %d, want 422", rows[4].MediaTokens)
		}
		if rows[5].MediaTokens != 0 {
			t.Fatalf("已提交 tool = %d, want 0", rows[5].MediaTokens)
		}
		if reads != 1 {
			t.Fatalf("同一 path+sha 应只读一次，读了 %d 次", reads)
		}
		annotateMediaTokens(ctx, rows, box, resolve)
		if reads != 1 {
			t.Fatalf("缓存命中后又读了盘，reads=%d", reads)
		}
	})

	t.Run("第一枪已发则 user 为 0", func(t *testing.T) {
		box := newFoldBox("s1", "r1")
		box.noteUserMediaPending()
		box.commitMediaSend()
		rows := []ModelMessage{
			{Role: "user", Text: userLine},
			{Role: "tool", ToolCallID: "call_new", Text: toolLine},
		}
		ctx := withRunProfile(withFoldBox(context.Background(), box), &RunProfile{Vision: true, Provider: "deepseek"})
		annotateMediaTokens(ctx, rows, box, resolve)
		if rows[0].MediaTokens != 0 {
			t.Fatalf("已发 user = %d, want 0", rows[0].MediaTokens)
		}
		if rows[1].MediaTokens != 422 {
			t.Fatalf("未发 tool 仍应计价，got %d", rows[1].MediaTokens)
		}
	})

	t.Run("只有 tool 行的 delta 也计价", func(t *testing.T) {
		box := newFoldBox("s1", "r1")
		box.setMediaResolve(resolve)
		ctx := withRunProfile(withFoldBox(context.Background(), box), &RunProfile{Vision: true, Provider: "deepseek"})
		counter := fakeCounter{f: func(s string) int { return len([]rune(s)) }}
		msgs := []*schema.Message{
			schema.SystemMessage("人设"),
			schema.UserMessage("问"),
			runest("答", "", 10, 0, -1),
			schema.ToolMessage(toolLine, "call_new"),
		}
		est, ok := usageBasedInput(ctx, counter, "r1", msgs)
		if !ok {
			t.Fatal("有账单应走 usage")
		}
		want := counter.Count(toolLine) + 422
		if est.delta != want || est.baseline != 10 {
			t.Fatalf("est = %+v, want baseline 10、delta %d", est, want)
		}
	})

	t.Run("Vision=false 全 0", func(t *testing.T) {
		before := reads
		rows := []ModelMessage{{Role: "user", Text: userLine, MediaTokens: 422}}
		ctx := withRunProfile(context.Background(), &RunProfile{Vision: false, Provider: "deepseek"})
		annotateMediaTokens(ctx, rows, newFoldBox("s1", "r1"), resolve)
		if rows[0].MediaTokens != 0 {
			t.Fatalf("不收图仍留下 %d", rows[0].MediaTokens)
		}
		if reads != before {
			t.Fatal("Vision=false 不应读盘")
		}
	})

	t.Run("别的厂商全 0", func(t *testing.T) {
		before := reads
		rows := []ModelMessage{{Role: "user", Text: userLine, MediaTokens: 99}}
		ctx := withRunProfile(context.Background(), &RunProfile{Vision: true, Provider: "openai"})
		annotateMediaTokens(ctx, rows, newFoldBox("s1", "r1"), resolve)
		if rows[0].MediaTokens != 0 || reads != before {
			t.Fatalf("非 deepseek 不应计价或读盘: tokens=%d reads=%d", rows[0].MediaTokens, reads-before)
		}
	})

	t.Run("没有 profile 清掉旧价", func(t *testing.T) {
		rows := []ModelMessage{{Role: "user", Text: userLine, MediaTokens: 422}}
		annotateMediaTokens(context.Background(), rows, newFoldBox("s1", "r1"), resolve)
		if rows[0].MediaTokens != 0 {
			t.Fatalf("无 profile = %d, want 0", rows[0].MediaTokens)
		}
	})
}

func TestMediaSendLogTokens(t *testing.T) {
	raw := testPNG(t, 800, 800)
	root := t.TempDir()
	name := "a3-red-800.png"
	if err := os.WriteFile(filepath.Join(root, name), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	line := mediaPlaceholder(name, 1, name, "image/png", len(raw), shaOf(raw))
	ctx := withRunProfile(withFoldBox(context.Background(), newFoldBox("s1", "r1")), &RunProfile{Vision: true, Provider: "deepseek"})
	buf := captureLog(t)
	m := &mediaSendModel{workspace: root}
	plain := m.restore(ctx, []*schema.Message{schema.UserMessage("只是文字")})
	if len(plain) != 1 || plain[0].Content != "只是文字" {
		t.Fatalf("纯文本被改写: %+v", plain[0])
	}
	if strings.Contains(buf.String(), "media send") || strings.Contains(buf.String(), "img=") {
		t.Fatalf("纯文本多打了日志: %q", buf.String())
	}
	out := m.restore(ctx, []*schema.Message{schema.UserMessage("颜色\n" + line)})
	if !strings.Contains(buf.String(), "media send s1/r1: user=1 tool=0 skipped=0 img=422tok") {
		t.Fatalf("日志 = %q", buf.String())
	}
	if len(out) != 1 || len(out[0].UserInputMultiContent) != 2 {
		t.Fatalf("应还原出一张图: %+v", out[0])
	}
}
