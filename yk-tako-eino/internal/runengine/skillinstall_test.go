// skillinstall_test.go 钉住解包器对真实归档的宽容与边界（doc/52 §6.5 C2）：
// PAX 头是元数据不是文件；仓库归档里的软链跳过，上传包里的软链拒绝。
package runengine

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func buildTarGz(t *testing.T, entries func(*tar.Writer)) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	entries(tw)
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractTarGzPaxAndSymlink(t *testing.T) {
	data := buildTarGz(t, func(tw *tar.Writer) {
		// GitHub 的 codeload tarball 第一条就是 pax 全局头。
		if err := tw.WriteHeader(&tar.Header{Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader}); err != nil {
			t.Fatal(err)
		}
		if err := tw.WriteHeader(&tar.Header{Name: "pkg/skill/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
			t.Fatal(err)
		}
		body := "---\nname: demo\ndescription: 演示\n---\n正文\n"
		if err := tw.WriteHeader(&tar.Header{Name: "pkg/skill/SKILL.md", Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
		if err := tw.WriteHeader(&tar.Header{Name: "pkg/skill/CLAUDE.md", Typeflag: tar.TypeSymlink, Linkname: "SKILL.md"}); err != nil {
			t.Fatal(err)
		}
	})

	// 仓库归档：跳过软链，正常解出 SKILL.md。
	repoDest := t.TempDir()
	if err := extractTarGzPackage(data, repoDest, skillRepoLimits); err != nil {
		t.Fatalf("仓库归档解包失败: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repoDest, "pkg/skill/SKILL.md")); err != nil {
		t.Errorf("SKILL.md 没解出来: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(repoDest, "pkg/skill/CLAUDE.md")); !os.IsNotExist(err) {
		t.Errorf("软链不应落进 staging（stat err=%v）", err)
	}

	// 上传包：同一个归档里的软链必须拒绝（116105）。
	uploadDest := t.TempDir()
	err := extractTarGzPackage(data, uploadDest, skillUploadLimits)
	var werr *skillWriteErr
	if !errors.As(err, &werr) || werr.code != codeSkillPackageBad {
		t.Errorf("上传包里的软链应回 116105，得到 %v", err)
	}
}

// 单个技能目录的预算复核：仓库归档整体可以很大，但一个技能不能超过 200 文件。
func TestCheckCandidateBudget(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "pkg", "big")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < skillPkgMaxFiles+1; i++ {
		name := filepath.Join(skillDir, "f"+itoa(i))
		if err := os.WriteFile(name, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cand := skillPkgCandidate{Name: "big", RelDir: "pkg/big"}
	err := checkCandidateBudget(dir, &cand)
	var werr *skillWriteErr
	if !errors.As(err, &werr) || werr.code != codeSkillPackageTooLarge {
		t.Fatalf("超文件数应回 116113，得到 %v", err)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [8]byte
	p := len(b)
	for i > 0 {
		p--
		b[p] = byte('0' + i%10)
		i /= 10
	}
	return string(b[p:])
}
