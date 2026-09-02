// gercmd 是配合本仓库 actor 框架使用的命令行工具。
//
// 目前提供五个互相独立的子命令：
//
//	dirs    列出路径下的文件夹
//	files   列出路径下的文件
//	cat     打印文件内容
//	check   检查模块是否符合 GameSvr 的模块规范
//	gen     为模块的公有方法生成 player 包的门面函数
//
// 用法与各命令的取舍见同目录的 README.md。
//
// 代码组织方式：每个子命令的实现和它的入口函数放在同一个文件里
// （list.go / cat.go / check.go / gen.go），共用的部分抽在 cli.go 与 walk.go。
// 加新功能 = 加一个文件 + 在下面的命令表里加一行，不动已有子命令。
package main

import (
	"fmt"
	"os"
)

// commands 是子命令表。顺序即帮助里的显示顺序。
var commands = []struct {
	name  string
	brief string
	run   func(args []string) int
}{
	{"dirs", "列出文件夹", func(args []string) int { return runList(args, kindDir) }},
	{"files", "列出文件", func(args []string) int { return runList(args, kindFile) }},
	{"cat", "打印文件内容", runCat},
	{"check", "检查模块是否符合规范", runCheck},
	{"gen", "生成门面函数", runGen},
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(exitUsage)
	}
	switch os.Args[1] {
	case "-h", "--help", "help":
		usage()
		return
	}
	for _, c := range commands {
		if c.name == os.Args[1] {
			os.Exit(c.run(os.Args[2:]))
		}
	}
	os.Exit(unknownCommand(os.Args[1]))
}

// unknownCommand 报告命令写错，并尽量猜出用户想干什么。
//
// 第一个参数如果本身就是个存在的路径，多半是把子命令漏了
// （比如从只有一个功能的旧版本升上来），这时直接给出正确写法比让人翻帮助强。
func unknownCommand(arg string) int {
	fmt.Fprintf(os.Stderr, "未知命令 %q\n", arg)
	if info, err := os.Stat(arg); err == nil {
		if info.IsDir() {
			fmt.Fprintf(os.Stderr, "如果想列这个路径，用：gercmd dirs %s  或  gercmd files %s\n", arg, arg)
		} else {
			fmt.Fprintf(os.Stderr, "如果想看这个文件，用：gercmd cat %s\n", arg)
		}
	}
	fmt.Fprintln(os.Stderr)
	usage()
	return exitUsage
}

func usage() {
	fmt.Fprint(os.Stderr, "列出路径下的文件夹与文件、打印文件内容、检查模块规范。\n\n"+
		"用法:\n  gercmd <命令> [选项] [参数]\n\n命令:\n")
	for _, c := range commands {
		fmt.Fprintf(os.Stderr, "  %s %s\n", padRight(c.name, 8), c.brief)
	}
	fmt.Fprint(os.Stderr, `
dirs / files / check 不给路径时用当前目录。dirs / files 默认只看直接子条目、
且包含隐藏条目，都不跟随符号链接。所有命令的结果走 stdout，统计与告警走 stderr，
所以可以直接重定向或接管道而不会混进杂音。

退出码: 0 正常 · 1 干活时出错 · 2 用法错误

各命令的选项用 gercmd <命令> -h 查看。

示例:
  gercmd dirs cmd                    列出 cmd 下的文件夹
  gercmd files cmd/gercmd            列出 cmd/gercmd 下的文件
  gercmd files -r -skip-hidden .     递归列出所有文件，跳过 .git 之类
  gercmd cat -n cmd/gercmd/main.go   带行号打印文件
  gercmd check bag cmd/GameSvr       检查 BagMod 是否符合模块规范
  gercmd gen -n cmd/GameSvr/bag/bag_mod.go cmd/GameSvr/player
                                     预览要为 bag 生成的门面函数
`)
}
