package main

import (
	"fmt"
	"strings"
)

// 这里是模板能看到的全部数据。
//
// 分工：Go 这边算语义（类型能不能用、参数名会不会撞、导入要哪些），
// 模板那边只管排版。模板拿不到 AST，也不需要——它要的东西都已经算好了。
//
// 字段一旦公开就是给模板用的契约，改名会让别人手写的模板失效，
// templates/export.tmpl 顶部那份字段清单要同步更新。

// FacadeParam 是门面函数的一个参数。
type FacadeParam struct {
	// Name 是可以直接用的参数名。撞上生成代码里的标识符时已经改过名了。
	Name string
	// Type 是在输出包里可用的类型写法。
	Type string
}

// FacadeResult 是门面函数的一个返回值。
type FacadeResult struct {
	// Var 是承接它的局部变量名，r0、r1……
	Var string
	// Type 是返回值类型。
	Type string
	// Index 是它在 ModInvoke 返回切片里的下标。
	Index int
}

// FacadeFunc 是一个待生成的门面函数。
type FacadeFunc struct {
	// Name 是门面函数名，如 BagAddItem。
	Name string
	// Method 是模块方法名，如 AddItem。作为 ModInvoke 的第二个参数。
	Method string
	// ModType 是模块类型名，如 BagMod。作为 ModInvoke 的第一个参数——
	// 框架就是按类型名注册模块的。
	ModType string
	// Doc 是模块方法的注释原文（每行含 //），已剔除 export: 那行。
	Doc []string

	Params  []FacadeParam
	Results []FacadeResult
}

// FacadeFile 是整个待生成文件。
type FacadeFile struct {
	// Package 是输出包名，如 player。
	Package string
	// Recv 是门面方法的接收者类型，如 PlayerEnt。
	Recv string
	// ModType 是模块类型名。
	ModType string
	// Source 只放模块文件名不放路径——放全路径的话，
	// 用相对路径和绝对路径调用生成器会产出不同内容的文件。
	Source string
	// Imports 是需要的导入路径，已排序，重复生成才不会产生无谓的 diff。
	Imports []string
	Funcs   []FacadeFunc
}

// ── 下面是给模板用的便捷方法 ──
//
// 有了它们，模板里就不必写
//   {{range $i, $p := .Params}}{{if $i}}, {{end}}{{$p.Name}} {{$p.Type}}{{end}}
// 这种东西。拼接规则留在 Go 里，模板只负责能一眼看出形状的部分。
// 原始的 Params/Results 切片仍然公开，需要另做花样时可以自己 range。

// HasResults 是否有返回值。两种模板形态就靠它分叉。
func (f FacadeFunc) HasResults() bool { return len(f.Results) > 0 }

// DocText 是可以直接贴在函数上方的注释块，没有注释时为空串。
func (f FacadeFunc) DocText() string {
	if len(f.Doc) == 0 {
		return ""
	}
	return strings.Join(f.Doc, "\n") + "\n"
}

// SigParams 是签名里的参数部分，如 "itemid int, count int"。
func (f FacadeFunc) SigParams() string {
	parts := make([]string, 0, len(f.Params))
	for _, p := range f.Params {
		parts = append(parts, p.Name+" "+p.Type)
	}
	return strings.Join(parts, ", ")
}

// ResultSig 是签名里的返回值部分，含前导空格：
// 无返回值时为空串，单个时为 " int"，多个时为 " (int, bool, error)"。
func (f FacadeFunc) ResultSig() string {
	switch len(f.Results) {
	case 0:
		return ""
	case 1:
		return " " + f.Results[0].Type
	}
	types := make([]string, 0, len(f.Results))
	for _, r := range f.Results {
		types = append(types, r.Type)
	}
	return " (" + strings.Join(types, ", ") + ")"
}

// InvokeArgs 是传给 ModInvoke 的完整实参，如 `"BagMod", "AddItem", itemid, count`。
func (f FacadeFunc) InvokeArgs() string {
	parts := []string{fmt.Sprintf("%q", f.ModType), fmt.Sprintf("%q", f.Method)}
	for _, p := range f.Params {
		parts = append(parts, p.Name)
	}
	return strings.Join(parts, ", ")
}

// RetVars 是返回语句里的变量列表，如 "r0, r1, r2"。
func (f FacadeFunc) RetVars() string {
	vars := make([]string, 0, len(f.Results))
	for _, r := range f.Results {
		vars = append(vars, r.Var)
	}
	return strings.Join(vars, ", ")
}

// NumResults 返回值个数，模板里用来生成个数校验。
func (f FacadeFunc) NumResults() int { return len(f.Results) }
