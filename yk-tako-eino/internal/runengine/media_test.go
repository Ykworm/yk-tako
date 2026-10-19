// media_test.go 是 doc/32 A1/A4/A6 的钉子网：入口图校验与内容寻址落盘（正/负/幂等）、
// 占位行字节面（含 A6 的 name/seq 通信字段与 sanitize）、取图端点三路
// （200 逐字节 / 越界 400 / 缺失 404）。
// postRuns 整链、seq 跨消息递增与「重发同图盘上仍一份」的活体在台账拍（curl + 磁盘对字节）。
// A2 翻案（2026-10-18）：图不受 token 准入线约束，本文件（≤5MB/≤4 张/白名单）就是图唯一的门；
// 「不折算」由 AdmitText 无 blobs 参数在编译层钉死，不复测。
package runengine

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func pngB64(t *testing.T, b byte) string {
	t.Helper()
	return base64.StdEncoding.EncodeToString([]byte{0x89, 0x50, 0x4E, 0x47, b, b, b, b})
}

func TestParseSessionMediaPath(t *testing.T) {
	sid, rel, ok := parseSessionMediaPath("sessions/vis/../priv/secret.png")
	if !ok || sid != "priv" || rel != "secret.png" {
		t.Fatalf("Clean 后应落到 priv 会话，得到 sid=%q rel=%q ok=%v", sid, rel, ok)
	}
	if _, _, ok := parseSessionMediaPath(".media/abc.png"); ok {
		t.Fatal("不带会话的 .media 路径不能跳过访问检查")
	}
	if _, _, ok := parseSessionMediaPath("sessions/s1/../../etc/passwd"); ok {
		t.Fatal("清出 sessions 前缀的路径必须拒绝")
	}
}

func TestIngestImagesHappy(t *testing.T) {
	root := t.TempDir()
	out, err := ingestImages(root, []postImage{{MIME: "image/png", DataBase64: pngB64(t, 1)}}, 1)
	if err != nil {
		t.Fatalf("合法图应过: %v", err)
	}
	raw, _ := base64.StdEncoding.DecodeString(pngB64(t, 1))
	sum := sha256.Sum256(raw)
	shaHex := hex.EncodeToString(sum[:])
	want := mediaPlaceholder("", 1, ".media/"+shaHex[:16]+".png", "image/png", len(raw), shaHex)
	if out != want {
		t.Fatalf("占位行字节面:\n got=%q\nwant=%q", out, want)
	}
	if _, err := os.Stat(filepath.Join(root, ".media", shaHex[:16]+".png")); err != nil {
		t.Fatalf("落盘文件应存在: %v", err)
	}
}

