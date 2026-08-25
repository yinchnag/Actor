package main

import (
	"fmt"
	"io"
	"io/fs"
	"os"
)

// kind 指定这趟要列什么。dirs 和 files 对外是两个独立命令，
// 底层共用同一套遍历与过滤，差别只在这一个判定上。
type kind int

const (
	kindDir kind = iota
	kindFile
)

func (k kind) unit() string {
	if k == kindFile {
		return "个文件"
	}
	return "个文件夹"
}

// matches 判断一个条目是否属于本次要列的类型。
func (k kind) matches(d fs.DirEntry) bool {
	if k == kindDir {
		return d.IsDir()
	}
	// 不用 !d.IsDir()：那样符号链接、设备文件之类也会被算成文件。
	// 只认常规文件，与"不把符号链接当文件夹"是同一套口径。
	return d.Type().IsRegular()
}

// lister 是 ListDirs 与 ListFiles 共同的形状，便于把两者当参数传递。
type lister func(root string, opt Options, out, warn io.Writer) (int, error)

// ListDirs 把 root 下的文件夹名写进 out，每行一个。
func ListDirs(root string, opt Options, out, warn io.Writer) (int, error) {
	return list(root, kindDir, opt, out, warn)
}

// ListFiles 把 root 下的文件名写进 out，每行一个。
func ListFiles(root string, opt Options, out, warn io.Writer) (int, error) {
	return list(root, kindFile, opt, out, warn)
}

// list 是两个功能共用的实现。
//
// 非递归时输出条目名；递归时输出相对 root 的路径——递归下只给名字会重名，
// 也没法定位，等于没用。
//
// 返回的 error 只表示 root 本身有问题；中途读不动的目录由 walkTree 写进 warn 后跳过。
func list(root string, what kind, opt Options, out, warn io.Writer) (int, error) {
	if err := mustBeDir(root); err != nil {
		return 0, err
	}
	n := 0
	err := walkTree(root, opt, warn, func(e walkEntry) error {
		if !what.matches(e.D) {
			return nil
		}
		name := e.D.Name()
		if opt.Recursive {
			name = e.Rel
		}
		fmt.Fprintln(out, name)
		n++
		return nil
	})
	if err != nil {
		return n, fmt.Errorf("遍历 %s 失败: %w", root, err)
	}
	return n, nil
}

// mustBeDir 校验路径存在且是文件夹。
// 单独拎出来是为了让"路径不存在"和"给的是文件"这两种错各有清楚的说法，
// 而不是笼统一句遍历失败。
func mustBeDir(root string) error {
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("无法访问 %s: %w", root, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s 不是文件夹", root)
	}
	return nil
}

// runList 是 dirs 与 files 两个子命令的入口，二者只差一个 kind。
func runList(args []string, what kind) int {
	name := "dirs"
	if what == kindFile {
		name = "files"
	}

	var opt Options
	c := newSubcmd(name, "[路径]", "不给路径时用当前目录。")
	c.fs.BoolVar(&opt.Recursive, "r", false, "递归进入所有层级（此时输出相对路径而非名称）")
	c.fs.BoolVar(&opt.SkipHidden, "skip-hidden", false, "跳过以 . 开头的条目，如 .git、.env")
	pos, code := c.parse(args, 0, 1)
	if code != exitOK {
		return code
	}

	// 结果走 stdout，统计和告警走 stderr——stdout 才能干净地接管道
	n, err := list(argOr(pos, 0, "."), what, opt, os.Stdout, os.Stderr)
	if err != nil {
		return fail(err)
	}
	countLine(int64(n), what.unit())
	return exitOK
}
