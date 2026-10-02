// 本文件：窗口大小的三级读取，以及 /models JSON 的解析。解析不访问网络。
package compact

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestDefaultPathIsCwdFile 钉住：默认文件在进程当前目录，不在家目录。
func TestDefaultPathIsCwdFile(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	got := DefaultPath()
	want := filepath.Join(cwd, "model-windows.json")
	if got != want {
		t.Fatalf("DefaultPath = %s，want %s", got, want)
	}
}

// TestWindowStoreMemoryThenFile 钉住：查到的正数进内存和文件；下一次新进程只读文件，不再调用 lookup。
func TestWindowStoreMemoryThenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "model-windows.json")
	store := NewWindowStore(path)
	calls := 0
	lookup := func(context.Context, string) (int, error) {
		calls++
		return 128000, nil
	}
	if got := store.Get(context.Background(), "deepseek/deepseek-v4-flash", lookup); got != 128000 {
		t.Fatalf("第一次 = %d", got)
	}
	if got := store.Get(context.Background(), "deepseek/deepseek-v4-flash", lookup); got != 128000 || calls != 1 {
		t.Fatalf("第二次 = %d，lookup %d 次", got, calls)
	}
	again := NewWindowStore(path)
	if got := again.Get(context.Background(), "deepseek/deepseek-v4-flash", func(context.Context, string) (int, error) {
		t.Fatal("文件里已经有这个模型，不应再查")
		return 0, nil
	}); got != 128000 {
		t.Fatalf("从文件读到 %d", got)
	}
}

// TestWindowStoreFallbackIsNotWritten 钉住：lookup 失败时用 256000，并且不写文件。
func TestWindowStoreFallbackIsNotWritten(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "model-windows.json")
	store := NewWindowStore(path)
	got := store.Get(context.Background(), "p/m", func(context.Context, string) (int, error) {
		return 0, os.ErrClosed
	})
	if got != FallbackWindow {
		t.Fatalf("失败时 = %d，want %d", got, FallbackWindow)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("失败不应写文件，stat err = %v", err)
	}
	got = store.Get(context.Background(), "p/m", func(context.Context, string) (int, error) {
		return -1, nil
	})
	if got != FallbackWindow {
		t.Fatalf("负数 = %d，want %d", got, FallbackWindow)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("负数不应写文件，stat err = %v", err)
	}
}

// TestWindowFromModelsJSONShapes 钉住：data、models、单个对象、数字字符串都能读到第一个大于 0 的字段。
func TestWindowFromModelsJSONShapes(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		modelID string
		want    int
	}{
		{"data", `{"data":[{"id":"other","context_window":1},{"id":"glm-5","context_window":128000}]}`, "glm-5", 128000},
		{"models", `{"models":[{"model":"kimi-k2.5","context_length":"131072"}]}`, "kimi-k2.5", 131072},
		{"object", `{"name":"x","max_model_len":1000}`, "x", 1000},
		{"skip zero", `[{"id":"m","context_window":0,"max_context_length":4096}]`, "m", 4096},
		{"order", `[{"id":"m","context_window":7,"max_input_tokens":9}]`, "m", 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := WindowFromModelsJSON([]byte(tc.raw), tc.modelID)
			if err != nil || got != tc.want {
				t.Fatalf("got %d err %v，want %d", got, err, tc.want)
			}
		})
	}
	if _, err := WindowFromModelsJSON([]byte(`{"data":[{"id":"m"}]}`), "missing"); err == nil {
		t.Fatal("没有这一项时应失败")
	}
}

// TestHTTPLookupReadsModels 钉住：地址已有 /v1 时补上 /models，密钥放在 Authorization 里。
func TestHTTPLookupReadsModels(t *testing.T) {
	var gotAuth string
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"data":[{"id":"m","context_window":1000}]}`))
	}))
	defer srv.Close()
	n, err := HTTPLookup(srv.URL+"/v1", "secret", "m")(context.Background(), "p/m")
	if err != nil || n != 1000 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if gotPath != "/v1/models" {
		t.Fatalf("path = %s", gotPath)
	}
	if gotAuth != "Bearer secret" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
}

// TestModelsURLDoesNotDoubleAppend 钉住：BaseURL 已经以 /models 结尾时不再加一段。
func TestModelsURLDoesNotDoubleAppend(t *testing.T) {
	if got := modelsURL("https://example.test/v1/models/"); got != "https://example.test/v1/models" {
		t.Fatalf("modelsURL = %s", got)
	}
}
