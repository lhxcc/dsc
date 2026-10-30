package main

import (
	"testing"

	"gorm.io/gorm"
)

// setupMemoryDir 把记忆目录指向临时目录，并清空句柄池。
//
// 记忆库现在是「一项目一文件」，测试必须从空目录开始，否则会读到开发机上真实的
// 记忆库（不同项目的库都在同一个目录下）；关句柄是为了让 Windows 能删掉临时目录。
func setupMemoryDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("DSC_MEMORY_DIR", dir)
	dbs.closeAll()
	t.Cleanup(func() {
		resetPendingMemories()
		dbs.closeAll()
	})
	return dir
}

// dbFor 取某个项目键的库句柄（不存在则创建，与生产路径同一入口）。
func dbFor(t *testing.T, key string) *gorm.DB {
	t.Helper()
	db, err := dbs.db(key)
	if err != nil {
		t.Fatalf("open db %q: %v", key, err)
	}
	return db
}
