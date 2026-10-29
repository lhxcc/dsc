package main

// 字符集解码后端（纯 Go，全平台）：把「码页号」翻成字符集名，按名字严格解码。
//
// 为什么需要第二套后端：Windows 的 MultiByteToWideChar 覆盖全部系统码页（含韩文
// 949），但**只在 Windows 存在**；非 Windows 平台上，GBK/Big5 输出的工具同样存在
// （容器里的老工具、跨区脚本），此前一律退化为 U+FFFD。这里用 mahonia 补上那半边。
//
// 要认清 mahonia 的能力边界：它是**转换器**，不做编码检测。所谓「检测」只能是
// 「这个字符集能不能把这些字节解成合法文本」——与 Windows 严格解码等价，且同样
// 无法区分 GBK 与 Big5（两者区间重叠，交叉解码会成功但给出错字）。所以它只当一个
// 解码后端用，码页由系统或 DSC_OUTPUT_CODEPAGE 决定，不靠它猜。

import (
	"runtime"
	"strings"

	"github.com/axgle/mahonia"
)

// charsetForCodePage 码页号 → mahonia 字符集名（不认识的返回空）。
//
// 936 映射到 gb18030 而非 gbk：GB18030 是 GBK 的超集，四字节区段（生僻字/emoji 区的
// 汉字）只有它解得开；实测 mahonia 的 "gbk" 遇到四字节序列会吐 U+FFFD。韩文 949 在
// mahonia 里没有对应字符集（其字符集表不含 euc-kr），故留空——Windows 上仍由
// MultiByteToWideChar 负责。
func charsetForCodePage(cp uint32) string {
	switch cp {
	case 936:
		return "gb18030"
	case 950:
		return "big5"
	case 932:
		return "shift_jis"
	case 874:
		return "cp874"
	case 437:
		return "cp437"
	case 850:
		return "cp850"
	case 1250:
		return "windows-1250"
	case 1251:
		return "windows-1251"
	case 1252:
		return "windows-1252"
	case 1254:
		return "windows-1254"
	case 65001:
		return "utf-8"
	}
	return ""
}

// decodeByCharset 按字符集名严格解码：解出任何 U+FFFD 即视为该字符集不成立。
//
// 「严格」就是这里的唯一判据——mahonia 不提供置信度或检测接口，U+FFFD 是它唯一
// 的失败信号（非法序列、半个双字节都会落到替换字符）。
func decodeByCharset(name, s string) (string, bool) {
	if name == "" || s == "" {
		return "", false
	}
	if runtime.GOOS != "windows" && strings.EqualFold(name, "utf-8") {
		return s, true // 已经是 UTF-8（调用方只在非法字节上进来，这里只是防御）
	}
	dec := mahonia.NewDecoder(name)
	if dec == nil {
		return "", false
	}
	out := dec.ConvertString(s)
	if strings.ContainsRune(out, '\uFFFD') {
		return "", false
	}
	return out, true
}
