package main

import (
	"fmt"
	"go/ast"
	"strings"
)

// 生成门面函数时最容易翻车的地方是类型：函数签名要原样搬到 player 包里，
// 而那边看得见什么类型和模块包完全不同。这个文件负责把 AST 里的类型表达式
// 渲染成能在输出包里编译通过的源码写法，渲染不了就明确报错，
// 绝不生成"看着像对、编译不过"的代码。

// builtinTypes 是预声明标识符，在哪个包里都能直接用。
var builtinTypes = map[string]bool{
	"bool": true, "string": true, "error": true, "any": true, "rune": true, "byte": true,
	"int": true, "int8": true, "int16": true, "int32": true, "int64": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true, "uintptr": true,
	"float32": true, "float64": true, "complex64": true, "complex128": true,
}

// typeRenderer 把类型表达式渲染成输出包里可用的源码。
type typeRenderer struct {
	// imports 是模块文件里的「包名 → 导入路径」。
	imports map[string]string
	// outPkg 是输出文件所在的包名。指向这个包的限定符要去掉——
	// 生成的代码就在这个包里，写 player.PlayerEnt 反而编不过。
	outPkg string
	// needed 记录实际用到的导入，生成 import 块时用。
	needed map[string]string
}

func newTypeRenderer(imports map[string]string, outPkg string) *typeRenderer {
	return &typeRenderer{
		imports: imports,
		outPkg:  outPkg,
		needed:  map[string]string{},
	}
}

// render 渲染类型表达式。返回的字符串可以直接写进输出包的源码。
func (r *typeRenderer) render(e ast.Expr) (string, error) {
	switch x := e.(type) {
	case *ast.Ident:
		if builtinTypes[x.Name] {
			return x.Name, nil
		}
		// 不带包名的自定义类型 = 模块包自己声明的类型。
		// 门面在 player 包里，要用它就得 import 模块包，而模块包又 import 了 player——
		// 直接成环。这是架构决定的硬限制，只能让人改签名。
		return "", fmt.Errorf("类型 %s 声明在模块自己的包里，"+
			"门面无法引用它（player 导入模块包会与模块包导入 player 成环）——"+
			"请把参数/返回值换成基础类型，或移到公共包", x.Name)

	case *ast.SelectorExpr:
		pkg, ok := x.X.(*ast.Ident)
		if !ok {
			return "", fmt.Errorf("看不懂的限定类型 %s", exprString(e))
		}
		// 指向输出包自己的限定符要去掉
		if pkg.Name == r.outPkg {
			return x.Sel.Name, nil
		}
		path, known := r.imports[pkg.Name]
		if !known {
			return "", fmt.Errorf("类型 %s.%s 用的包没在模块文件里导入，无法确定导入路径",
				pkg.Name, x.Sel.Name)
		}
		r.needed[pkg.Name] = path
		return pkg.Name + "." + x.Sel.Name, nil

	case *ast.StarExpr:
		inner, err := r.render(x.X)
		if err != nil {
			return "", err
		}
		return "*" + inner, nil

	case *ast.ArrayType:
		inner, err := r.render(x.Elt)
		if err != nil {
			return "", err
		}
		if x.Len == nil {
			return "[]" + inner, nil
		}
		return "[" + exprString(x.Len) + "]" + inner, nil

	case *ast.MapType:
		k, err := r.render(x.Key)
		if err != nil {
			return "", err
		}
		v, err := r.render(x.Value)
		if err != nil {
			return "", err
		}
		return "map[" + k + "]" + v, nil

	case *ast.ChanType:
		inner, err := r.render(x.Value)
		if err != nil {
			return "", err
		}
		switch {
		case x.Dir == ast.SEND:
			return "chan<- " + inner, nil
		case x.Dir == ast.RECV:
			return "<-chan " + inner, nil
		}
		return "chan " + inner, nil

	case *ast.FuncType:
		return r.renderFuncType(x)

	case *ast.InterfaceType:
		if x.Methods == nil || len(x.Methods.List) == 0 {
			return "any", nil
		}
		return "", fmt.Errorf("暂不支持内联的非空接口类型")

	case *ast.Ellipsis:
		// 调用方会先拦掉可变参数，走到这里说明漏了
		return "", fmt.Errorf("可变参数不能出现在这里")

	default:
		return "", fmt.Errorf("暂不支持的类型写法 %s", exprString(e))
	}
}

// renderFuncType 渲染函数类型，如回调参数 cb func(int) error。
func (r *typeRenderer) renderFuncType(ft *ast.FuncType) (string, error) {
	params, err := r.renderTypeList(ft.Params)
	if err != nil {
		return "", err
	}
	results, err := r.renderTypeList(ft.Results)
	if err != nil {
		return "", err
	}
	s := "func(" + strings.Join(params, ", ") + ")"
	switch len(results) {
	case 0:
	case 1:
		s += " " + results[0]
	default:
		s += " (" + strings.Join(results, ", ") + ")"
	}
	return s, nil
}

// renderTypeList 只渲染类型、丢掉参数名，用于函数类型的签名。
func (r *typeRenderer) renderTypeList(fl *ast.FieldList) ([]string, error) {
	if fl == nil {
		return nil, nil
	}
	var out []string
	for _, f := range fl.List {
		s, err := r.render(f.Type)
		if err != nil {
			return nil, err
		}
		// 一个 Field 带多个名字表示多个同类型参数，要展开成对应份数
		n := len(f.Names)
		if n == 0 {
			n = 1
		}
		for i := 0; i < n; i++ {
			out = append(out, s)
		}
	}
	return out, nil
}

// fileImports 收集文件的「包名 → 导入路径」。
// 带别名的按别名记，因为源码里就是用别名引用的。
func fileImports(f *ast.File) map[string]string {
	out := map[string]string{}
	for _, im := range f.Imports {
		path := strings.Trim(im.Path.Value, `"`)
		name := path
		if i := strings.LastIndex(path, "/"); i >= 0 {
			name = path[i+1:]
		}
		if im.Name != nil {
			name = im.Name.Name
		}
		if name == "_" || name == "." {
			continue // 空导入和点导入都不会用来限定类型
		}
		out[name] = path
	}
	return out
}
