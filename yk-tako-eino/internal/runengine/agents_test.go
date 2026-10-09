package runengine

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cloudwego/eino/components/tool"
)

// agents_test.go 钉装载面最容易悄悄坏的两处：frontmatter 的 tools 三态（缺席/[]/名单——靠 *[]string
// 指针区分，yaml.v3 对 `tools: []` 必须给出非 nil 空切片）与逐条拒载语义（坏文件不拖垮其余）。

func writeSpec(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadAgentSpecs(t *testing.T) {
	dir := t.TempDir()
	writeSpec(t, dir, "a_good.md", "---\nname: alpha\ndescription: 甲\ntools: [get_time]\n---\nAAA\n")
	writeSpec(t, dir, "b_nodesc.md", "---\nname: beta\n---\nB\n")
	writeSpec(t, dir, "c_dup.md", "---\nname: alpha\ndescription: 撞名\n---\nC\n")
	writeSpec(t, dir, "d_model.md", "---\nname: delta\ndescription: 丁\nmodel: gpt-x\n---\nD\n")
	writeSpec(t, dir, "e_empty.md", "---\nname: eps\ndescription: 空集\ntools: []\n---\nE\n")
	writeSpec(t, dir, "f_absent.md", "---\nname: phi\ndescription: 缺席\n---\nF\n")
	writeSpec(t, dir, "g_nofront.md", "没有 frontmatter\n")
	writeSpec(t, dir, "h_typo.md", "---\nname: eta\ndescription: 拼错\ntool: [get_time]\n---\nH\n")

	specs, err := LoadAgentSpecs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 3 {
		t.Fatalf("应剩 3 条（alpha/eps/phi），实得 %d: %+v", len(specs), specs)
	}
	if specs[0].Name != "alpha" || specs[0].Prompt != "AAA" {
		t.Fatalf("首条应为 alpha/AAA: %+v", specs[0])
	}
	if specs[0].Tools == nil || len(*specs[0].Tools) != 1 || (*specs[0].Tools)[0] != "get_time" {
		t.Fatalf("alpha 应有名单 [get_time]: %+v", specs[0].Tools)
	}
	if specs[1].Name != "eps" || specs[1].Tools == nil || len(*specs[1].Tools) != 0 {
		t.Fatalf("eps 应为非 nil 空集: name=%s tools=%v", specs[1].Name, specs[1].Tools)
	}
	if specs[2].Name != "phi" || specs[2].Tools != nil {
		t.Fatalf("phi 应缺席（nil 指针 = 继承）: name=%s tools=%v", specs[2].Name, specs[2].Tools)
	}

	missing, err := LoadAgentSpecs(filepath.Join(dir, "不存在"))
	if err != nil || missing != nil {
		t.Fatalf("目录不存在应为 (nil, nil)：%v %v", missing, err)
	}
}

func TestResolveToolPool(t *testing.T) {
	ctx := context.Background()
	gt, err := newGetTimeTool()
	if err != nil {
		t.Fatal(err)
	}
	base := make([]tool.BaseTool, 1, 4)
	base[0] = gt

	inherit, err := resolveToolPool(ctx, AgentSpec{Name: "i"}, base)
	if err != nil || len(inherit) != 1 || inherit[0] != gt {
		t.Fatalf("缺席应继承父池单工具: %v %v", inherit, err)
	}
	if reflect.ValueOf(inherit).Pointer() == reflect.ValueOf(base).Pointer() {
		t.Fatal("继承必须拷贝切片——装载 handler 会就地 append 委派工具，共享底层数组即串扰")
	}

	empty := []string{}
	if pool, err := resolveToolPool(ctx, AgentSpec{Tools: &empty}, base); err != nil || len(pool) != 0 {
		t.Fatalf("空集档应为 len 0: %v %v", pool, err)
	}

	list := []string{"get_time"}
	if pool, err := resolveToolPool(ctx, AgentSpec{Tools: &list}, base); err != nil || len(pool) != 1 || pool[0] != gt {
		t.Fatalf("名单应映射到 get_time: %v %v", pool, err)
	}

	unknown := []string{"nope"}
	if _, err := resolveToolPool(ctx, AgentSpec{Tools: &unknown}, base); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("未知名应拒载并报出名字，实得: %v", err)
	}
}
