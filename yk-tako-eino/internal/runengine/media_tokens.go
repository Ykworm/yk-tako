// 本文件是 doc/32 A3-5 的图片 token 计数。
// 计算器是 DeepSeek 公开公式 v41 的逐句移植（dsh image-tokens.ts）：
// 14px patch、3:1 下采样、总像素不到 544×544 先放大、单图最多 1024、
// 网格 rows×(cols+1)+2，没有对齐 pad，也没有宽高比钳制。
// 输入是原图像素。tako 发原图，provider 按它收到的尺寸自己投影；这里不移植发送侧缩小。
// 解不出预算内的网格，或 10 轮还没到不动点，返回 0，不另造一个数。
package runengine

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"os"

	_ "golang.org/x/image/webp"
)

const (
	deepSeekPatchSize       = 14
	deepSeekDownsampleRatio = 3
	deepSeekMaxImageTokens  = 1024
	deepSeekMinPixels       = 544 * 544
	deepSeekCellSize        = deepSeekPatchSize * deepSeekDownsampleRatio
)

type gridResize struct {
	gridHeight int
	gridWidth  int
	bestHeight int
	bestWidth  int
	numTokens  int
}

func intDiv(value, divisor int) int {
	return value / divisor
}

func ceilDiv(value, divisor int) int {
	return (value + divisor - 1) / divisor
}

func gridTokens(gridHeight, gridWidth int) int {
	return gridHeight*(gridWidth+1) + 2
}

func gridCells(paddedLength int) int {
	return ceilDiv(intDiv(paddedLength, deepSeekPatchSize), deepSeekDownsampleRatio)
}

func solveResizeRatio(height, width, budget int) gridResize {
	aspect := float64(height) / float64(width)
	idealGridWidth := math.Sqrt((float64(budget)-2)/aspect+0.25) - 0.5
	idealGridHeight := idealGridWidth * aspect
	var bestHeight, bestWidth int
	if idealGridWidth < 1 {
		solvedGridWidth := 1
		solvedGridHeight := intDiv(budget-2, solvedGridWidth+1)
		bestWidth = solvedGridWidth * deepSeekCellSize
		bestHeight = solvedGridHeight * deepSeekCellSize
	} else if idealGridHeight < 1 {
		solvedGridHeight := 1
		solvedGridWidth := intDiv(budget-2, solvedGridHeight) - 1
		bestWidth = solvedGridWidth * deepSeekCellSize
		bestHeight = solvedGridHeight * deepSeekCellSize
	} else {
		solvedGridWidth := int(math.Trunc(idealGridWidth))
		solvedGridHeight := int(math.Trunc(idealGridHeight))
		scale := math.Min(
			float64(solvedGridWidth*deepSeekCellSize)/float64(width),
			float64(solvedGridHeight*deepSeekCellSize)/float64(height),
		)
		bestWidth = int(math.Trunc(float64(width)*scale/float64(deepSeekPatchSize))) * deepSeekPatchSize
		bestHeight = int(math.Trunc(float64(height)*scale/float64(deepSeekPatchSize))) * deepSeekPatchSize
	}
	gh := gridCells(bestHeight)
	gw := gridCells(bestWidth)
	return gridResize{
		gridHeight: gh,
		gridWidth:  gw,
		bestHeight: bestHeight,
		bestWidth:  bestWidth,
		numTokens:  gridTokens(gh, gw),
	}
}

func safeResize(height, width, paddedHeight, paddedWidth int) (gridResize, bool) {
	gh := gridCells(paddedHeight)
	gw := gridCells(paddedWidth)
	direct := gridResize{
		gridHeight: gh,
		gridWidth:  gw,
		bestHeight: paddedHeight,
		bestWidth:  paddedWidth,
		numTokens:  gridTokens(gh, gw),
	}
	if direct.numTokens <= deepSeekMaxImageTokens {
		return direct, true
	}
	solved := solveResizeRatio(height, width, deepSeekMaxImageTokens)
	if solved.numTokens > deepSeekMaxImageTokens {
		return gridResize{}, false
	}
	return solved, true
}

func resizeOnce(width, height int) (gridResize, bool) {
	scaledWidth := width
	scaledHeight := height
	pixels := int64(scaledWidth) * int64(scaledHeight)
	if pixels < int64(deepSeekMinPixels) && pixels > 0 {
		scale := math.Sqrt(float64(deepSeekMinPixels) / float64(pixels))
		scaledWidth = int(math.Trunc(float64(scaledWidth) * scale))
		scaledHeight = int(math.Trunc(float64(scaledHeight) * scale))
	}
	paddedWidth := ceilDiv(scaledWidth, deepSeekPatchSize) * deepSeekPatchSize
	paddedHeight := ceilDiv(scaledHeight, deepSeekPatchSize) * deepSeekPatchSize
	return safeResize(scaledHeight, scaledWidth, paddedHeight, paddedWidth)
}

func sameResize(a, b gridResize) bool {
	return a.gridHeight == b.gridHeight &&
		a.gridWidth == b.gridWidth &&
		a.bestHeight == b.bestHeight &&
		a.bestWidth == b.bestWidth &&
		a.numTokens == b.numTokens
}

func deepSeekImageTokens(width, height int) int {
	if width <= 0 || height <= 0 {
		return 0
	}
	result, ok := resizeOnce(width, height)
	if !ok {
		return 0
	}
	for iteration := 1; iteration < 10; iteration++ {
		next, nextOK := resizeOnce(result.bestWidth, result.bestHeight)
		if !nextOK {
			return 0
		}
		if sameResize(next, result) {
			return result.numTokens
		}
		result = next
	}
	return 0
}

// imageDimensions 只读文件头。png/jpeg/gif 走标准库，webp 走 golang.org/x/image/webp。
func imageDimensions(path string) (int, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, 0, err
	}
	return cfg.Width, cfg.Height, nil
}

func imageTokensFromBytes(raw []byte) (int, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return 0, err
	}
	return deepSeekImageTokens(cfg.Width, cfg.Height), nil
}

// mediaRefTokens 先走与发送侧同一套校验，再按原图宽高计价。
// 不是收图的 deepseek 模型时返回 0：别的 vision 模型没有计算器，不算。
func mediaRefTokens(ctx context.Context, resolve func(context.Context, string) (string, error), ref mediaRef) (int, error) {
	p := profileFrom(ctx)
	if p == nil || !p.Vision || p.Provider != "deepseek" {
		return 0, nil
	}
	if resolve == nil {
		return 0, fmt.Errorf("no media resolver")
	}
	raw, reason := loadMediaBytes(ctx, resolve, ref)
	if reason != "" {
		return 0, fmt.Errorf("%s", reason)
	}
	n, err := imageTokensFromBytes(raw)
	if err != nil {
		return 0, err
	}
	if n <= 0 {
		return 0, fmt.Errorf("deepseek image tokens: no price for %s", ref.path)
	}
	return n, nil
}
