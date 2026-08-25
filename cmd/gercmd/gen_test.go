package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// genFixture 造一个可生成的场景：一个模块包 + 一个 player 包。
// 模块文件只会被解析、不会被编译，所以里面不需要真的能编译通过。
func genFixture(t *testing.T, modSrc string) (modFile, outDir string) {
	t.Helper()
	root := t.TempDir()
	modFile = writeFileAt(t, root, "bag/bag_mod.go", []byte(modSrc))
	outDir = mkdir(t, root, "player")
	writeFile(t, outDir, "player.go", []byte("package player\n"))
	return modFile, outDir
}

func mustGen(t *testing.T, modSrc string, opt GenOptions) *GenResult {
	t.Helper()
	modFile, outDir := genFixture(t, modSrc)
	res, err := GenerateExports(modFile, outDir, opt)
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	return res
}

const genModSrc = `package bag

import (
	"time"

	"actor"
)

func NewBagMod() actor.IModule { return &BagMod{} }

type BagMod struct {
	actor.ModObj[*BagMod]
}

// 增加物品
//	export: BagAddItem
func (that *BagMod) AddItem(itemid, count int) {}

//	export: BagCount
func (that *BagMod) Count(kind int) int { return 0 }

//	export: BagGetItem
func (that *BagMod) GetItem(id int) (int, bool, error) { return 0, false, nil }

//	export: BagComplex
func (that *BagMod) Complex(ids []int, m map[string][]byte, cb func(int) error, d time.Duration) (*int, []string) {
	return nil, nil
}

//	export: BagCollide
func (that *BagMod) Collide(ret int, err string, r0 bool) int { return 0 }

//	export: BagUnnamed
func (that *BagMod) Unnamed(int, string) int { return 0 }

// 没有 export: 标记
func (that *BagMod) Internal() {}

// 私有方法
func (that *BagMod) hidden() {}
`

// TestGenCompiles 是这个功能唯一不能省的测试：生成的代码必须真的能编译。
//
// 光看输出"像对的"没有意义——参数名遮蔽内建类型、返回值零值没声明、
// 类型渲染漏了包名，这几类问题肉眼都很难发现，但编译器一看一个准。
//
// 做法是造一个自带 go.mod 的临时模块，里面用桩件顶掉 PlayerEnt 与 loader，
// 这样既不依赖本仓库、也不会污染本仓库，跑完即弃。
func TestGenCompiles(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("找不到 go，跳过编译验证")
	}

	root := t.TempDir()
	modFile := writeFileAt(t, root, "src/bag/bag_mod.go", []byte(genModSrc))

	// 临时模块：只有 player 包，PlayerEnt 与 loader 都是桩件
	proj := mkdir(t, root, "proj")
	writeFile(t, proj, "go.mod", []byte("module gentest\n\ngo 1.22\n"))
	outDir := mkdir(t, proj, "player")
	writeFile(t, outDir, "player.go", []byte(`package player

import "reflect"

type modLoader interface {
	ModInvoke(mod, method string, args ...any) ([]reflect.Value, error)
}

type PlayerEnt struct{}

func (p *PlayerEnt) GetModloader() modLoader { return nil }
`))

	res, err := GenerateExports(modFile, outDir, GenOptions{})
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if len(res.Funcs) == 0 {
		t.Fatal("一个函数都没生成")
	}

	cmd := exec.Command("go", "build", "./...")
	cmd.Dir = proj
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("生成的代码编译不过: %v\n%s\n───── 生成内容 ─────\n%s",
			err, out, res.Content)
	}
	t.Logf("生成并编译通过 %d 个门面函数: %s", len(res.Funcs), strings.Join(res.Funcs, ", "))
}

// TestGenNoResultShape 无返回值的模板形状。
func TestGenNoResultShape(t *testing.T) {
	res := mustGen(t, genModSrc, GenOptions{})
	src := string(res.Content)

	want := `func (that *PlayerEnt) BagAddItem(itemid int, count int) {
	if _, err := that.GetModloader().ModInvoke("BagMod", "AddItem", itemid, count); err != nil {
		println("BagAddItem err:", err.Error())
	}
}`
	if !strings.Contains(src, want) {
		t.Fatalf("无返回值的模板不对，期望包含:\n%s\n实际:\n%s", want, src)
	}
}

