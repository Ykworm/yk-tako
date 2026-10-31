// tempsweep.go 是 `.staging` / `.trash` 的过期清扫（doc/52 §4.6 第 4 条、§9-16）。
//
// 两个点目录都是「写面留下的半成品」：staging 是解包/上传的在途目录，trash 是
// rename 下来的旧版本。正常路径会在 defer 或下一次安装里收走它们，但进程崩在中途
// （或对话内安装的提议没人答）时它们会永远留着——每装一次就多一份磁盘字节。
//
// 清扫规则只有一条：**锁内、按 mtime、超过 24 小时就删**，唯一的豁免是
// 「本次操作正要 rename 走的那个 staging」（见 sweepTempDirsKeeping）。
//   - 锁内：扫盘与安装/删除/重扫排同一队（D27），不会删到另一个副本正在用的 staging；
//   - 启动时不扫盘：启动那一刻没锁，多副本下别的副本可能正在写 staging；
//   - 失败只记日志：清不掉是垃圾多留一天，不是这次安装失败的理由。
//
// 技能根与工具根共用本文件：两者 `.staging` / `.trash` 的形状相同
// （`<root>/.staging/<id>`、`<root>/.trash/<name>-<ts>`），规则没有理由分家。
package runengine

import (
	"log"
	"os"
	"path/filepath"
	"time"
)

// tempDirTTL 是点目录里一条垃圾的存活上限。24 小时不是为了「刚装一半」——
// 在途的写最多几分钟；留一天是给「装错了想手动捞回来」留窗口。
const tempDirTTL = 24 * time.Hour

// sweepTempDirs 删掉 root 下 `.staging` 与 `.trash` 里 mtime 超过 24 小时的条目。
//
// now 由调用方给（测试用假时钟）：生产调用点一律传 time.Now()。
// 点目录本身不在时什么都不做——那是「从没装过」，不是错误。
func sweepTempDirs(root string, now time.Time) {
	sweepTempDirsKeeping(root, now, "")
}

// sweepTempDirsKeeping 同上，但 `.staging/<keepStaging>` 无条件留着。
//
// keepStaging 是**本次操作正要 rename 走的那个 staging 目录名**。为什么需要它：
// 对话内安装的提议可以挂起很久（等人点头），挂起超过 24 小时的暂存目录按规则就是垃圾；
// 而批准那一下走的正是「锁内清扫 → 锁内 rename staging」这一段——
// 清扫把自己下一步要吃的目录删掉，rename 就 ENOENT，用户看到的是一句裸 I/O 错误
// （`安装失败：rename …: no such file or directory`）而不是设计好的「安装已过期」。
// 豁免不会攒垃圾：在途 staging 由调用方的 defer RemoveAll 收走。
//
// keepStaging 为空 = 没有豁免（删除、重扫、工具删除都不消费 staging）。
func sweepTempDirsKeeping(root string, now time.Time, keepStaging string) {
	for _, name := range []string{".staging", ".trash"} {
		dir := filepath.Join(root, name)
		entries, err := os.ReadDir(dir)
		if err != nil {
			// 目录不存在是常态；只有真的读不动（权限、io 错误）才值得记一行。
			if !os.IsNotExist(err) {
				log.Printf("temp-sweep %s: %v", dir, err)
			}
			continue
		}
		for _, e := range entries {
			if name == ".staging" && keepStaging != "" && e.Name() == keepStaging {
				continue
			}
			p := filepath.Join(dir, e.Name())
			info, err := e.Info()
			if err != nil {
				// 扫的过程中被别的进程删掉了（rename/RemoveAll）——正是我们要的结果。
				continue
			}
			if now.Sub(info.ModTime()) <= tempDirTTL {
				continue
			}
			if err := os.RemoveAll(p); err != nil {
				log.Printf("temp-sweep %s: %v", p, err)
			}
		}
	}
}

// stagingKeep 从一条 staging 全路径取出它的目录名，供 sweepTempDirsKeeping 豁免。
//
// 不是 `<root>/.staging/<id>` 形态的一律返回空——传进来的路径可能是别的根下的东西
// （配置写错、测试夹具），拿它的 Base 去豁免本根的同名条目就是无中生有的豁免。
func stagingKeep(root, staging string) string {
	if staging == "" {
		return ""
	}
	dir := filepath.Clean(filepath.Dir(staging))
	want := filepath.Clean(filepath.Join(root, ".staging"))
	same := dir == want
	if !same {
		// 提议里的 staging 是 EvalSymlinks 过的绝对路径，root 是拼出来的字面路径：
		// 工作区在符号链接下（macOS 的 /var → /private/var）时两者字面不同、指向同一处。
		resolved, err := filepath.EvalSymlinks(want)
		same = err == nil && dir == filepath.Clean(resolved)
	}
	if !same {
		return ""
	}
	return filepath.Base(staging)
}
