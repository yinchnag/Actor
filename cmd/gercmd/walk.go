package main

import (
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strings"
)

// Options 是三个遍历类子命令共用的选项。
type Options struct {
	// Naming 命名约定。零值表示用 defaultNaming()。
	Naming Naming

	// Recursive 为 true 时递归进入所有层级；为 false 时只看直接子条目。
	Recursive bool
	// SkipHidden 跳过以 . 开头的条目。隐藏目录会连同整棵子树一起剪掉，
	// 否则会出现"目录被过滤了、里面的东西却还在"的自相矛盾结果。
	//
	// 按点号前缀判断是 Go 工具链的跨平台惯例；Windows 上的"隐藏"是文件属性，
	// 这里不去读属性——三个平台行为一致，比在某个平台上更"准"更重要。
	SkipHidden bool
}

// walkEntry 是遍历过程中交给回调的一个条目。
type walkEntry struct {
	// Path 是可以直接拿去打开文件的完整路径。
	Path string
	// Rel 是相对 root 的路径，分隔符已统一成 /，
	// 这样 Windows 上的输出才不会和其它平台不一致。
	Rel string
	// D 携带条目类型，用 D.IsDir() / D.Type().IsRegular() 判定。
	D fs.DirEntry
}

// walkTree 遍历 root 下的条目，逐个交给 fn。
//
// 抽出来是因为这里有三条容易写错、且必须在所有子命令间保持一致的规则：
//
//  1. root 自己不作为条目上报，也不受隐藏规则约束——
//     用户明确指了这个路径，哪怕它叫 .config 也得照办；
//  2. 隐藏目录整棵子树一起剪掉，而不是只漏掉它自己；
//  3. 中途某个目录读不动（多半是权限）只跳过它并写一句告警，不中断整趟。
//     但 root 本身读不动是真失败，要返回错误——不能让人以为"这里就是空的"。
//
// 之前 list 和 check 各写了一份，第 3 条的处理还不一样：一边告警一边静默，
// 于是 check 遇到受限目录会悄悄漏掉结果。统一到这里就不会再各走各的。
//
// fn 返回非 nil 会中止遍历并把该错误原样抛出。
func walkTree(root string, opt Options, warn io.Writer, fn func(walkEntry) error) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err // 起点就读不动，这是真失败
			}
			fmt.Fprintf(warn, "跳过 %s: %v\n", path, err)
			return fs.SkipDir
		}
		if path == root {
			return nil
		}
		if opt.SkipHidden && isHidden(d.Name()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path // 理论上到不了这儿；真到了退回绝对路径，总比丢掉这条强
		}
		if ferr := fn(walkEntry{Path: path, Rel: filepath.ToSlash(rel), D: d}); ferr != nil {
			return ferr
		}

		// 非递归模式：这一层报过就不再往下钻
		if d.IsDir() && !opt.Recursive {
			return fs.SkipDir
		}
		return nil
	})
}

// isHidden 判断条目名是否以点号开头。
// naming 返回本次要用的命名约定，零值时退回默认。
func (that Options) naming() Naming { return that.Naming.withDefaults() }

func isHidden(name string) bool {
	return strings.HasPrefix(name, ".")
}

// hasGoSuffix 判断是不是 Go 源文件。
func hasGoSuffix(name string) bool {
	return strings.HasSuffix(name, ".go")
}