// TestGenResultShape 有返回值的模板形状。
//
// 三处是草稿里没有、但缺了就出问题的：零值变量、个数校验、逗号 ok 断言。
func TestGenResultShape(t *testing.T) {
	res := mustGen(t, genModSrc, GenOptions{})
	src := string(res.Content)

	for _, want := range []string{
		"func (that *PlayerEnt) BagGetItem(id int) (int, bool, error) {",
		"\tvar r0 int\n\tvar r1 bool\n\tvar r2 error\n", // 零值必须提前声明
		"\tret, err := that.GetModloader().ModInvoke(\"BagMod\", \"GetItem\", id)\n",
		"\t\treturn r0, r1, r2\n",              // 失败路径也要有返回
		"\tif len(ret) != 3 {\n",               // 个数校验独立于 err
		"\tr0, _ = ret[0].Interface().(int)\n", // 逗号 ok，不会 panic
		"\tr2, _ = ret[2].Interface().(error)\n",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("缺少片段 %q\n实际:\n%s", want, src)
		}
	}
}

// TestGenParamNameCollision 参数名撞上生成代码里的标识符时必须改名。
//
// ret / err / r0 会与函数体里的变量冲突，直接编译不过；
// 参数叫 int 之类还会遮蔽内建类型，让 var r0 int 失效。
func TestGenParamNameCollision(t *testing.T) {
	res := mustGen(t, genModSrc, GenOptions{})
	src := string(res.Content)

	if !strings.Contains(src, "func (that *PlayerEnt) BagCollide(p0 int, p1 string, p2 bool) int {") {
		t.Fatalf("撞名的参数没有被改名:\n%s", src)
	}
	if !strings.Contains(src, `ModInvoke("BagMod", "Collide", p0, p1, p2)`) {
		t.Fatal("改名后没有把新名字传给 ModInvoke")
	}
}

// TestGenUnnamedParams 匿名参数要自动编名字，否则没法往下传。
func TestGenUnnamedParams(t *testing.T) {
	res := mustGen(t, genModSrc, GenOptions{})
	if !strings.Contains(string(res.Content),
		"func (that *PlayerEnt) BagUnnamed(p0 int, p1 string) int {") {
		t.Fatalf("匿名参数没被命名:\n%s", res.Content)
	}
}

// TestGenImportsCollected 用到外部包的类型时要带上导入。
func TestGenImportsCollected(t *testing.T) {
	src := string(mustGen(t, genModSrc, GenOptions{}).Content)
	if !strings.Contains(src, `"time"`) {
		t.Fatalf("用到 time.Duration 却没有导入 time:\n%s", src)
	}
	if !strings.Contains(src, "d time.Duration") {
		t.Fatal("time.Duration 的限定名渲染错了")
	}
}

// TestGenOnlyMarkedByDefault 默认只为带 export: 标记的方法生成。
func TestGenOnlyMarkedByDefault(t *testing.T) {
	res := mustGen(t, genModSrc, GenOptions{})
	if strings.Contains(string(res.Content), "Internal") {
		t.Fatal("没标记的方法不该被生成")
	}
	var skipped []string
	for _, s := range res.Skipped {
		skipped = append(skipped, s.Name)
	}
	if len(skipped) != 1 || skipped[0] != "Internal" {
		t.Fatalf("跳过的应当只有 Internal, got %v", skipped)
	}
}

// TestGenAllFlag -all 为所有公有方法生成，名字按 <前缀><方法名> 推导。
func TestGenAllFlag(t *testing.T) {
	res := mustGen(t, genModSrc, GenOptions{All: true})
	src := string(res.Content)
	if !strings.Contains(src, "func (that *PlayerEnt) BagInternal()") {
		t.Fatalf("-all 应当把 Internal 也生成为 BagInternal:\n%s", src)
	}
	if strings.Contains(src, "hidden") {
		t.Fatal("私有方法任何情况下都不该生成")
	}
}

// TestGenRejectsVariadic 可变参数必须拒掉。
//
// 框架用 reflect.Value.Call 而不是 CallSlice，可变参数方法一调必 panic，
// 而 panic 会连带关闭整个 actor。生成一个"能编译但一调就把玩家踢下线"的门面，
// 比不生成糟得多。
func TestGenRejectsVariadic(t *testing.T) {
	src := `package bag

import "actor"

type BagMod struct{ actor.ModObj[*BagMod] }

//	export: BagSum
func (that *BagMod) Sum(ids ...int) int { return 0 }

//	export: BagFine
func (that *BagMod) Fine(id int) int { return id }
`
	res := mustGen(t, src, GenOptions{})
	if strings.Contains(string(res.Content), "BagSum") {
		t.Fatal("可变参数的方法不该被生成")
	}
	if len(res.Funcs) != 1 || res.Funcs[0] != "BagFine" {
		t.Fatalf("其它合法方法应当照常生成, got %v", res.Funcs)
	}
	var reason string
	for _, s := range res.Skipped {
		if s.Name == "Sum" {
			reason = s.Reason
		}
	}
	if !strings.Contains(reason, "可变参数") || !strings.Contains(reason, "actor") {
		t.Fatalf("跳过原因应当说清可变参数会拖垮 actor: %q", reason)
	}
}

