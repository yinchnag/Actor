package main

import (
	"go/ast"
	"strings"
)

// GameSvr 的模块方法用 doc 注释里的 export: 标记声明它对应的门面函数名：
//
//	// 增加物品
//	//	export: BagAddItem
//	func (that *BagMod) AddItem(itemid, count int) {}
//
// 意思是 BagMod.AddItem 要通过 player 包的门面暴露成 PlayerEnt.BagAddItem。
// 没带标记的公有方法就只是模块内部方法，不对外。

// exportMarker 是标记的前缀。
const exportMarker = "export:"

// ModuleMethod 是模块上的一个公有方法。
type ModuleMethod struct {
	Name  string // 方法名，如 AddItem
	Where string // file:line

	// Exported 表示带了 export: 标记，属于导出函数。
	Exported bool
	// ExportName 是标记声明的门面函数名，如 BagAddItem。
	ExportName string
	// Extra 是同一个方法上多余的 export: 标记。一个方法只该对应一个门面函数，
	// 出现多个说明写重了，得让人知道。
	Extra []string
}

// parseExportMarkers 从 doc 注释里取出所有 export: 声明的名字。
//
// 两个容易出错的地方：
//
//   - CommentGroup.Text() 只剥掉 // 和紧跟的一个空格，行首的缩进会原样留下
//     （实测 "//\texport: X" 出来是 "\texport: X"），所以每行必须先 TrimSpace，
//     否则前缀匹配一律落空；
//   - 前缀之后也要 TrimSpace，这样 "export: X" 和 "export:X" 都能认。
func parseExportMarkers(doc *ast.CommentGroup) (names []string, typos []string) {
	if doc == nil {
		return nil, nil
	}
	for _, line := range strings.Split(doc.Text(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if rest, ok := strings.CutPrefix(line, exportMarker); ok {
			if name := strings.TrimSpace(rest); name != "" {
				names = append(names, name)
			}
			continue
		}
		// 大小写写错（Export:、EXPORT:）不该被静默忽略——
		// 那会让方法悄悄不算导出函数，正是这个工具要拦的那类问题
		if looksLikeMarker(line) {
			typos = append(typos, line)
		}
	}
	return names, typos
}

// looksLikeMarker 判断一行像不像写错大小写的 export 标记。
func looksLikeMarker(line string) bool {
	i := strings.Index(line, ":")
	if i <= 0 {
		return false
	}
	head := strings.TrimSpace(line[:i+1])
	return head != exportMarker && strings.EqualFold(head, exportMarker)
}
