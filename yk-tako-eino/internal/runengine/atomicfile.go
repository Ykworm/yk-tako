package runengine

// 本文件是「同目录临时文件 + 原子 rename」的短写入助手（方案真源 = doc/71 §4.3；
// 施工包 §9.1 的「统一短写入助手」）。write_file / edit_file / move_file 之后的
// Markdown 记忆文件共用它——**写入落盘只有这一条路**，别处再出现 os.WriteFile 直写目标
// 就是把「读者看见半截文件」这件事又请回来。
//
// 它保证什么、不保证什么（边界必须写在脸上，否则会被当成万能）：
//
//	保证：读者只会打开**完整旧版**或**完整新版**。不会读到空文件、半截、混合版本。
//	不保证：不防「最后写者覆盖」。两个写者各自在锁外算好内容，再先后 rename，
//	      后一个照样盖掉前一个（doc/71 §4.1）。防那个要靠两样别的东西：
//	      写者互斥（filelock.go 的 PG advisory lock）与写入依据校验
//	      （write_file 的读取基准 filefp.go 的 fileBaseline）。
//
//	fsync 服务的是「崩溃后不丢」，**不是**原子可见性的来源——原子可见性来自 rename 本身。
//	同一个 rename 要能原子，前提是临时文件与目标**同一个文件系统**：跨文件系统的 rename
//	会退化成「复制 + 删除」（中间态暴露、还可能半途失败留下半个文件），所以临时文件
//	必须建在目标的同目录。
//
// 失败语义（硬要求）：**任何失败路径，目标文件的字节不变，临时文件被清掉**。
// 目标目录里不留垃圾，也不留「看起来像目标」的东西（临时文件带 .tako-tmp- 前缀，
// 与目标不同名；崩溃残留的临时文件也不会被任何读路径当成目标）。

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
)

// atomicTempPattern 是临时文件名模板：建在目标同目录、名字唯一、自带识别性。
//
// 「唯一」用 os.CreateTemp 的随机后缀（O_EXCL 语义）→ 两个并发写者不会撞名，
// 撞名的后果是其中一个把别人的临时文件 rename 成目标（内容错乱）。
const atomicTempPattern = ".tako-tmp-*"

// atomicTempHook 是**测试用的观察点**（生产恒为 nil）：临时文件建好时回调一次
// （目录 + 文件名），让测试能断言「临时文件确实建在目标的同目录」。
// 不设它就没有任何额外开销（一次 nil 判断）。
var atomicTempHook func(dir, name string)

// atomicWriteFile 把 content 原子写到 path，返回 nil 表示内容已经替换到位。
//
// 步骤与每步的失败后果：
//
//  1. 在目标的**同目录**建唯一临时文件（同文件系统 → rename 才是原子替换）；
//  2. 写完整内容 → Sync → Close（Sync 服务崩溃持久性：写完不 fsync，崩溃后可能留下空文件）；
//  3. 设权限：**已有文件保留原权限**（覆盖写不该顺手改权限），新文件用 perm（调用方给 0644）；
//  4. rename 覆盖目标（同目录 → 原子替换，读者只见完整旧版或完整新版）；
//  5. Sync 父目录（让「这次替换」在崩溃后也留得住）。
//
// 第 1–3 步失败 → 目标字节不变（还没碰过它），临时文件被删。
// 第 4 步失败 → 目标字节不变（rename 是原子的：要么成了，要么没成），临时文件被删。
// 第 5 步失败 → 内容**已经**换掉了，只记日志不改判（见 syncDir 的注释）。
func atomicWriteFile(path string, content []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, atomicTempPattern)
	if err != nil {
		return fmt.Errorf("建临时文件失败：%w", err)
	}
	tmpName := tmp.Name()
	if atomicTempHook != nil {
		atomicTempHook(dir, tmpName)
	}
	// 提前返回的每一条路都要收掉临时文件：目标目录里不留垃圾。
	cleanup := func() { _ = os.Remove(tmpName) }

	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("写临时文件失败：%w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("同步临时文件失败：%w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("关闭临时文件失败：%w", err)
	}
	// 权限：目标已存在 → 沿用它原来的（覆盖写不该顺手改权限）；不存在 → 用调用方给的值。
	if fi, err := os.Stat(path); err == nil {
		perm = fi.Mode().Perm()
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		cleanup()
		return fmt.Errorf("设置权限失败：%w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("替换目标失败：%w", err)
	}
	// rename 之后同步父目录：让这次替换在崩溃后也留得住。
	syncDir(dir)
	return nil
}

// syncDir 同步目录项，让 rename 本身落盘（崩溃持久性）。
//
// 失败只记日志、**不改判成「写失败」**：走到这一步时字节已经换掉了，
// 报「写入失败」会让模型以为没写而重写一遍——那是拿一个假失败换一个真重复写。
// 有些平台/文件系统不支持对目录 fsync（EINVAL / ENOTSUP），那不是写失败。
func syncDir(dir string) {
	f, err := os.Open(dir)
	if err != nil {
		log.Printf("atomicfile: 打开目录 %s 失败：%v", dir, err)
		return
	}
	defer f.Close()
	if err := f.Sync(); err != nil {
		log.Printf("atomicfile: 同步目录 %s 失败：%v", dir, err)
	}
}
