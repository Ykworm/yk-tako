package runengine

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestBashTimeoutMessage(t *testing.T) {
	if !sandboxAvailable() {
		t.Skip("这台机器的 sandbox-exec 不能 apply，超时用例无法证明沙箱路径")
	}
	out, err := runBash(context.Background(), t.TempDir(), "sleep 30", time.Second, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "bash 超时") {
		t.Fatalf("超时应回到工具结果，得到 %q", out)
	}
	if strings.Contains(out, "退出码：") {
		t.Fatalf("超时不应伪装成普通退出码，得到 %q", out)
	}
}
