package main

import (
	"fmt"
	"go/ast"
	"strings"
)

// param 是渲染好的一个参数。
type param struct {
	Name string // 传给 ModInvoke 时用的名字
	Type string // 在输出包里可用的类型写法
}

// buildFacadeFunc 把一个模块方法分析成模板要的数据。
//
// 这里只做语义：类型能不能在输出包里用、参数名会不会跟生成代码撞车、
// 可变参数要不要拒。至于拼成什么样子，交给模板。
//
// fnName 是门面函数名（如 BagAddItem），modType 是模块类型名（如 BagMod）——
// 后者要原样作为 ModInvoke 的第一个参数，因为框架就是按类型名注册模块的。
func buildFacadeFunc(fd *ast.FuncDecl, modType, fnName string, r *typeRenderer) (FacadeFunc, error) {
	// 顺序不能反：参数定名要先知道返回值占了哪些标识符
	results, err := renderResults(fd.Type.Results, r)
	if err != nil {
		return FacadeFunc{}, err
	}
	params, err := renderParams(fd.Type.Params, r, reservedNames(results))
	if err != nil {
		return FacadeFunc{}, err
	}

	f := FacadeFunc{
		Name:    fnName,
		Method:  fd.Name.Name,
		ModType: modType,
		Doc:     methodDocLines(fd),
	}
	for _, p := range params {
		f.Params = append(f.Params, FacadeParam{Name: p.Name, Type: p.Type})
	}
	for i, t := range results {
		f.Results = append(f.Results, FacadeResult{
			Var:   fmt.Sprintf("r%d", i),
			Type:  t,
			Index: i,
		})
	}
	return f, nil
}

// methodDocLines 取模块方法的注释，原样搬到门面上，但去掉 export: 那行——
// 那是给生成器看的指令，不是给读代码的人看的。
//
// 剔除之后还要修掉尾部的空注释行。原因是 gofmt（Go 1.19 起的文档注释规范化）
// 会把缩进的 "//\texport: X" 当作代码块，在它前面补一个空的 "//"：
//
//	// 增加物品
//	//              ← gofmt 补的
//	//	export: X
//
// 只删标记行的话，搬过去的注释就会以一个孤零零的 // 收尾，很难看。
func methodDocLines(fd *ast.FuncDecl) []string {
	if fd.Doc == nil {
		return nil
	}
	var out []string
	for _, c := range fd.Doc.List {
		line := strings.TrimSpace(strings.TrimPrefix(c.Text, "//"))
		if strings.HasPrefix(line, exportMarker) {
			continue
		}
		out = append(out, c.Text)
	}
	for len(out) > 0 {
		last := strings.TrimSpace(strings.TrimPrefix(out[len(out)-1], "//"))
		if last != "" {
			break
		}
		out = out[:len(out)-1]
	}
	return out
}

// reservedNames 收集生成的函数体里已经占用的标识符。
//
// 函数体固定会用到 that / ret / err 和 r0…rN，参数名撞上任何一个都会出事：
//
//	参数叫 ret  → "ret, err := ..." 给一个类型不符的变量赋值，编译不过
//	参数叫 r0   → 与返回值零值变量重名，编译不过
//	参数叫 int  → 遮蔽了内建类型，后面 "var r0 int" 直接失效
//
// 最后一类最阴，光看签名根本看不出来。所以类型串里出现过的标识符
// （int、string、time、error…）也一并算作占用，宁可多改几个参数名。
//
// 注意这份名单跟模板是绑定的：模板里换了变量名（比如把 ret 改成 out），
// 这里也要跟着改，否则消毒就漏了。
func reservedNames(results []string) map[string]bool {
	used := map[string]bool{"that": true, "ret": true, "err": true}
	for i := range results {
		used[fmt.Sprintf("r%d", i)] = true
	}
	for _, t := range results {
		for _, id := range identifiersIn(t) {
			used[id] = true
		}
	}
	return used
}

// identifiersIn 从渲染好的类型串里粗略地抠出标识符。
// 宁可多抠（多改几个参数名无害），也不能漏（漏一个就是编译错误）。
func identifiersIn(s string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, ch := range s {
		if ch == '_' || ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' ||
			ch >= '0' && ch <= '9' {
			cur.WriteRune(ch)
			continue
		}
		flush()
	}
	flush()
	return out
}

// renderParams 渲染参数列表，并给每个参数配一个在生成代码里安全的名字。
//
// 分两趟：先渲染全部类型（类型与名字无关），据此把类型里的标识符也纳入占用，
// 再逐个定名。顺序反过来的话就没法知道哪些名字会跟类型撞车。
func renderParams(fl *ast.FieldList, r *typeRenderer, reserved map[string]bool) ([]param, error) {
	if fl == nil {
		return nil, nil
	}

	// 第一趟：只渲染类型
	types := make([]string, 0, len(fl.List))
	for _, f := range fl.List {
		// 可变参数直接拒掉：ModObj.Invoke 用的是 reflect.Value.Call 而不是 CallSlice，
		// 可变参数的模块方法一调必 panic，而 handleTask 收到 panic 会关掉整个 actor。
		// 生成一个"能编译但一调就把玩家踢下线"的门面，比不生成糟得多。
		if _, isEllipsis := f.Type.(*ast.Ellipsis); isEllipsis {
			return nil, fmt.Errorf("有可变参数。框架的反射调用不支持它——" +
				"调用时会 panic 并连带关闭整个 actor，请改成切片参数")
		}
		typ, err := r.render(f.Type)
		if err != nil {
			return nil, err
		}
		types = append(types, typ)
		for _, id := range identifiersIn(typ) {
			reserved[id] = true
		}
	}

	// 第二趟：定名
	var out []param
	idx := 0
	nextName := func() string {
		for {
			n := fmt.Sprintf("p%d", idx)
			idx++
			if !reserved[n] {
				reserved[n] = true
				return n
			}
		}
	}
	for i, f := range fl.List {
		typ := types[i]
		if len(f.Names) == 0 {
			// 匿名参数：签名里没名字就没法往下传，给它编一个
			out = append(out, param{Name: nextName(), Type: typ})
			continue
		}
		for _, n := range f.Names {
			name := n.Name
			// 空白名传不出去；撞上生成代码里的标识符会编译不过，都得换掉
			if name == "_" || reserved[name] {
				name = nextName()
			} else {
				reserved[name] = true
			}
			out = append(out, param{Name: name, Type: typ})
		}
	}
	return out, nil
}

// renderResults 渲染返回值类型列表。返回值名字用不上，只要类型。
func renderResults(fl *ast.FieldList, r *typeRenderer) ([]string, error) {
	if fl == nil {
		return nil, nil
	}
	var out []string
	for _, f := range fl.List {
		typ, err := r.render(f.Type)
		if err != nil {
			return nil, err
		}
		n := len(f.Names)
		if n == 0 {
			n = 1
		}
		for i := 0; i < n; i++ {
			out = append(out, typ)
		}
	}
	return out, nil
}
