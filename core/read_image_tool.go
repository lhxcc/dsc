package core

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"os"
	"strings"

	_ "image/gif"  // 注册 gif 的 DecodeConfig 分发
	_ "image/jpeg" // 注册 jpeg 的 DecodeConfig 分发
	_ "image/png"  // 注册 png 的 DecodeConfig 分发

	"golang.org/x/image/webp"
)

// 模型主动读图的工具（宿主内置，只读）。
//
// 与输入框「@图片」的分工：@ 是**用户**在发言时把图带进对话；本工具是**模型**在
// 执行中途要看图（截图、图表、UI 稿、报错弹窗、扫描件）。两者共用同一条管线——
// 字节都经 core.SaveImageAttachment 落成 dsc-img://<sha256> 内容寻址引用，请求时
// 再由 core.ProjectImageRef 按路由策略缩放。所以 @ 能看到的图，工具读到的就是同一份，
// 不存在两套缓存或两种尺寸。
//
// 命名与语义对齐 DSH 的 read_image（工具名、file_path 参数、"返回图像本身 + 一段
// 描述信封"）。差别只在本仓没有独立的图像卡片渲染层，信封文本要同时给人看，故在
// DSH 的三段结构上压掉多余空行。
//
// 为什么由宿主提供而不是某个工具插件：三样东西都在宿主——附件库（SaveImageAttachment）、
// 投影上限（DefaultProjectionMaxSide）、以及「当前模型是否接受图像」（插件上报的
// capabilities）。插件要做这三件事得跨进程各抄一份，正是能力漂移的来源。
const readImageToolName = "read_image"

// 读图入口的硬上限。它们防的是「一张图把宿主拖死」，不是模型能看到的边界：
//
//   - 字节上限在 stat 后即挡住，避免把几百 MB 的文件整个读进内存；
//   - 像素上限挡的是解码：投影层缩放前要 image.Decode 出整图，20000×20000 的
//     PNG 可能是 8 MB 文件、却是 1.6 GB 的 RGBA。文件不大而像素极大是最坏的一类。
//
// 两者都远高于视觉模型的实用范围（最优边长 1568），只用来拒绝病态输入。
const (
	readImageMaxBytes  = 64 << 20   // 单文件 64 MiB
	readImageMaxPixels = 40_000_000 // 解码后约 4000 万像素
	readImageMaxSide   = 20_000     // 单边 2 万像素
)

type readImageTool struct{ m *Manager }

func (t *readImageTool) Name() string { return readImageToolName }

func (t *readImageTool) Description() string {
	return "Read a PNG/JPEG/GIF/WebP image file and return the image itself, so you can look at it directly — " +
		"screenshots, diagrams, charts, UI mockups, photos of a screen or a document. " +
		"One image per call. Use this whenever you need to see a picture instead of reading bytes: " +
		"do not shell out to an image converter, write a script, or try to OCR a screenshot just to describe it. " +
		"A file whose long side exceeds " + fmt.Sprint(DefaultProjectionMaxSide) + " px is downscaled for viewing, " +
		"and the reply names the scale factor so coordinates measured on the image map back to the file. " +
		"Requires a model that accepts image input; on a text-only model the call fails with an explanation."
}

func (t *readImageTool) ParametersSchema() json.RawMessage {
	return json.RawMessage(`{
  "type": "object",
  "properties": {
    "file_path": {"type": "string", "description": "Path to the image file. Relative paths are resolved against the workspace root; /workspace/... also works."}
  },
  "required": ["file_path"],
  "additionalProperties": false
}`)
}

// Execute 是接口要求的最小实现：图像本身经 ExecuteWithViewAndImages 返回，
// 凡走工具流水线的调用都会命中后者（executeToolBody 先做接口断言）。这里保留
// 同一条实现路径，避免两套逻辑各自漂移。
func (t *readImageTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	result, _, _, err := t.ExecuteWithViewAndImages(ctx, args)
	return result, err
}

