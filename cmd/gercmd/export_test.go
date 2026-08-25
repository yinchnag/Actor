package main

import (
	"strings"
	"testing"
)

// exportSrc 是一份带各种 export: 写法的模块，用来把标记解析的边界钉死。
const exportSrc = `package bag

import "actor"

func NewBagMod() actor.IModule { return &BagMod{} }

type BagMod struct {
	actor.ModObj[*BagMod]
}

// 增加物品
//	export: BagAddItem
func (that *BagMod) AddItem(itemid, count int) {}

// 没有标记，属于模块内部方法
func (that *BagMod) RemoveItem(itemid int) {}

// 冒号后面不留空格也要认
//export:BagUseItem
func (that *BagMod) UseItem(itemid int) {}

// 完全没有 doc 注释
func (that *BagMod) Count() int { return 0 }

// 值接收者的方法一样要收
//	export: BagPeek
func (that BagMod) Peek() int { return 0 }

// 私有方法：框架反射不到它，标了也没用
//	export: BagSecret
func (that *BagMod) secret() {}
`

func exportsOf(t *testing.T, src string) *ModuleCheck {
	t.Helper()
	return mustCheck(t, fixture(t, "bag", "bag_mod.go", src), "bag")
}

// TestExportMarkerBasics 标记的识别与不识别。
func TestExportMarkerBasics(t *testing.T) {
	res := exportsOf(t, exportSrc)

	// 私有方法不进列表
	want := map[string]string{
		"AddItem":    "BagAddItem",
		"RemoveItem": "", // 没标记
		"UseItem":    "BagUseItem",
		"Count":      "", // 没 doc
		"Peek":       "BagPeek",
	}
	if len(res.Methods) != len(want) {
		var got []string
		for _, m := range res.Methods {
			got = append(got, m.Name)
		}
		t.Fatalf("公有方法应当有 %d 个, got %d: %v", len(want), len(res.Methods), got)
	}
	for _, m := range res.Methods {
		exp, ok := want[m.Name]
		if !ok {
			t.Errorf("多出了方法 %s", m.Name)
			continue
		}
		if m.ExportName != exp {
			t.Errorf("%s 的导出名 = %q, want %q", m.Name, m.ExportName, exp)
		}
		if m.Exported != (exp != "") {
			t.Errorf("%s 的导出标志 = %v, want %v", m.Name, m.Exported, exp != "")
		}
		if m.Where == "" {
			t.Errorf("%s 缺少位置信息", m.Name)
		}
	}

	if n := len(res.Exports()); n != 3 {
		t.Fatalf("导出函数应当有 3 个, got %d", n)
	}
}

// TestExportMarkerOnPrivateMethod 私有方法带标记必须报出来。
//
// 框架的反射注册只认公有方法，所以这种写法是纯粹的误会：
// 写的人以为它对外了，实际上永远不会被调用到，而且一声不吭。
func TestExportMarkerOnPrivateMethod(t *testing.T) {
	res := exportsOf(t, exportSrc)
	for _, m := range res.Methods {
		if m.Name == "secret" {
			t.Fatal("私有方法不该进公有方法列表")
		}
	}
	if !strings.Contains(strings.Join(res.Warns, "\n"), "私有方法") {
		t.Fatalf("私有方法带 export: 标记应当告警: %v", res.Warns)
	}
}

// TestExportMarkerCaseTypo 大小写写错的标记不该被静默忽略。
//
// 静默忽略的后果是这个方法悄悄不算导出函数——正是这个工具存在的意义所在。
func TestExportMarkerCaseTypo(t *testing.T) {
	src := strings.Replace(exportSrc, "//\texport: BagAddItem", "//\tExport: BagAddItem", 1)
	res := exportsOf(t, src)

	for _, m := range res.Methods {
		if m.Name == "AddItem" && m.Exported {
			t.Fatal("大小写不对的标记不该生效")
		}
	}
	if !strings.Contains(strings.Join(res.Warns, "\n"), "大小写") {
		t.Fatalf("应当提示疑似写错大小写: %v", res.Warns)
	}
}

// TestExportMarkerDuplicateOnOneMethod 一个方法写了多个标记，只有第一个生效。
func TestExportMarkerDuplicateOnOneMethod(t *testing.T) {
	src := strings.Replace(exportSrc,
		"//\texport: BagAddItem",
		"//\texport: BagAddItem\n//\texport: BagAddItem2", 1)
	res := exportsOf(t, src)

	for _, m := range res.Methods {
		if m.Name != "AddItem" {
			continue
		}
		if m.ExportName != "BagAddItem" {
			t.Fatalf("应当取第一个标记, got %q", m.ExportName)
		}
		if len(m.Extra) != 1 || m.Extra[0] != "BagAddItem2" {
			t.Fatalf("多余的标记应当记下来, got %v", m.Extra)
		}
	}
	if !strings.Contains(strings.Join(res.Warns, "\n"), "只有第一个") {
		t.Fatalf("应当提示多余标记: %v", res.Warns)
	}
}

// TestExportNameCollision 两个方法抢同一个门面名字。
// 门面层只能有一个同名函数，不点破的话必然有一个悄悄失效。
func TestExportNameCollision(t *testing.T) {
	src := strings.Replace(exportSrc, "//export:BagUseItem", "//export:BagAddItem", 1)
	res := exportsOf(t, src)
	if !strings.Contains(strings.Join(res.Warns, "\n"), "都声明导出为") {
		t.Fatalf("重名的导出应当告警: %v", res.Warns)
	}
}

// TestExportMarkerNeedsParseComments 是对实现的一条护栏。
//
// go/parser 默认丢弃注释，不显式加 ParseComments 的话所有 Doc 都是 nil，
// 标记一个也读不到。这个用例保证那个 flag 不会在将来被误删。
func TestExportMarkerNeedsParseComments(t *testing.T) {
	res := exportsOf(t, exportSrc)
	if len(res.Exports()) == 0 {
		t.Fatal("一个导出标记都没读到——多半是 parser.ParseComments 丢了")
	}
}

// TestExportMethodsOfOtherTypeIgnored 别的类型上的方法不能算进来。
func TestExportMethodsOfOtherTypeIgnored(t *testing.T) {
	src := exportSrc + `
// 另一个类型的方法，不该出现在 BagMod 的方法表里
//	export: HeroAddHero
func (that *HeroMod) AddHero(id int) {}
`
	res := exportsOf(t, src)
	for _, m := range res.Methods {
		if m.Name == "AddHero" {
			t.Fatal("HeroMod 的方法混进 BagMod 的列表了")
		}
	}
}

// TestExportRealGameSvr 拿仓库里真实的模块跑一遍。
//
// 只断言不变量，不钉死导出的具体个数——GameSvr 是活的代码，
// 随手给某个方法加个 export: 标记就让工具的测试挂掉，那是测试的问题。
func TestExportRealGameSvr(t *testing.T) {
	res, err := CheckModule("../GameSvr", "bag", Options{})
	if err != nil {
		t.Skipf("找不到 GameSvr，跳过: %v", err)
	}
	if !res.AllOK() {
		t.Fatalf("bag 模块本身应当合规: %+v", res)
	}

	byName := map[string]string{}
	for _, m := range res.Exports() {
		if m.ExportName == "" {
			t.Errorf("%s 标了导出却没有名字", m.Name)
		}
		byName[m.Name] = m.ExportName
	}
	// AddItem 是规范文档里举的例子，它必须一直对
	if got := byName["AddItem"]; got != "BagAddItem" {
		t.Fatalf("AddItem 的导出名 = %q, want BagAddItem（当前导出: %v）", got, byName)
	}
}
