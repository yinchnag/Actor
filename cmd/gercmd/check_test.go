package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixture 在临时目录里造一个包和一个源文件。
// 内容只需语法正确即可——检查器只解析 AST，不做类型检查、也不编译。
func fixture(t *testing.T, pkgDir, file, src string) string {
	t.Helper()
	root := t.TempDir()
	writeFileAt(t, root, pkgDir+"/"+file, []byte(src))
	return root
}

func mustCheck(t *testing.T, root, name string) *ModuleCheck {
	t.Helper()
	res, err := CheckModule(root, name, Options{})
	if err != nil {
		t.Fatalf("CheckModule 失败: %v", err)
	}
	return res
}

// 一份完全合规的模块，其它用例在它基础上改坏某一处。
const goodSrc = `package bag

import (
	"actor"
	"actor/cmd/GameSvr/player"
)

func NewBagMod(p *player.PlayerEnt) actor.IModule {
	return &BagMod{host: p}
}

type BagMod struct {
	host *player.PlayerEnt
	actor.ModObj[*BagMod]
}

func (that *BagMod) AddItem(itemid, count int) {}
`

func TestCheckAllPass(t *testing.T) {
	res := mustCheck(t, fixture(t, "bag", "bag_mod.go", goodSrc), "bag")
	if !res.AllOK() {
		t.Fatalf("合规模块应当三项全过: struct=%v embed=%v ctor=%v",
			res.Struct, res.Embed, res.Ctor)
	}
	for _, it := range []CheckItem{res.Struct, res.Embed, res.Ctor} {
		if it.Where == "" {
			t.Fatal("通过的检查项应当带上位置")
		}
	}
}

// TestCheckWrongTypeParam 是这个命令最值钱的一条。
//
// type BagMod struct{ actor.ModObj[*HeroMod] } 编译得过、不报错，
// 但 heir 指针会反推到别的类型上，模块名变成空串。运行期表现为
// "这个模块的功能就是没反应"，而框架层只有一句 module not found。
func TestCheckWrongTypeParam(t *testing.T) {
	src := strings.Replace(goodSrc, "actor.ModObj[*BagMod]", "actor.ModObj[*HeroMod]", 1)
	res := mustCheck(t, fixture(t, "bag", "bag_mod.go", src), "bag")

	if !res.Struct.OK {
		t.Fatal("struct 本身是在的")
	}
	if res.Embed.OK {
		t.Fatal("类型参数写错了，内嵌检查不该通过")
	}
	if !strings.Contains(res.Embed.Detail, "*HeroMod") ||
		!strings.Contains(res.Embed.Detail, "*BagMod") {
		t.Fatalf("错误信息应当同时点明实际值和期望值: %q", res.Embed.Detail)
	}
	if res.Embed.Where == "" {
		t.Fatal("应当给出出错位置")
	}
}

// TestCheckMissingEmbed 完全没有内嵌 ModObj。
func TestCheckMissingEmbed(t *testing.T) {
	src := strings.Replace(goodSrc, "\tactor.ModObj[*BagMod]\n", "", 1)
	res := mustCheck(t, fixture(t, "bag", "bag_mod.go", src), "bag")

	if !res.Struct.OK || res.Embed.OK {
		t.Fatalf("struct 在但没内嵌: struct=%v embed=%v", res.Struct.OK, res.Embed.OK)
	}
	if !strings.Contains(res.Embed.Detail, "没有内嵌") {
		t.Fatalf("错误信息没说清: %q", res.Embed.Detail)
	}
}

// TestCheckNamedFieldNotEmbed 具名字段不算内嵌——写成 mod actor.ModObj[*BagMod]
// 的话反射拿不到宿主指针，和没写一样。
func TestCheckNamedFieldNotEmbed(t *testing.T) {
	src := strings.Replace(goodSrc,
		"\tactor.ModObj[*BagMod]", "\tmod actor.ModObj[*BagMod]", 1)
	res := mustCheck(t, fixture(t, "bag", "bag_mod.go", src), "bag")
	if res.Embed.OK {
		t.Fatal("具名字段不该被当成内嵌")
	}
}

// TestCheckCtorWrongReturn 构造函数返回具体类型而不是 actor.IModule。
func TestCheckCtorWrongReturn(t *testing.T) {
	src := strings.Replace(goodSrc,
		"func NewBagMod(p *player.PlayerEnt) actor.IModule",
		"func NewBagMod(p *player.PlayerEnt) *BagMod", 1)
	res := mustCheck(t, fixture(t, "bag", "bag_mod.go", src), "bag")

	if res.Ctor.OK {
		t.Fatal("返回类型不符合规范，不该通过")
	}
	if !strings.Contains(res.Ctor.Detail, "*BagMod") ||
		!strings.Contains(res.Ctor.Detail, "actor.IModule") {
		t.Fatalf("错误信息应当点明实际值与期望值: %q", res.Ctor.Detail)
	}
}

