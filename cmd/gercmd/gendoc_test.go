package main

import (
	"strings"
	"testing"
)

// TestGenDocAfterGofmt 模块文件被 gofmt 处理过之后，注释仍要搬得干净。
//
// Go 1.19 起 gofmt 会把缩进的 "//\texport: X" 当成文档里的代码块，
// 在它前面补一个空的 "//"。只删标记行的话，搬到门面上的注释会以孤零零的
// // 收尾。这条用例用 gofmt 之后的形态跑一遍，确保结果干净。
func TestGenDocAfterGofmt(t *testing.T) {
	// 注意这里比原始写法多了一行空的 //，就是 gofmt 会补上的那行
	src := "package bag\n\nimport \"actor\"\n\n" +
		"type BagMod struct{ actor.ModObj[*BagMod] }\n\n" +
		"// 增加物品\n" +
		"//\n" +
		"//\texport: BagAddItem\n" +
		"func (that *BagMod) AddItem(id int) {}\n"

	res := mustGen(t, src, GenOptions{DryRun: true})
	out := string(res.Content)

	if !strings.Contains(out, "// 增加物品\nfunc (that *PlayerEnt) BagAddItem(id int) {") {
		t.Fatalf("注释没有搬干净（尾部残留空注释行？）:\n%s", out)
	}
	// 不能只查 "export:"——生成文件头部那句"改动请改模块方法上的 export: 标记"
	// 里就有它。要查的是标记连同它的值有没有被搬过来。
	if strings.Contains(out, "export: BagAddItem") {
		t.Fatalf("export: 标记不该被搬到门面上:\n%s", out)
	}
}

// TestGenDocOnlyMarker 注释里只有标记时，门面上不该留下任何注释。
func TestGenDocOnlyMarker(t *testing.T) {
	src := "package bag\n\nimport \"actor\"\n\n" +
		"type BagMod struct{ actor.ModObj[*BagMod] }\n\n" +
		"//\texport: BagAddItem\n" +
		"func (that *BagMod) AddItem(id int) {}\n"

	res := mustGen(t, src, GenOptions{DryRun: true})
	out := string(res.Content)
	if strings.Contains(out, "//\nfunc") || strings.Contains(out, "// \nfunc") {
		t.Fatalf("不该留下空注释行:\n%s", out)
	}
	if !strings.Contains(out, "\nfunc (that *PlayerEnt) BagAddItem(id int) {") {
		t.Fatalf("函数没生成对:\n%s", out)
	}
}
