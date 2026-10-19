package runengine

// media.go — doc/32 A1 + A6：入口媒体的校验、内容寻址落盘与占位行渲染。
// 纪律（doc/32 第 2 节）：媒体字节永远不进对话的落库面。登录后的字节落在
// <workspace>/tenants/<tid>/sessions/<sid>/.media/，占位 path 是 sessions/<sid>/.media/...。
// 对话里只有一行占位文本。A4 的 GET /api/media 是 path 换字节的唯一出口。
// A6 通信字段（doc/32 §3）：占位行加 name=/seq=——name 是用户上传文件名（可省），
// seq 是全会话累计序号（恒在）；模型拿这对字段回应「第 N 张」「同名的新版」。

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// errStorageQuota 是租户存储帽用完。postRuns 把它收成 429，不建 run。
var errStorageQuota = errors.New("storage quota")

const (
	// mediaDirName 是 workspace 根下的媒体目录；占位行里的 path 相对 workspace 根，
	// read_media_file 与 GET /api/media 共用同一把牢笼（resolveInWorkspace）。
	mediaDirName = ".media"
	// mediaMaxBytes 是单文件帽：入口图（A1）与 read_media_file（filetools.go）同值引用。
	mediaMaxBytes = 5 << 20
	// mediaMaxPerMsg 是单条消息的图数帽（planning 设计决定 2）。
	mediaMaxPerMsg = 4
	// mediaNameMaxRunes 是 name 字段的截断长度（doc/32 §3 sanitize）：用户文件名超长
	// 只削显示面，不影响落盘。
	mediaNameMaxRunes = 120
)

// canonicalMediaExt 是入口媒体的白名单（键集）与 mime→扩展名映射（planning 设计决定 2）。
// 与 filetools 的 mediaMimeByExt 分开：这张表管「收什么」，那张管「读什么」；jpeg 收进来
// 统一落 .jpg（canonical），读侧两个扩展名都认。
var canonicalMediaExt = map[string]string{
	"image/png":       ".png",
	"image/jpeg":      ".jpg",
	"image/gif":       ".gif",
	"image/webp":      ".webp",
	"image/bmp":       ".bmp",
	"image/svg+xml":   ".svg",
	"application/pdf": ".pdf",
	"audio/mpeg":      ".mp3",
	"audio/wav":       ".wav",
	"video/mp4":       ".mp4",
}

// ingestImages 把图写进 root/.media，占位 path 相对这个 root。会话入口走 ingestImagesAt。
func ingestImages(root string, images []postImage, seqStart int) (string, error) {
	return ingestImagesAt(root, images, seqStart, "", nil)
}

// ingestImagesAt 校验并落盘一批入口图，返回拼好的占位行块（每图一行、按入参序；
// 无图返回空串）。任何一项不合法整批拒收（调用方回 400）。seqStart 是本消息首图的
// 全会话序号。relPrefix 拼在占位 path 前面（会话图用 sessions/<sid>/，
// GET /api/media 和 read_media_file 读同一条路径）。quota 在落盘前数本张字节；nil 表示不限额。
func ingestImagesAt(root string, images []postImage, seqStart int, relPrefix string, quota func(int) (func(), error)) (string, error) {
	if len(images) == 0 {
		return "", nil
	}
	if len(images) > mediaMaxPerMsg {
		return "", fmt.Errorf("images 最多 %d 张，收到 %d 张", mediaMaxPerMsg, len(images))
	}
	if err := os.MkdirAll(filepath.Join(root, mediaDirName), 0o755); err != nil {
		return "", fmt.Errorf("创建 %s 失败: %v", mediaDirName, err)
	}
	lines := make([]string, 0, len(images))
	for i, im := range images {
		raw, err := base64.StdEncoding.DecodeString(im.DataBase64)
		if err != nil {
			return "", fmt.Errorf("images[%d]: base64 解不开: %v", i, err)
		}
		ext, ok := canonicalMediaExt[im.MIME]
		if !ok {
			return "", fmt.Errorf("images[%d]: mime %q 不在白名单", i, im.MIME)
		}
		if len(raw) == 0 {
			return "", fmt.Errorf("images[%d]: 内容为空", i)
		}
		if len(raw) > mediaMaxBytes {
			return "", fmt.Errorf("images[%d]: %d 字节超过单文件 %d 上限", i, len(raw), mediaMaxBytes)
		}
		if quota != nil {
			release, err := quota(len(raw))
			if err != nil {
				return "", err
			}
			if release != nil {
				defer release()
			}
		}
		sum := sha256.Sum256(raw)
		shaHex := hex.EncodeToString(sum[:])
		rel := relPrefix + mediaDirName + "/" + shaHex[:16] + ext
		if err := writeFileOnce(filepath.Join(root, mediaDirName, shaHex[:16]+ext), raw); err != nil {
			return "", fmt.Errorf("images[%d]: 落盘失败: %v", i, err)
		}
		lines = append(lines, mediaPlaceholder(im.Name, seqStart+i, rel, im.MIME, len(raw), shaHex))
	}
	return strings.Join(lines, "\n"), nil
}