// 内容寻址幂等：同字节再收一次（同 sha 同名 O_EXCL 撞上）不报错、盘上仍一份。
func TestIngestImagesDedupe(t *testing.T) {
	root := t.TempDir()
	img := postImage{MIME: "image/png", DataBase64: pngB64(t, 2)}
	for i := 0; i < 2; i++ {
		if _, err := ingestImages(root, []postImage{img}, 1); err != nil {
			t.Fatalf("第 %d 次收图应幂等: %v", i+1, err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(root, ".media"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("重发同图盘上应恰一份: n=%d err=%v", len(entries), err)
	}
}

func TestIngestImagesRejects(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		name string
		imgs []postImage
		want string
	}{
		{"白名单外", []postImage{{MIME: "image/heic", DataBase64: pngB64(t, 3)}}, "不在白名单"},
		{"base64 坏", []postImage{{MIME: "image/png", DataBase64: "!!!"}}, "base64 解不开"},
		{"空内容", []postImage{{MIME: "image/png", DataBase64: ""}}, "内容为空"},
		{"超单文件帽", []postImage{{MIME: "image/png", DataBase64: base64.StdEncoding.EncodeToString(make([]byte, mediaMaxBytes+1))}}, "上限"},
	}
	for _, tc := range cases {
		_, err := ingestImages(root, tc.imgs, 1)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: want err 含 %q, got %v", tc.name, tc.want, err)
		}
	}
	// 超 4 张整批拒收
	many := make([]postImage, mediaMaxPerMsg+1)
	for i := range many {
		many[i] = postImage{MIME: "image/png", DataBase64: pngB64(t, byte(i+4))}
	}
	if _, err := ingestImages(root, many, 1); err == nil || !strings.Contains(err.Error(), "最多") {
		t.Fatalf("超张数应拒: %v", err)
	}
}

func TestIngestImagesEmpty(t *testing.T) {
	out, err := ingestImages(t.TempDir(), nil, 1)
	if err != nil || out != "" {
		t.Fatalf("无图应空串直通: out=%q err=%v", out, err)
	}
}

// A6：seq 从调用方给的起点连续编、name 经 sanitize 进引号列、无名省略 name= 列。
func TestIngestImagesNameSeq(t *testing.T) {
	root := t.TempDir()
	out, err := ingestImages(root, []postImage{
		{Name: "photo.png", MIME: "image/png", DataBase64: pngB64(t, 4)},
		{MIME: "image/png", DataBase64: pngB64(t, 5)},
	}, 3)
	if err != nil {
		t.Fatalf("双图应过: %v", err)
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 2 {
		t.Fatalf("应两行占位: %q", out)
	}
	if !strings.HasPrefix(lines[0], `[media seq=3 name="photo.png" path=.media/`) {
		t.Fatalf("带名占位字节面: %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "[media seq=4 path=.media/") {
		t.Fatalf("无名占位应省 name= 列: %q", lines[1])
	}
}

func TestMediaDisplayName(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"photo.png", "photo.png"},
		{"Screenshot 2026.png", "Screenshot 2026.png"}, // 空格保留（引号包裹下不破坏分字段）
		{"a\nb\rc.png", "abc.png"},                     // 换行/回车剥掉（占位行必须单行）
		{`he said "hi".png`, "he said hi.png"},         // 引号剥掉（name 值引号包裹）
		{"  padded  ", "padded"},
		{"", ""},
		{"\t\n", ""},
	}
	for _, tc := range cases {
		if got := mediaDisplayName(tc.raw); got != tc.want {
			t.Fatalf("mediaDisplayName(%q)=%q want %q", tc.raw, got, tc.want)
		}
	}
	long := strings.Repeat("长", 150)
	if got := mediaDisplayName(long); got != string([]rune(long)[:mediaNameMaxRunes]) {
		t.Fatalf("超长应截 %d 字符", mediaNameMaxRunes)
	}
}

// A2 翻案后图与 token 准入线彻底解耦：本组硬帽（白名单/张数/单文件字节）就是图唯一的门。
// 旧 TestAdmitTextFoldsImages（base64 折进 cl100k 预算）随翻案删除——「不折算」由
// AdmitText 签名去 blobs 在编译层钉死。窗口关闭放行、边界两侧的文本行为归 compact_test.go
// 的 TestAdmitTextBoundary。
func TestGetMediaEndpoint(t *testing.T) {
	root := t.TempDir()
	raw := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A}
	if err := os.MkdirAll(filepath.Join(root, ".media"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".media", "abc123.png"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Server{mediaRoot: root}
	gin.SetMode(gin.TestMode)

	call := func(url string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, url, nil)
		s.getMedia(c)
		return w
	}

	if w := call("/api/media?path=.media/abc123.png"); w.Code != http.StatusOK || w.Body.String() != string(raw) || w.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("金路径应逐字节回流: code=%d body=%q mime=%q", w.Code, w.Body.String(), w.Header().Get("Content-Type"))
	}
	if w := call("/api/media?path=../.env"); w.Code != http.StatusBadRequest {
		t.Fatalf("越界应 400: code=%d", w.Code)
	}
	if w := call("/api/media?path=.media/absent.png"); w.Code != http.StatusNotFound {
		t.Fatalf("缺失应 404: code=%d", w.Code)
	}
	if w := call("/api/media"); w.Code != http.StatusBadRequest {
		t.Fatalf("缺 path 应 400: code=%d", w.Code)
	}
}
