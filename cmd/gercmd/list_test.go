package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// buildTree 造一棵固定的目录树，文件夹和文件在各层都有，也有隐藏的：
//
//	root/
//	  alpha/
//	    inner.md
//	    nested/
//	      deep/
//	        buried.txt
//	  beta/                 空目录
//	  .hidden/
//	    .dotfile
//	    inside/
//	      secret.txt
//	  note.txt
//	  .env
func buildTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	// beta 是空目录，得单独建——它下面没有文件，靠 writeFileAt 带不出来
	for _, d := range []string{"beta"} {
		mkdir(t, root, d)
	}
	for _, f := range []string{
		"note.txt", ".env",
		"alpha/inner.md", "alpha/nested/deep/buried.txt",
		".hidden/.dotfile", ".hidden/inside/secret.txt",
	} {
		writeFileAt(t, root, f, []byte("x"))
	}
	return root
}

func run(t *testing.T, do lister, root string, opt Options) ([]string, int) {
	t.Helper()
	var out, warn bytes.Buffer
	n, err := do(root, opt, &out, &warn)
	if err != nil {
		t.Fatalf("列举失败: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = nil
	}
	return lines, n
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func check(t *testing.T, got []string, n int, want []string) {
	t.Helper()
	if !equal(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	if n != len(want) {
		t.Fatalf("计数 = %d, want %d", n, len(want))
	}
}

// --- 文件夹（原有功能，改造后必须保持行为不变）---

func TestDirsChildren(t *testing.T) {
	got, n := run(t, ListDirs, buildTree(t), Options{})
	check(t, got, n, []string{".hidden", "alpha", "beta"})
}

func TestDirsSkipHidden(t *testing.T) {
	got, n := run(t, ListDirs, buildTree(t), Options{SkipHidden: true})
	check(t, got, n, []string{"alpha", "beta"})
}

func TestDirsRecursive(t *testing.T) {
	got, n := run(t, ListDirs, buildTree(t), Options{Recursive: true})
	check(t, got, n, []string{
		".hidden", ".hidden/inside",
		"alpha", "alpha/nested", "alpha/nested/deep",
		"beta",
	})
}

func TestDirsRecursiveSkipHiddenPrunesSubtree(t *testing.T) {
	got, _ := run(t, ListDirs, buildTree(t), Options{Recursive: true, SkipHidden: true})
	check(t, got, len(got), []string{"alpha", "alpha/nested", "alpha/nested/deep", "beta"})
}

// --- 文件（新增功能）---

// TestFilesChildren 只列直接子文件，文件夹不算，隐藏文件默认要列出来。
func TestFilesChildren(t *testing.T) {
	got, n := run(t, ListFiles, buildTree(t), Options{})
	check(t, got, n, []string{".env", "note.txt"})
}

func TestFilesSkipHidden(t *testing.T) {
	got, n := run(t, ListFiles, buildTree(t), Options{SkipHidden: true})
	check(t, got, n, []string{"note.txt"})
}

// TestFilesRecursive 递归时输出相对路径。deep/buried.txt 与 inside/secret.txt
// 都叫不同名字，但真实项目里同名文件遍地都是——只给文件名根本没法用。
func TestFilesRecursive(t *testing.T) {
	got, n := run(t, ListFiles, buildTree(t), Options{Recursive: true})
	check(t, got, n, []string{
		".env",
		".hidden/.dotfile", ".hidden/inside/secret.txt",
		"alpha/inner.md", "alpha/nested/deep/buried.txt",
		"note.txt",
	})
	for _, line := range got {
		if strings.Contains(line, `\`) {
			t.Fatalf("输出里有反斜杠，跨平台不一致: %q", line)
		}
	}
}

// TestFilesRecursiveSkipHidden 隐藏目录里的文件也要一起跳掉——
// 只滤掉 .env 却留下 .hidden/inside/secret.txt 是自相矛盾的。
func TestFilesRecursiveSkipHidden(t *testing.T) {
	got, n := run(t, ListFiles, buildTree(t), Options{Recursive: true, SkipHidden: true})
	check(t, got, n, []string{"alpha/inner.md", "alpha/nested/deep/buried.txt", "note.txt"})
}

// TestFilesEmptyDir 只有子目录、没有文件时，结果为空而不是报错。
func TestFilesEmptyDir(t *testing.T) {
	root := buildTree(t)
	got, n := run(t, ListFiles, filepath.Join(root, "beta"), Options{})
	if len(got) != 0 || n != 0 {
		t.Fatalf("空目录应当没有输出, got %v (n=%d)", got, n)
	}
}

// --- 两个功能共有的性质 ---

// TestRootItselfNotListed 起始目录自己不算"这个路径里的"东西。
func TestRootItselfNotListed(t *testing.T) {
	root := buildTree(t)
	for _, do := range []lister{ListDirs, ListFiles} {
		for _, opt := range []Options{{}, {Recursive: true}} {
			got, _ := run(t, do, root, opt)
			for _, line := range got {
				if line == "." || line == filepath.Base(root) {
					t.Fatalf("起始目录不该出现在结果里: %q", line)
				}
			}
		}
	}
}

// TestHiddenRootStillListed 用户明确指了一个隐藏目录，就得照列——
// 隐藏规则只作用于里面的条目，不能把起始目录自己剪掉。
func TestHiddenRootStillListed(t *testing.T) {
	root := filepath.Join(buildTree(t), ".hidden")
	got, _ := run(t, ListFiles, root, Options{Recursive: true, SkipHidden: true})
	// .dotfile 是隐藏的会被滤掉，inside/secret.txt 该留下
	check(t, got, len(got), []string{"inside/secret.txt"})
}

// TestErrors 路径不存在、或指向文件时要给出清楚的错误，两个功能表现一致。
func TestErrors(t *testing.T) {
	root := buildTree(t)
	for _, do := range []lister{ListDirs, ListFiles} {
		var out, warn bytes.Buffer
		if _, err := do(filepath.Join(root, "不存在"), Options{}, &out, &warn); err == nil {
			t.Fatal("路径不存在时应当报错")
		}
		_, err := do(filepath.Join(root, "note.txt"), Options{}, &out, &warn)
		if err == nil {
			t.Fatal("路径指向文件时应当报错")
		}
		if !strings.Contains(err.Error(), "不是文件夹") {
			t.Fatalf("错误信息没说清原因: %v", err)
		}
	}
}

// TestSymlinkCountedAsNeither 符号链接既不算文件夹也不算文件：
// 当文件夹会在递归时成环，当文件则会把目录的软链也数进去。
func TestSymlinkCountedAsNeither(t *testing.T) {
	root := buildTree(t)
	if err := os.Symlink(filepath.Join(root, "alpha"), filepath.Join(root, "link-dir")); err != nil {
		t.Skipf("本环境建不了符号链接（Windows 需要权限），跳过: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "note.txt"), filepath.Join(root, "link-file")); err != nil {
		t.Skipf("建文件符号链接失败，跳过: %v", err)
	}

	for _, do := range []lister{ListDirs, ListFiles} {
		got, _ := run(t, do, root, Options{Recursive: true})
		for _, line := range got {
			if strings.HasPrefix(line, "link-") {
				t.Fatalf("符号链接被算进结果了: %q", line)
			}
		}
	}
}