// writeFileOnce 内容寻址写：O_EXCL 创建，已存在（同 sha 同字节）即跳过——并发与重投幂等。
func writeFileOnce(path string, raw []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if os.IsExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(raw)
	return err
}

// mediaDisplayName 把用户上传文件名清成可安全放进占位行的显示串（A6，doc/32 §3
// sanitize）：去控制字符与换行（占位行必须单行）、去引号（name 值以引号包裹，
// 内嵌引号会截断解析）、截 120 字符；清完为空返回空串（占位行整个省略 name=）。
func mediaDisplayName(raw string) string {
	clean := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '"' {
			return -1
		}
		return r
	}, raw)
	clean = strings.TrimSpace(clean)
	runes := []rune(clean)
	if len(runes) > mediaNameMaxRunes {
		runes = runes[:mediaNameMaxRunes]
	}
	return string(runes)
}

// parseSessionMediaPath 把占位 path 收成「会话 id + 会话目录内的相对路径」。
// 先 Clean，再认 sessions/<sid>/。sessions/<可见>/../<别人> 会变成别人的 sid，
// 调用方按清完的 sid 做访问检查，不能按第一段放行。
// .media/... 这种不带会话的路径返回 false。
func parseSessionMediaPath(path string) (sid, rel string, ok bool) {
	slash := filepath.ToSlash(strings.TrimSpace(path))
	if slash == "" || strings.HasPrefix(slash, "/") {
		return "", "", false
	}
	cleaned := filepath.ToSlash(filepath.Clean(slash))
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") || strings.HasPrefix(cleaned, "/") {
		return "", "", false
	}
	parts := strings.Split(cleaned, "/")
	if len(parts) < 3 || parts[0] != "sessions" || parts[1] == "" || parts[1] == "." || parts[1] == ".." {
		return "", "", false
	}
	for _, part := range parts[2:] {
		if part == "" || part == "." || part == ".." {
			return "", "", false
		}
	}
	return parts[1], strings.Join(parts[2:], "/"), true
}

// mediaPlaceholder 是 doc/32 §3 的占位行真身（入口图用）。句尾指回 read_media_file 的
// 读者是**后续轮次**的模型——A3 的工具回执占位不含这句（读者就是刚调完的模型，指回
// 自己 = 死循环），两种占位永远别混。A6：name/seq 是通信字段——seq 恒在（全会话累计，
// 调用方计），name 可省（清完为空不写字段，不写字面空值）；name 值带引号包裹，
// 文件名里的空格不破坏按空格分字段的读法。
func mediaPlaceholder(rawName string, seq int, relPath, mime string, size int, shaHex string) string {
	name := mediaDisplayName(rawName)
	head := fmt.Sprintf("[media seq=%d", seq)
	if name != "" {
		head += fmt.Sprintf(" name=%q", name)
	}
	return fmt.Sprintf("%s path=%s mime=%s size=%dB sha=%s；要看就调 read_media_file 读这个路径]",
		head, relPath, mime, size, shaHex)
}
