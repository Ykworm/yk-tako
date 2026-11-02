package runengine

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// sandboxDenied 是跨后端统一的「围栏拒绝」判据：
//   - seatbelt：Operation not permitted
//   - bwrap 未挂载的路径：No such file or directory
//   - bwrap 只读挂载的写：Read-only file system / Permission denied
//
// 三句都算越界被挡。测试里的目标路径在宿主上真实存在，所以 ENOENT 不可能是「路径写错了」。
func sandboxDenied(out string) bool {
	for _, s := range []string{"not permitted", "No such file or directory", "Read-only file system", "Permission denied"} {
		if strings.Contains(out, s) {
			return true
		}
	}
	return false
}

// TestBwrapArgsGolden 钉住挂载表：新根里只有 /usr（只读）、白名单 /etc（只读）、
// 可写的工作区根与本场技能/工具根（只读），网络命名空间**不**卸。
func TestBwrapArgsGolden(t *testing.T) {
	root := t.TempDir()
	extra := t.TempDir()
	inside := filepath.Join(root, "inside")
	if err := os.WriteFile(inside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	realRoot := sandboxRealPath(root)
	realExtra := sandboxRealPath(extra)
	realInside := sandboxRealPath(inside)
	exists := func(p string) bool {
		switch p {
		case "/opt", "/etc/resolv.conf", realExtra, realInside:
			return true
		}
		return false
	}
	got := bwrapArgs(root, []string{inside, extra}, []string{"/bin/bash", "-c", "echo hi"}, exists)
	want := []string{
		"--unshare-user", "--unshare-pid", "--unshare-ipc", "--unshare-uts", "--unshare-cgroup",
		"--die-with-parent", "--new-session",
		"--proc", "/proc",
		"--dev", "/dev",
		"--tmpfs", "/tmp",
		"--ro-bind", "/usr", "/usr",
		"--symlink", "usr/bin", "/bin",
		"--symlink", "usr/sbin", "/sbin",
		"--symlink", "usr/lib", "/lib",
		"--symlink", "usr/lib64", "/lib64",
		"--ro-bind", "/etc/resolv.conf", "/etc/resolv.conf",
		"--ro-bind", "/opt", "/opt",
		"--ro-bind", realExtra, realExtra,
		"--bind", realRoot, realRoot,
		"--chdir", realRoot,
		"--", "/bin/bash", "-c", "echo hi",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bwrap 挂载表与金样不一致\n got: %q\nwant: %q", got, want)
	}
	for i := 0; i+1 < len(got); i++ {
		if got[i] == "--ro-bind" && got[i+1] == "/" {
			t.Fatal("绝不能 --ro-bind / /：那会把 .env、home、别的租户一起只读暴露")
		}
	}
	for _, a := range got {
		if a == "--unshare-net" || a == "--unshare-all" {
			t.Fatalf("网络必须保留（D37 出站开），得到 %q", a)
		}
	}
}

// TestBwrapProbe：探针不能只看 binary 在不在——真跑一次才能区分 userns 被禁 / seccomp 挡住。
func TestBwrapProbe(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "bwrap-good")
	if err := os.WriteFile(good, []byte("#!/bin/sh\necho tako-bash-probe\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if ok, reason := probeBwrap(good); !ok {
		t.Fatalf("能输出探针串的 bwrap 应判为可用，得到 %q", reason)
	}

	bad := filepath.Join(dir, "bwrap-bad")
	if err := os.WriteFile(bad, []byte("#!/bin/sh\necho userns disabled >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ok, reason := probeBwrap(bad)
	if ok {
		t.Fatal("探针失败的 bwrap 不能判为可用")
	}
	if !strings.Contains(reason, "userns disabled") {
		t.Fatalf("失败原因要保留探针输出，得到 %q", reason)
	}
}