// TestCheckCtorTwoResults 多返回值也不合规。
// 注意 func f() (a, b int) 在 AST 里是一个 Field 带两个 Name，
// 只数 Field 会误判成 1 个返回值。
func TestCheckCtorTwoResults(t *testing.T) {
	src := strings.Replace(goodSrc,
		"func NewBagMod(p *player.PlayerEnt) actor.IModule",
		"func NewBagMod(p *player.PlayerEnt) (actor.IModule, error)", 1)
	res := mustCheck(t, fixture(t, "bag", "bag_mod.go", src), "bag")
	if res.Ctor.OK {
		t.Fatal("两个返回值不该通过")
	}
	if !strings.Contains(res.Ctor.Detail, "2 个返回值") {
		t.Fatalf("错误信息没点明返回值个数: %q", res.Ctor.Detail)
	}
}

// TestCheckMethodNotCtor 同名的方法不能被当成构造函数。
func TestCheckMethodNotCtor(t *testing.T) {
	src := strings.Replace(goodSrc,
		"func NewBagMod(p *player.PlayerEnt) actor.IModule",
		"func (x *X) NewBagMod(p *player.PlayerEnt) actor.IModule", 1)
	res := mustCheck(t, fixture(t, "bag", "bag_mod.go", src), "bag")
	if res.Ctor.OK {
		t.Fatal("方法不能算作构造函数")
	}
}

// TestCheckMissingStruct 模块压根不存在时，三项都不过且说明清楚。
func TestCheckMissingStruct(t *testing.T) {
	res := mustCheck(t, fixture(t, "bag", "bag_mod.go", goodSrc), "mail")
	if res.Struct.OK || res.Embed.OK || res.Ctor.OK {
		t.Fatal("不存在的模块不该有任何一项通过")
	}
	if res.TypeName != "MailMod" || res.CtorName != "NewMailMod" {
		t.Fatalf("名字推导不对: %s / %s", res.TypeName, res.CtorName)
	}
}

// TestCheckBareModObj 同包内直接写 ModObj[*X] 而不带包名，也应当认得。
// 框架自己的测试模块就是这么写的。
func TestCheckBareModObj(t *testing.T) {
	src := `package actor

type BagMod struct {
	ModObj[*BagMod]
}

func NewBagMod() IModule { return &BagMod{} }
`
	res := mustCheck(t, fixture(t, "actor", "mod.go", src), "bag")
	if !res.AllOK() {
		t.Fatalf("不带包名的写法也该通过: %+v", res)
	}
}

// TestCheckAliasedImport 包被起了别名时仍应通过，但要提醒一句。
func TestCheckAliasedImport(t *testing.T) {
	src := `package bag

import a "actor"

type BagMod struct {
	a.ModObj[*BagMod]
}

func NewBagMod() a.IModule { return &BagMod{} }
`
	res := mustCheck(t, fixture(t, "bag", "bag_mod.go", src), "bag")
	if !res.AllOK() {
		t.Fatalf("别名导入不该判为不合规: %+v", res)
	}
	if len(res.Warns) == 0 {
		t.Fatal("用了别名应当提醒一句，免得看的人以为写错了")
	}
}

// TestCheckDuplicateStruct 两个包各定义一个同名模块。
//
// 框架按类型名注册模块，两个 BagMod 会抢同一个 key，
// AddModule 时后挂的直接覆盖先挂的，而且一声不吭。
func TestCheckDuplicateStruct(t *testing.T) {
	root := fixture(t, "bag", "bag_mod.go", goodSrc)
	dup := strings.Replace(goodSrc, "package bag", "package other", 1)
	writeFileAt(t, root, "other/dup.go", []byte(dup))

	res := mustCheck(t, root, "bag")
	if len(res.Dups) == 0 {
		t.Fatal("同名 struct 应当被报出来")
	}
}

// TestCheckParseErrorWarned 语法错的文件要提醒，不能让"没找到"和"没解析成"混为一谈。
func TestCheckParseErrorWarned(t *testing.T) {
	root := fixture(t, "bag", "broken.go", "package bag\n\nfunc ( {{{ 语法错\n")
	res := mustCheck(t, root, "bag")
	if len(res.Warns) == 0 {
		t.Fatal("解析失败应当出现在警告里")
	}
	if !strings.Contains(strings.Join(res.Warns, "\n"), "broken.go") {
		t.Fatalf("警告里应当指出是哪个文件: %v", res.Warns)
	}
}

func TestNormalizeModName(t *testing.T) {
	for in, want := range map[string]string{
		"bag":     "BagMod",
		"Bag":     "BagMod",
		"bagMod":  "BagMod",
		"BagMod":  "BagMod",
		"hero":    "HeroMod",
		"mailBox": "MailBoxMod",
		"  bag  ": "BagMod",
		"":        "",
	} {
		if got := NormalizeModName(in); got != want {
			t.Errorf("NormalizeModName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCheckRealGameSvr 拿仓库里真实的 GameSvr 模块跑一遍——
// 规范校验器要是对自家规范的样板都判错，那就没意义了。
func TestCheckRealGameSvr(t *testing.T) {
	root := filepath.Join("..", "GameSvr")
	if _, err := os.Stat(root); err != nil {
		t.Skipf("找不到 %s，跳过: %v", root, err)
	}
	for _, name := range []string{"bag", "hero"} {
		res := mustCheck(t, root, name)
		if !res.AllOK() {
			t.Errorf("%s 应当合规: struct=%+v embed=%+v ctor=%+v",
				name, res.Struct, res.Embed, res.Ctor)
		}
	}
}