// TestGenRejectsLocalType 参数用了模块包自己声明的类型时必须拒掉。
//
// 门面在 player 包里，引用模块包的类型就得 import 模块包，
// 而模块包又 import 了 player——直接成环，生成出来也编译不过。
func TestGenRejectsLocalType(t *testing.T) {
	src := `package bag

import "actor"

type BagMod struct{ actor.ModObj[*BagMod] }

type Item struct{ ID int }

//	export: BagTake
func (that *BagMod) Take(it Item) {}
`
	res := mustGen(t, src, GenOptions{})
	if len(res.Funcs) != 0 {
		t.Fatalf("不该生成任何函数, got %v", res.Funcs)
	}
	if len(res.Skipped) != 1 || !strings.Contains(res.Skipped[0].Reason, "成环") {
		t.Fatalf("跳过原因应当点明导入成环: %+v", res.Skipped)
	}
}

// TestGenDoesNotOverwrite 默认不覆盖已存在的文件。
// 门面文件里往往有手写代码，静默覆盖等于毁掉别人的工作。
func TestGenDoesNotOverwrite(t *testing.T) {
	modFile, outDir := genFixture(t, genModSrc)
	existing := writeFile(t, outDir, "bag_export.go", []byte("package player\n// 手写的内容\n"))

	_, err := GenerateExports(modFile, outDir, GenOptions{})
	if err == nil {
		t.Fatal("已存在的文件不该被静默覆盖")
	}
	if !strings.Contains(err.Error(), "-force") {
		t.Fatalf("错误信息应当告诉用户怎么办: %v", err)
	}
	data, _ := os.ReadFile(existing)
	if !strings.Contains(string(data), "手写的内容") {
		t.Fatal("原文件被改动了")
	}

	// 加 -force 才覆盖
	if _, err := GenerateExports(modFile, outDir, GenOptions{Force: true}); err != nil {
		t.Fatalf("-force 应当允许覆盖: %v", err)
	}
	data, _ = os.ReadFile(existing)
	if strings.Contains(string(data), "手写的内容") {
		t.Fatal("-force 没有真的覆盖")
	}
}

// TestGenDryRunWritesNothing -n 只预览，不落盘。
func TestGenDryRunWritesNothing(t *testing.T) {
	modFile, outDir := genFixture(t, genModSrc)
	res, err := GenerateExports(modFile, outDir, GenOptions{DryRun: true})
	if err != nil {
		t.Fatalf("预览失败: %v", err)
	}
	if len(res.Content) == 0 {
		t.Fatal("预览应当有内容")
	}
	if res.Written {
		t.Fatal("预览不该写盘")
	}
	if _, err := os.Stat(res.OutPath); err == nil {
		t.Fatal("预览不该产生文件")
	}
}

func TestExportFileName(t *testing.T) {
	for in, want := range map[string]string{
		"bag_mod.go":   "bag_export.go",
		"hero_mod.go":  "hero_export.go",
		"mod.go":       "export.go",
		"bagmod.go":    "bagexport.go",
		"nomarker.go":  "nomarker_export.go",
		"mod_thing.go": "export_thing.go",
	} {
		if got := exportFileName(in); got != want {
			t.Errorf("exportFileName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestGenOutputIsStable 同样的输入两次生成，内容必须一字不差。
// 不稳定的话每次重新生成都会产生无意义的 diff。
func TestGenOutputIsStable(t *testing.T) {
	a := mustGen(t, genModSrc, GenOptions{DryRun: true})
	b := mustGen(t, genModSrc, GenOptions{DryRun: true})
	if string(a.Content) != string(b.Content) {
		t.Fatal("两次生成的内容不一致，多半是遍历 map 时没定序")
	}
}

// TestGenFindsModuleType 找不到模块类型时要明确报错，而不是生成一个空文件。
func TestGenFindsModuleType(t *testing.T) {
	modFile, outDir := genFixture(t, "package bag\n\ntype NotAMod struct{ X int }\n")
	_, err := GenerateExports(modFile, outDir, GenOptions{})
	if err == nil || !strings.Contains(err.Error(), "模块类型") {
		t.Fatalf("应当报告找不到模块类型, got %v", err)
	}
}

// TestGenOutPathUsesOutDir 输出文件必须落在指定文件夹里，文件名按 mod→export 换。
func TestGenOutPathUsesOutDir(t *testing.T) {
	res := mustGen(t, genModSrc, GenOptions{DryRun: true})
	if filepath.Base(res.OutPath) != "bag_export.go" {
		t.Fatalf("文件名不对: %s", res.OutPath)
	}
	if filepath.Base(filepath.Dir(res.OutPath)) != "player" {
		t.Fatalf("没落在指定文件夹里: %s", res.OutPath)
	}
	if res.OutPkg != "player" {
		t.Fatalf("包名应当取自输出目录已有文件, got %q", res.OutPkg)
	}
}
