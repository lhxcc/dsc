package core

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"strconv"
	"strings"
	"testing"

	"dsc/proto"
	"dsc/proto/metadata"
)

// newTestPNG 生成 w×h 的 PNG 字节（纯色即可：尺寸与格式是这两个测试唯一关心的）。
func newTestPNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// writeTempImage 把 PNG 字节写到工作区下的临时文件，返回绝对路径。
// 走 WorkspaceRoot 是为了让 ResolveWorkspacePath 的相对路径分支也顺带被覆盖。
func writeTempImage(t *testing.T, name string, data []byte) string {
	t.Helper()
	dir := t.TempDir()
	path := PJoin(dir, name)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// runReadImage 调一次 read_image，返回结果、图像引用与错误。
func runReadImage(t *testing.T, tool *readImageTool, filePath string) (string, []string, error) {
	t.Helper()
	args, err := json.Marshal(map[string]string{"file_path": filePath})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	result, _, refs, err := tool.ExecuteWithViewAndImages(context.Background(), args)
	return result, refs, err
}

// TestReadImageReturnsRef 读一张小图：结果给三段信封，图像以 dsc-img:// 引用回来。
//
// 引用形态是关键——它必须与输入框「@图片」完全相同（内容寻址 dsc-img://<sha256>），
// 这样投影层、会话历史、LLM 插件的处理只有一套，不存在「工具读的图和用户贴的图
// 走两条路」。
func TestReadImageReturnsRef(t *testing.T) {
	data := newTestPNG(t, 120, 80)
	path := writeTempImage(t, "shot.png", data)

	tool := &readImageTool{m: NewManager(&ManagerConfig{ExecDir: t.TempDir()})}
	out, refs, err := runReadImage(t, tool, path)
	if err != nil {
		t.Fatalf("read_image: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("应回传 1 个图像引用，got %d: %v", len(refs), refs)
	}
	if !strings.HasPrefix(refs[0], imageRefPrefix) {
		t.Fatalf("引用应是 %s 前缀，got %q", imageRefPrefix, refs[0])
	}
	// 与 @ 引用同源：同内容必得同引用（内容寻址）
	again, err := SaveImageAttachment(data)
	if err != nil {
		t.Fatalf("SaveImageAttachment: %v", err)
	}
	if again != refs[0] {
		t.Fatalf("同内容应得同引用：@ 路径 %q vs 工具 %q", again, refs[0])
	}

	for _, want := range []string{"<path>" + path + "</path>", "<type>image</type>", "image/png image, 120x80 px"} {
		if !strings.Contains(out, want) {
			t.Fatalf("结果缺少 %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "<content>") || !strings.Contains(out, "</content>") {
		t.Fatalf("结果应有 content 段:\n%s", out)
	}
	// 小于投影上限：说明「原样」，不该出现缩放乘数
	if strings.Contains(out, "multiply coordinates") {
		t.Fatalf("小图不该给缩放说明:\n%s", out)
	}
}

// TestReadImageMentionsDownscale 大于投影上限的图必须给出观看尺寸与换算乘数。
//
// 没有这句话，模型会把在缩图上看到的位置当成原图坐标——在截图分析、点击定位这类
// 场景里就是稳定地错一个比例。
func TestReadImageMentionsDownscale(t *testing.T) {
	data := newTestPNG(t, 4000, 3000)
	path := writeTempImage(t, "big.png", data)

	tool := &readImageTool{m: NewManager(&ManagerConfig{ExecDir: t.TempDir()})}
	out, refs, err := runReadImage(t, tool, path)
	if err != nil {
		t.Fatalf("read_image: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("应回传图像引用，got %v", refs)
	}
	if !strings.Contains(out, "4000x3000 px") {
		t.Fatalf("应报告原始尺寸:\n%s", out)
	}
	if !strings.Contains(out, "multiply coordinates measured on the image by 2.55") {
		t.Fatalf("应给出 4000/1568=2.55 的换算乘数:\n%s", out)
	}
	// 预测的观看尺寸必须与投影层真实产物一致——两处各算一遍就会漂
	url, err := ProjectImageRef(refs[0], DefaultProjectionMaxSide)
	if err != nil {
		t.Fatalf("ProjectImageRef: %v", err)
	}
	gotW, gotH := dataURLDimensions(t, url)
	if !strings.Contains(out, "viewed at "+strconv.Itoa(gotW)+"x"+strconv.Itoa(gotH)+" px") {
		t.Fatalf("信封的观看尺寸与投影实际不一致：投影 %dx%d\n%s", gotW, gotH, out)
	}
}

// TestReadImageRejectsNonImage 扩展名不是图片、字节也不是图片 → 明确拒绝。
//
// 不按扩展名放行、也不按扩展名猜测，是为了让「读错了文件」当场暴露，而不是等到
// 模型盯着一张空白图猜内容。
func TestReadImageRejectsNonImage(t *testing.T) {
	path := writeTempImage(t, "notes.txt", []byte("这不是图片，只是一段文本。"))
	tool := &readImageTool{m: NewManager(&ManagerConfig{ExecDir: t.TempDir()})}
	if _, _, err := runReadImage(t, tool, path); err == nil {
		t.Fatal("非图片文件应报错")
	} else if !strings.Contains(err.Error(), "not PNG, JPEG, GIF or WebP") {
		t.Fatalf("错误应说明支持的格式: %v", err)
	}

	// 扩展名像图片但内容是文本：同样拒绝（以字节为准）
	fake := writeTempImage(t, "fake.png", []byte("not a png at all"))
	if _, _, err := runReadImage(t, tool, fake); err == nil {
		t.Fatal("字节不是图片时即便扩展名是 .png 也应报错")
	}
}

// TestReadImageMissingAndDir 路径写错是最常见的失败，错误必须点名路径。
func TestReadImageMissingAndDir(t *testing.T) {
	tool := &readImageTool{m: NewManager(&ManagerConfig{ExecDir: t.TempDir()})}
	if _, _, err := runReadImage(t, tool, PJoin(t.TempDir(), "nope.png")); err == nil ||
		!strings.Contains(err.Error(), "no such file") {
		t.Fatalf("缺失文件应报 no such file，got %v", err)
	}
	dir := t.TempDir()
	if _, _, err := runReadImage(t, tool, dir); err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("目录应报 directory，got %v", err)
	}
}

// TestReadImageRejectsHugePixels 像素过大要拒绝并给出可执行的出路。
//
// 字节数不大、像素极多的图（纯色 20000×20000 PNG 只有几十 KB）能过字节检查，
// 但解码要 1.6 GB——这一类必须在入口挡住，否则宿主被一张图拖死。
func TestReadImageRejectsHugePixels(t *testing.T) {
	data := newTestPNG(t, 7000, 7000) // 4900 万像素 > 4000 万上限
	path := writeTempImage(t, "huge.png", data)
	tool := &readImageTool{m: NewManager(&ManagerConfig{ExecDir: t.TempDir()})}
	_, _, err := runReadImage(t, tool, path)
	if err == nil {
		t.Fatal("超像素上限应报错")
	}
	if !strings.Contains(err.Error(), "px exceeds") || !strings.Contains(err.Error(), "downscale") {
		t.Fatalf("错误应说明超限与出路: %v", err)
	}
}

// TestReadImageGatedByModelVision 文本模型上读图应直接拒绝，并说清换模型的出路。
//
// 不拒绝的话，图会被存进附件库、随每条后续请求重发，但模型根本看不见——
// 既费 token 又让人以为「它看过图了」。
func TestReadImageGatedByModelVision(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	m.agentLLMName = "llm-test"
	m.coreMetadata["llm-test"] = &metadata.PluginInfo{
		Name:         "llm-test",
		Capabilities: map[string]string{"supports_images": "false"},
	}
	path := writeTempImage(t, "shot.png", newTestPNG(t, 64, 64))

	tool := &readImageTool{m: m}
	_, refs, err := runReadImage(t, tool, path)
	if err == nil {
		t.Fatal("文本模型上读图应报错")
	}
	if len(refs) != 0 {
		t.Fatalf("被拒时不应产出图像引用: %v", refs)
	}
	if !strings.Contains(err.Error(), "text only") || !strings.Contains(err.Error(), "Switch to a model") {
		t.Fatalf("错误应说明原因与出路: %v", err)
	}

	// 能力改为支持 → 同一路径应放行（判定不是一次性缓存）
	m.coreMetadata["llm-test"].Capabilities["supports_images"] = "true"
	if _, refs, err := runReadImage(t, tool, path); err != nil || len(refs) != 1 {
		t.Fatalf("视觉模型上应放行: err=%v refs=%v", err, refs)
	}
}

// TestReadImageIsImageExecutor 注册表按接口断言分发图像：read_image 必须实现
// ViewImageExecutor，否则工具结果里的图会静默丢掉（文本还在，模型以为看过图）。
func TestReadImageIsImageExecutor(t *testing.T) {
	var tool interface{} = &readImageTool{}
	if _, ok := tool.(ViewImageExecutor); !ok {
		t.Fatal("read_image 必须实现 ViewImageExecutor，否则图像不会随结果送回")
	}
}

// TestReadImageInToolCatalog 工具必须真的出现在模型可见目录里（注册漏了等于没做）。
func TestReadImageInToolCatalog(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	var found *proto.Tool
	for _, tl := range m.AllToolsProto() {
		if tl.GetName() == readImageToolName {
			found = tl
			break
		}
	}
	if found == nil {
		t.Fatal("工具目录里没有 read_image")
	}
	if !strings.Contains(found.GetParametersJson(), "file_path") {
		t.Fatalf("参数 schema 应含 file_path: %s", found.GetParametersJson())
	}
	if !strings.Contains(found.GetDescription(), "image") {
		t.Fatalf("描述应说明读的是图像: %s", found.GetDescription())
	}
}

// dataURLDimensions 从 data URL 里解出图像尺寸（测试辅助：验证投影真实产物）。
func dataURLDimensions(t *testing.T, url string) (int, int) {
	t.Helper()
	idx := strings.Index(url, ";base64,")
	if idx < 0 {
		t.Fatalf("不是 data URL: %.40s", url)
	}
	raw, err := base64.StdEncoding.DecodeString(url[idx+len(";base64,"):])
	if err != nil {
		t.Fatalf("base64: %v", err)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("decode projected: %v", err)
	}
	return cfg.Width, cfg.Height
}

// TestRunCodeSDKExcludesImageTools 图像类工具不得进 run_code 的 Lua SDK。
//
// run_code 的 Lua 绑定只把结果作为**字符串**交回程序，图像附件无处可放——于是
// 调用会「成功」，模型拿到一段描述却看不到图，还以为自己看过。让它根本不存在，
// 模型才会去直接调；这比静默丢图诚实。
func TestRunCodeSDKExcludesImageTools(t *testing.T) {
	m := NewManager(&ManagerConfig{ExecDir: t.TempDir()})
	rc := &runCodeTool{m: m}

	imageNames := rc.imageToolNames()
	if !imageNames[readImageToolName] {
		t.Fatalf("read_image 应按接口（ViewImageExecutor）被识别为图像工具，got %v", imageNames)
	}
	skip := rc.imageToolNames()
	skip[runCodeToolName] = true
	specs := toolSpecsFromProto(m.AllToolsProto(), skip)
	for _, sp := range specs {
		if sp.Name == readImageToolName {
			t.Fatal("read_image 不该出现在 run_code 的 SDK 里")
		}
		if sp.Name == runCodeToolName {
			t.Fatal("run_code 自身不该出现在 SDK 里（会嵌套/遮蔽）")
		}
	}
	if len(specs) == 0 {
		t.Fatal("除图像工具外，其余工具应照常进 SDK")
	}
}
