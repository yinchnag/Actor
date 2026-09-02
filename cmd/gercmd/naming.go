package main

import (
	"flag"
	"strings"
	"unicode"
)

// Naming 是模块相关的命名约定。
//
// gercmd 同时服务两支项目模板——游戏服务器照 cmd/GameSvr、Web 服务器照
// cmd/noteserver，它们的约定不完全一样（前者只有 **_mod.go / **Mod，
// 后者还有挂在服务器上的 **_mgr.go / **Mgr），以后还会有第三支。
//
// 所以约定不散落在各处的字符串字面量里，而是集中在这里，并且**全部开成命令行
// 参数**。默认值取两支模板的并集，于是它们都不必传参；约定不同的项目传参即可，
// 不用改工具。
type Naming struct {
	// ModFileSuffixes 模块文件的后缀，如 _mod、_mgr（不含 .go）。
	ModFileSuffixes []string
	// TypeSuffixes 模块类型名的后缀，如 Mod、Mgr。
	TypeSuffixes []string
	// ExportSuffix 生成物文件的后缀，如 _export（不含 .go）。
	ExportSuffix string
	// CtorPrefix 构造函数的前缀，如 New。
	CtorPrefix string
}

// defaultNaming 返回两支模板的并集。
func defaultNaming() Naming {
	return Naming{
		ModFileSuffixes: []string{"_mod", "_mgr"},
		TypeSuffixes:    []string{"Mod", "Mgr"},
		ExportSuffix:    "_export",
		CtorPrefix:      "New",
	}
}

// withDefaults 逐字段补上默认值。
//
// 必须逐字段补，不能"整个 Naming 是零值才用默认"——用户很可能只改一项
// （比如只给 -mod-suffix），那时其余字段仍是零值，整体替换会把他刚设的那项
// 也一起丢掉。这个错犯过一次：-mod-suffix 传了却完全不生效。
func (that Naming) withDefaults() Naming {
	def := defaultNaming()
	if len(that.ModFileSuffixes) == 0 {
		that.ModFileSuffixes = def.ModFileSuffixes
	}
	if len(that.TypeSuffixes) == 0 {
		that.TypeSuffixes = def.TypeSuffixes
	}
	if that.ExportSuffix == "" {
		that.ExportSuffix = def.ExportSuffix
	}
	if that.CtorPrefix == "" {
		that.CtorPrefix = def.CtorPrefix
	}
	return that
}

// ModFilePattern 用于提示信息：把后缀列表写成 **_mod.go / **_mgr.go 的样子。
func (that Naming) ModFilePattern() string {
	parts := make([]string, 0, len(that.ModFileSuffixes))
	for _, s := range that.ModFileSuffixes {
		parts = append(parts, "**"+s+".go")
	}
	return strings.Join(parts, " / ")
}

// bind 把约定注册成命令行参数。
//
// 用逗号分隔的字符串而不是可重复的 flag：约定是一整套，一次写全比分几次追加
// 更不容易漏，也更容易在 //go:generate 那一行里看清楚。
func (that *Naming) bind(fs *flag.FlagSet) {
	def := defaultNaming()
	fs.Func("mod-suffix",
		"模块文件后缀，逗号分隔（默认 "+strings.Join(def.ModFileSuffixes, ",")+"）",
		func(v string) error {
			that.ModFileSuffixes = splitList(v)
			return nil
		})
	fs.Func("type-suffix",
		"模块类型名后缀，逗号分隔（默认 "+strings.Join(def.TypeSuffixes, ",")+"）",
		func(v string) error {
			that.TypeSuffixes = splitList(v)
			return nil
		})
	fs.StringVar(&that.ExportSuffix, "export-suffix", def.ExportSuffix, "生成物文件后缀")
	fs.StringVar(&that.CtorPrefix, "ctor-prefix", def.CtorPrefix, "构造函数前缀")
}

// splitList 拆逗号分隔的列表，忽略空项。
func splitList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// IsModuleFile 判断一个文件名是不是模块入口文件。
func (that Naming) IsModuleFile(name string) bool {
	stem, ok := strings.CutSuffix(name, ".go")
	if !ok {
		return false
	}
	for _, s := range that.ModFileSuffixes {
		if strings.HasSuffix(stem, s) {
			return true
		}
	}
	return false
}

// ExportFileName 由模块文件名推出门面文件名：剥掉末尾的模块后缀，再加 ExportSuffix。
//
//	bag_mod.go   → bag_export.go
//	auth_mgr.go  → auth_export.go
//
// 生成物里不带 mod/mgr 是有意的：那个后缀描述的是"模块挂在谁身上"，而门面没有
// 这个区别——它们都是挂在同一个宿主上的普通方法，文件名里留着只会让人以为
// 那是两类东西。
//
// 按**段**剥离而不是替换子串。早先的实现是把名字里最后一个 "mod" 换成 "export"，
// 于是 bagmod.go 会变成 bagexport.go、mod_thing.go 会变成 export_thing.go——
// 那类结果没人能预测。不符合命名约定的文件一律退化成加后缀，至少是可预期的。
func (that Naming) ExportFileName(base string) string {
	stem := strings.TrimSuffix(base, ".go")
	for _, s := range that.ModFileSuffixes {
		if trimmed, ok := strings.CutSuffix(stem, s); ok {
			return trimmed + that.ExportSuffix + ".go"
		}
	}
	return stem + that.ExportSuffix + ".go"
}

// ExportGlob 返回匹配生成物的 glob 模式，用于找残留。
func (that Naming) ExportGlob() string { return "*" + that.ExportSuffix + ".go" }

// TypeCandidates 把用户输入的模块名归一成候选类型名。
//
// 一个输入可能对应多个类型：check auth 既可能指 AuthMod，也可能指 AuthMgr——
// 光看名字分不出来，所以两个都返回，由调用方去文件里找哪个真的存在。
// 输入本身就带后缀时（check AuthMgr），那一个排在最前。
func (that Naming) TypeCandidates(input string) []string {
	s := strings.TrimSpace(input)
	if s == "" {
		return nil
	}

	// 先削掉已有的后缀，免得 BagMod 变成 BagModMod。
	// 记住削掉的是哪个，好让它排第一——用户既然写全了，多半就是指它。
	base, matched := s, ""
	for _, suffix := range that.TypeSuffixes {
		for _, form := range []string{suffix, strings.ToLower(suffix)} {
			if len(s) > len(form) && strings.HasSuffix(s, form) {
				base, matched = s[:len(s)-len(form)], suffix
				break
			}
		}
		if matched != "" {
			break
		}
	}

	r := []rune(base)
	if len(r) == 0 {
		return nil
	}
	r[0] = unicode.ToUpper(r[0])
	base = string(r)

	out := make([]string, 0, len(that.TypeSuffixes))
	if matched != "" {
		out = append(out, base+matched)
	}
	for _, suffix := range that.TypeSuffixes {
		if suffix == matched {
			continue
		}
		out = append(out, base+suffix)
	}
	return out
}

// TrimTypeSuffix 剥掉类型名末尾的模块后缀：BagMod → Bag，AuthMgr → Auth。
// 门面函数名的默认前缀由它得出。
func (that Naming) TrimTypeSuffix(typeName string) string {
	for _, s := range that.TypeSuffixes {
		if trimmed, ok := strings.CutSuffix(typeName, s); ok {
			return trimmed
		}
	}
	return typeName
}

// CtorName 返回某个模块类型对应的构造函数名。
func (that Naming) CtorName(typeName string) string { return that.CtorPrefix + typeName }
