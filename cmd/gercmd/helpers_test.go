package main

import (
	"os"
	"path/filepath"
	"testing"
)

// 三个测试文件都要在临时目录里造目录和文件，原先各写了一遍
// MkdirAll/WriteFile 加 t.Fatalf 的样板。收到这里之后，
// 用例正文只剩下"造什么"，不再夹杂"怎么造"。

// mkdir 在 root 下建出多级子目录，rel 用 / 分隔。
func mkdir(t *testing.T, root, rel string) string {
	t.Helper()
	dir := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建目录 %s 失败: %v", rel, err)
	}
	return dir
}

// writeFile 在 dir 下写一个文件，dir 必须已存在。
func writeFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("写文件 %s 失败: %v", name, err)
	}
	return p
}

// writeFileAt 按相对 root 的路径写文件，中间目录自动建好。
// rel 用 / 分隔，跨平台一致。
func writeFileAt(t *testing.T, root, rel string, data []byte) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("建 %s 的父目录失败: %v", rel, err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("写文件 %s 失败: %v", rel, err)
	}
	return p
}
