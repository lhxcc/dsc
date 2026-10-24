package session

import "path/filepath"

// posix 路径拼接/父目录（结果归一化为正斜杆）。session 包与 core 存在包级
// import 环（core 引用 session 存储、session 不引用 core），无法复用 core.P*，
// 故在本包内提供等价实现；本文件是 session 包内唯一允许直接调用被禁 filepath
// 函数的文件（core/filepath_guard_test.go 白名单）。
func posixJoin(elem ...string) string {
	return filepath.ToSlash(filepath.Join(elem...))
}

func posixDir(path string) string {
	return filepath.ToSlash(filepath.Dir(path))
}

// posixClean 等价 core.PClean：按平台语义清理后强制归一化为正斜杆。
//
// 为什么不能用标准库 path 包的 Clean（它同样是「清理」语义）：path 只把 `/` 当分隔符，
// 反斜杆在它眼里是普通字符，清理前后都原样保留——那违背规则 10「一律归一化为正斜杆」，
// 因此它已与 filepath 那族一并列入守卫黑名单。实测（Windows）：path 保留反斜杆，
// 本函数转换。反斜杆一旦漏进会话键，键就会含文件名非法字符，会话直接落不了盘。
//
// 与 core.PClean 的唯一差别是 UNC 前导分隔符：Windows 的 filepath.Clean 会保留
// `\\server\share` 的双前导，POSIX 上则压成单个——调用方若需要跨平台一致的键，
// 应先把前导分隔符去掉（见 session.normalizeProjectPath）。
func posixClean(p string) string {
	return filepath.ToSlash(filepath.Clean(p))
}