// ExecuteWithViewAndImages 读图并随结果带回图像引用（dsc-img://<sha256>）。
//
// 顺序有意为之：所有「不该读」的判定都在任何文件 I/O 之前完成（模型能力、路径形态），
// 之后才 stat/读/解码——拒绝时不会留下半截附件写入。
func (t *readImageTool) ExecuteWithViewAndImages(ctx context.Context, args json.RawMessage) (string, string, []string, error) {
	var in struct {
		FilePath string `json:"file_path"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", "", nil, fmt.Errorf("invalid arguments: %w", err)
	}
	raw := strings.TrimSpace(in.FilePath)
	if raw == "" {
		return "", "", nil, fmt.Errorf("file_path is required")
	}

	// 门控先行：文本模型看不到图，读出来也只是一段描述，白白花一次调用与一张附件。
	// 说清原因与出路，而不是让它换个工具瞎试。
	if t.m != nil && !t.m.SupportsImageInput() {
		return "", "", nil, fmt.Errorf("cannot read %q as an image: the current model accepts text only. "+
			"Switch to a model with image input to look at pictures; on this model, describe the image from its filename or ask the user", raw)
	}

	path, err := ResolveWorkspacePath(raw)
	if err != nil {
		return "", "", nil, fmt.Errorf("cannot resolve %q: %w", raw, err)
	}
	path = PClean(path) // 路径出口一律正斜杆（与工具结果其它路径同规范）

	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", nil, fmt.Errorf("cannot read %q: no such file", path)
		}
		return "", "", nil, fmt.Errorf("cannot read %q: %w", path, err)
	}
	if info.IsDir() {
		return "", "", nil, fmt.Errorf("cannot read %q: it is a directory, not an image file", path)
	}
	if info.Size() > readImageMaxBytes {
		return "", "", nil, fmt.Errorf("cannot read %q: %d bytes exceeds the %s limit; downscale or crop the image first",
			path, info.Size(), humanBytesTool(readImageMaxBytes))
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", nil, fmt.Errorf("cannot read %q: %w", path, err)
	}
	// 复查：stat 与读之间文件可能变大（日志/截图正在被写）。
	if len(data) > readImageMaxBytes {
		return "", "", nil, fmt.Errorf("cannot read %q: it grew past the %s limit while reading; downscale or crop the image first",
			path, humanBytesTool(readImageMaxBytes))
	}

	mime := sniffImageMime(data)
	if mime == "application/octet-stream" {
		return "", "", nil, fmt.Errorf("cannot read %q as an image: the bytes are not PNG, JPEG, GIF or WebP "+
			"(read_image does not guess from the file name)", path)
	}
	w, h, err := imageDimensions(data, mime)
	if err != nil {
		return "", "", nil, fmt.Errorf("cannot read %q: the bytes look like %s but do not decode as a valid image (%v)",
			path, mime, err)
	}
	if w <= 0 || h <= 0 {
		return "", "", nil, fmt.Errorf("cannot read %q: it reports a %dx%d px frame", path, w, h)
	}
	if w*h > readImageMaxPixels || w > readImageMaxSide || h > readImageMaxSide {
		return "", "", nil, fmt.Errorf("cannot read %q: %dx%d px exceeds the %d-pixel / %d-px-per-side limit; "+
			"downscale the image (it would need %s of memory to decode) and read the smaller copy",
			path, w, h, readImageMaxPixels, readImageMaxSide, humanBytesTool(int64(w)*int64(h)*4))
	}

	ref, err := SaveImageAttachment(data)
	if err != nil {
		return "", "", nil, fmt.Errorf("cannot store the image from %q: %w", path, err)
	}

	return imageReadEnvelope(path, mime, w, h, len(data)), "", []string{ref}, nil
}

// imageDimensions 读取图像尺寸（不解码整图）。webp 未注册全局分发，需显式解码器。
func imageDimensions(data []byte, mime string) (int, int, error) {
	if mime == "image/webp" {
		cfg, err := webp.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return 0, 0, err
		}
		return cfg.Width, cfg.Height, nil
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, err
	}
	return cfg.Width, cfg.Height, nil
}

// imageReadEnvelope 生成结果文本（三段结构对齐 DSH：path / type / content）。
//
// 缩放说明只在真会被缩小时给出，且给出可直接用的乘数：模型在缩图上看出来的位置
// （"按钮在 (420, 180)"）必须能换算回原图坐标才有意义，否则它会按缩图坐标在原图上
// 做点击或裁剪，位置全错。
func imageReadEnvelope(path, mime string, w, h, size int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "<path>%s</path>\n<type>image</type>\n<content>\n", path)
	fmt.Fprintf(&b, "%s image, %dx%d px, %d bytes", mime, w, h, size)
	if vw, vh, factor, scaled := projectedImageSize(w, h); scaled {
		fmt.Fprintf(&b, " (viewed at %dx%d px after downscaling; multiply coordinates measured on the image by %.2f to locate features in the original file)",
			vw, vh, factor)
	}
	b.WriteString("\n</content>")
	return b.String()
}

// projectedImageSize 预测这张图在请求时的观看尺寸（与 ProjectImageRef 同一套取整）。
// 返回的 factor = 原图边长 / 观看边长，即「把图上的坐标乘多少才回到原图」。
func projectedImageSize(w, h int) (vw, vh int, factor float64, scaled bool) {
	longest := w
	if h > longest {
		longest = h
	}
	if longest <= DefaultProjectionMaxSide {
		return w, h, 1, false
	}
	// 与 resizeLongestSide 一致：最长边取上限，另一边按比例整除（int 截断）。
	if w >= h {
		vw = DefaultProjectionMaxSide
		vh = int(float64(h) * float64(DefaultProjectionMaxSide) / float64(w))
	} else {
		vh = DefaultProjectionMaxSide
		vw = int(float64(w) * float64(DefaultProjectionMaxSide) / float64(h))
	}
	if vw < 1 {
		vw = 1
	}
	if vh < 1 {
		vh = 1
	}
	return vw, vh, float64(longest) / float64(DefaultProjectionMaxSide), true
}

// humanBytesTool 人类可读的字节数（工具错误文案用；core 内另有展示用实现，不复用
// 是为了避免把这层文案与 UI 格式化绑在一起）。
func humanBytesTool(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB"}
	v := float64(n)
	i := -1
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}
