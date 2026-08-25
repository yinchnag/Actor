package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
)

// 退出码约定，三个子命令一致：
//
//	0  正常
//	1  干活时出错（路径不存在、检查不通过等）
//	2  用法错误（命令写错、参数个数不对）
//
// 分开是为了让脚本能区分"跑了但结果是坏的"和"我命令写错了"。
const (
	exitOK    = 0
	exitFail  = 1
	exitUsage = 2
)

// subcmd 收拢子命令共有的样板：建 FlagSet、统一的用法提示、位置参数校验。
//
// 三个子命令原先各写一遍这套流程，用法行的格式还不完全一致。
// 收到一处之后，加新命令只需要关心它自己的选项和活儿。
type subcmd struct {
	name string
	fs   *flag.FlagSet
}

// newSubcmd 建一个子命令。
//
//	argSpec 是用法行里参数部分的写法，如 "[路径]"、"<文件>"
//	notes   是跟在用法行后面的补充说明，可以为空
func newSubcmd(name, argSpec, notes string) *subcmd {
	c := &subcmd{name: name, fs: flag.NewFlagSet(name, flag.ExitOnError)}
	c.fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "用法: gercmd %s [选项] %s\n", name, argSpec)
		if notes != "" {
			fmt.Fprintf(os.Stderr, "\n%s\n", strings.TrimRight(notes, "\n"))
		}
		fmt.Fprint(os.Stderr, "\n选项:\n")
		c.fs.PrintDefaults()
	}
	return c
}

// parse 解析选项并校验位置参数个数。
//
// 校验不过时提示已经打好了，调用方直接把 code 交出去即可：
//
//	pos, code := c.parse(args, 0, 1)
//	if code != exitOK { return code }
func (c *subcmd) parse(args []string, minArgs, maxArgs int) (positional []string, code int) {
	_ = c.fs.Parse(args) // ExitOnError：选项写错时 flag 包会自己报错并退出

	n := c.fs.NArg()
	if n < minArgs || n > maxArgs {
		fmt.Fprintf(os.Stderr, "%s 需要 %s，收到 %d 个\n\n", c.name, argCountDesc(minArgs, maxArgs), n)
		c.fs.Usage()
		return nil, exitUsage
	}
	return c.fs.Args(), exitOK
}

func argCountDesc(min, max int) string {
	switch {
	case min == max:
		return fmt.Sprintf("%d 个参数", min)
	case min == 0:
		return fmt.Sprintf("至多 %d 个参数", max)
	default:
		return fmt.Sprintf("%d 到 %d 个参数", min, max)
	}
}

// argOr 取第 i 个位置参数，没有就用默认值。
// 三个子命令的路径参数都可省略，取默认值这件事不必各写一遍。
func argOr(positional []string, i int, def string) string {
	if i < len(positional) {
		return positional[i]
	}
	return def
}

// fail 是统一的失败出口：错误写 stderr，返回退出码。
func fail(err error) int {
	fmt.Fprintf(os.Stderr, "%v\n", err)
	return exitFail
}

// countLine 打印"共 N 个X"这类收尾统计。
// 走 stderr 是刻意的：stdout 要留给结果本身，才能干净地接管道或重定向。
func countLine(n int64, unit string) {
	fmt.Fprintf(os.Stderr, "共 %d %s\n", n, unit)
}

// padRight 按显示宽度补空格。
//
// 不能用 %-Ns：那个数的是字节数，而一个汉字占 3 字节却只显示 2 列，
// 中英混排的标题会全部歪掉。
func padRight(s string, width int) string {
	w := 0
	for _, r := range s {
		if isWideRune(r) {
			w += 2
		} else {
			w++
		}
	}
	if w >= width {
		return s
	}
	return s + strings.Repeat(" ", width-w)
}

// isWideRune 判断是否为占两列的字符。
// 只覆盖 CJK 与全角这几段，这个工具的输出里不会出现别的宽字符。
func isWideRune(r rune) bool {
	switch {
	case r >= 0x1100 && r <= 0x115F, // 谚文字母
		r >= 0x2E80 && r <= 0xA4CF, // CJK 部首、假名、汉字
		r >= 0xAC00 && r <= 0xD7A3, // 谚文音节
		r >= 0xF900 && r <= 0xFAFF, // CJK 兼容汉字
		r >= 0xFE30 && r <= 0xFE6F, // CJK 兼容形式
		r >= 0xFF00 && r <= 0xFF60, // 全角
		r >= 0xFFE0 && r <= 0xFFE6:
		return true
	}
	return false
}
