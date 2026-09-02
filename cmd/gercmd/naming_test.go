package main

import (
	"flag"
	"slices"
	"testing"
)

// TestNamingDefaultsCoverBothTemplates 默认值必须同时覆盖两支项目模板。
//
// gercmd 服务两支：游戏服务器照 GameSvr（只有 **_mod.go / **Mod），
// Web 服务器照 noteserver（还有 **_mgr.go / **Mgr）。默认值只照顾一支的话，
// 另一支每条命令都得带参数，那种工具没人愿意用。
func TestNamingDefaultsCoverBothTemplates(t *testing.T) {
	n := defaultNaming()
	for _, f := range []string{"bag_mod.go", "hero_mod.go", "auth_mgr.go", "note_mod.go"} {
		if !n.IsModuleFile(f) {
			t.Errorf("默认约定应当认得 %s", f)
		}
	}
	for _, f := range []string{"player.go", "bag_export.go", "note_type.go"} {
		if n.IsModuleFile(f) {
			t.Errorf("%s 不是模块文件，不该被认作模块", f)
		}
	}
}

// TestWithDefaultsIsPerField 只改一项时，其余项要退回默认而不是把这一项也吞掉。
//
// 这条对应一个真实的 bug：早先的实现是"整个 Naming 是零值才用默认"，于是
// 只传 -mod-suffix 时 TypeSuffixes 仍是零值，整体被默认值替换，
// 用户刚设的 mod-suffix 一起没了——表现就是"参数传了却完全不生效"。
func TestWithDefaultsIsPerField(t *testing.T) {
	got := Naming{ModFileSuffixes: []string{"_service"}}.withDefaults()

	if !slices.Equal(got.ModFileSuffixes, []string{"_service"}) {
		t.Errorf("显式设的字段被覆盖了: %v", got.ModFileSuffixes)
	}
	def := defaultNaming()
	if !slices.Equal(got.TypeSuffixes, def.TypeSuffixes) {
		t.Errorf("没设的字段应当退回默认: %v", got.TypeSuffixes)
	}
	if got.ExportSuffix != def.ExportSuffix || got.CtorPrefix != def.CtorPrefix {
		t.Errorf("没设的字段应当退回默认: %+v", got)
	}
}

// TestTypeCandidates 一个输入可能对应多个类型名，都要给出来。
func TestTypeCandidates(t *testing.T) {
	n := defaultNaming()
	for _, c := range []struct {
		in   string
		want []string
	}{
		// 只给业务名：两个后缀都是候选，由调用方去文件里找哪个存在
		{"auth", []string{"AuthMod", "AuthMgr"}},
		{"bag", []string{"BagMod", "BagMgr"}},
		// 写全了：写的那个排第一
		{"AuthMgr", []string{"AuthMgr", "AuthMod"}},
		{"BagMod", []string{"BagMod", "BagMgr"}},
		// 小写后缀也认，且不会变成 BagModMod
		{"bagmod", []string{"BagMod", "BagMgr"}},
		{"", nil},
	} {
		got := n.TypeCandidates(c.in)
		if !slices.Equal(got, c.want) {
			t.Errorf("TypeCandidates(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestTypeCandidatesCustomSuffix 换一套类型后缀，候选跟着换。
func TestTypeCandidatesCustomSuffix(t *testing.T) {
	n := Naming{TypeSuffixes: []string{"Service"}}.withDefaults()
	got := n.TypeCandidates("shop")
	if !slices.Equal(got, []string{"ShopService"}) {
		t.Errorf("got %v", got)
	}
	if name := n.CtorName("ShopService"); name != "NewShopService" {
		t.Errorf("CtorName = %q", name)
	}
	if trimmed := n.TrimTypeSuffix("ShopService"); trimmed != "Shop" {
		t.Errorf("TrimTypeSuffix = %q", trimmed)
	}
}

// TestNamingFlags 参数要真的能改到约定上。
//
// 这几个参数存在的全部意义就是"第三支约定不同的项目不必改工具"，
// 所以走一遍 flag 解析而不是直接构造结构体。
func TestNamingFlags(t *testing.T) {
	var n Naming
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	n.bind(fs)
	if err := fs.Parse([]string{
		"-mod-suffix", "_service, _job",
		"-type-suffix", "Service,Job",
		"-export-suffix", "_api",
		"-ctor-prefix", "Make",
	}); err != nil {
		t.Fatal(err)
	}
	n = n.withDefaults()

	if !slices.Equal(n.ModFileSuffixes, []string{"_service", "_job"}) {
		t.Errorf("ModFileSuffixes = %v（逗号后的空格要被吃掉）", n.ModFileSuffixes)
	}
	if !slices.Equal(n.TypeSuffixes, []string{"Service", "Job"}) {
		t.Errorf("TypeSuffixes = %v", n.TypeSuffixes)
	}
	if got := n.ExportFileName("shop_service.go"); got != "shop_api.go" {
		t.Errorf("ExportFileName = %q, want shop_api.go", got)
	}
	if got := n.ExportGlob(); got != "*_api.go" {
		t.Errorf("ExportGlob = %q", got)
	}
	if got := n.CtorName("ShopService"); got != "MakeShopService" {
		t.Errorf("CtorName = %q", got)
	}
}

// TestModFilePattern 提示信息里的样式要跟着配置走，不能写死 **_mod.go。
func TestModFilePattern(t *testing.T) {
	if got := defaultNaming().ModFilePattern(); got != "**_mod.go / **_mgr.go" {
		t.Errorf("got %q", got)
	}
	n := Naming{ModFileSuffixes: []string{"_service"}}.withDefaults()
	if got := n.ModFilePattern(); got != "**_service.go" {
		t.Errorf("got %q", got)
	}
}
