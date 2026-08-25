package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

// 校验的是 GameSvr 的模块规范。一个合规的模块必须同时满足三条：
//
//	type BagMod struct {
//	    actor.ModObj[*BagMod]          // ← 类型参数必须是自己的指针
//	}
//	func NewBagMod(...) actor.IModule  // ← 返回接口
//
// 第二条最值得查：类型参数写成别的类型（如 ModObj[*HeroMod]）编译得过、
// 也不报错，但 heir 指针反推到错的对象上，模块名会是空串，
// 运行期表现为"这个模块的功能就是没反应"，极难排查。

// CheckItem 是单项检查的结果。
type CheckItem struct {
	OK     bool
	Where  string // file:line，找到时才有
	Detail string // 说明找到了什么、差在哪
}

// ModuleCheck 是一个模块的完整检查结果。
type ModuleCheck struct {
	Input    string // 用户输入的原始名字，如 bag
	TypeName string // 规范化后的类型名，如 BagMod
	CtorName string // 构造函数名，如 NewBagMod

	Struct CheckItem // 有没有这个 struct
	Embed  CheckItem // 有没有内嵌 ModObj[*自己]
	Ctor   CheckItem // 有没有 New<X>(...) actor.IModule

	// Methods 是模块上的公有方法，按发现顺序排列。
	// 带 export: 标记的属于导出函数，会通过 player 包的门面对外暴露。
	Methods []ModuleMethod

	Dups  []string // 同名 struct 的其它定义位置
	Warns []string // 解析失败之类，不影响结论但要让人知道
}

// Exports 返回其中被标记为导出函数的那些。
func (c *ModuleCheck) Exports() []ModuleMethod {
	var out []ModuleMethod
	for _, m := range c.Methods {
		if m.Exported {
			out = append(out, m)
		}
	}
	return out
}

// AllOK 三项全过才算合规。
func (c *ModuleCheck) AllOK() bool {
	return c.Struct.OK && c.Embed.OK && c.Ctor.OK
}

// NormalizeModName 把用户给的名字规范成模块类型名。
// bag / Bag / bagMod / BagMod 都归一到 BagMod，输入宽松、检查精确。
func NormalizeModName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// 先削掉已有的 Mod 后缀，免得 BagMod 变成 BagModMod
	for _, suffix := range []string{"Mod", "mod"} {
		if len(s) > len(suffix) && strings.HasSuffix(s, suffix) {
			s = s[:len(s)-len(suffix)]
			break
		}
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r) + "Mod"
}

// CheckModule 在 root 下递归查找 .go 文件，检查指定模块是否符合规范。
func CheckModule(root, input string, opt Options) (*ModuleCheck, error) {
	typeName := NormalizeModName(input)
	if typeName == "" {
		return nil, fmt.Errorf("模块名不能为空")
	}
	res := &ModuleCheck{
		Input:    input,
		TypeName: typeName,
		CtorName: "New" + typeName,
	}
	res.Struct.Detail = fmt.Sprintf("没有找到 struct %s", typeName)
	res.Embed.Detail = "struct 都没找到，无从检查"
	res.Ctor.Detail = fmt.Sprintf("没有找到函数 %s", res.CtorName)

	if err := mustBeDir(root); err != nil {
		return nil, err
	}

	// 复用 dirs/files 那套遍历：隐藏目录剪枝、读不动只跳过并告警，
	// 三个子命令的行为因此天然一致，不会各走各的。
	fset := token.NewFileSet()
	var warns strings.Builder
	err := walkTree(root, Options{Recursive: true, SkipHidden: opt.SkipHidden}, &warns,
		func(e walkEntry) error {
			if !e.D.Type().IsRegular() || !hasGoSuffix(e.D.Name()) {
				return nil
			}
			// SkipObjectResolution：只看语法结构，不做标识符解析，快很多。
			// ParseComments 必须显式加上——不加的话所有 Doc 都是 nil，
			// 方法上的 export: 标记根本读不到。
			f, perr := parser.ParseFile(fset, e.Path, nil,
				parser.SkipObjectResolution|parser.ParseComments)
			if perr != nil {
				// 单个文件语法错不该让整趟检查失败，但必须说出来——
				// 否则"没找到"到底是真没有还是没解析成，用户分不清
				res.Warns = append(res.Warns, fmt.Sprintf("解析 %s 失败: %v", e.Path, perr))
				return nil
			}
			inspectFile(f, fset, res)
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("遍历 %s 失败: %w", root, err)
	}
	// walkTree 的跳过告警也并进结果，免得被静默吞掉
	for _, line := range strings.Split(strings.TrimSpace(warns.String()), "\n") {
		if line != "" {
			res.Warns = append(res.Warns, line)
		}
	}
	return res, nil
}

func inspectFile(f *ast.File, fset *token.FileSet, res *ModuleCheck) {
	ast.Inspect(f, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.TypeSpec:
			if x.Name.Name == res.TypeName {
				checkStruct(x, fset, res)
			}
		case *ast.FuncDecl:
			if x.Recv == nil {
				// 构造函数必须是普通函数，不能是方法
				if x.Name.Name == res.CtorName {
					checkCtor(x, fset, res)
				}
				return true
			}
			// 挂在本模块类型上的方法，收集起来并读它的 export: 标记
			if receiverTypeName(x) == res.TypeName {
				collectMethod(x, fset, res)
			}
		}
		return true
	})
}

func checkStruct(ts *ast.TypeSpec, fset *token.FileSet, res *ModuleCheck) {
	where := pos(fset, ts.Pos())
	st, ok := ts.Type.(*ast.StructType)
	if !ok {
		res.Struct.Detail = fmt.Sprintf("%s 存在但不是 struct（%s）", res.TypeName, where)
		return
	}
	if res.Struct.OK {
		// 多个包各定义一个同名 struct：框架按类型名注册模块，
		// 两个 BagMod 会抢同一个 key，AddModule 时后来的直接覆盖前面的
		res.Dups = append(res.Dups, where)
		return
	}
	res.Struct.OK = true
	res.Struct.Where = where
	res.Struct.Detail = ""

	// 找内嵌的 ModObj
	res.Embed.Detail = fmt.Sprintf("%s 没有内嵌 ModObj", res.TypeName)
	for _, fld := range st.Fields.List {
		if len(fld.Names) != 0 {
			continue // 具名字段不是内嵌
		}
		qualifier, arg, ok := modObjTypeArg(fld.Type)
		if !ok {
			continue
		}
		fldWhere := pos(fset, fld.Pos())
		want := "*" + res.TypeName
		got := exprString(arg)
		if got != want {
			// 这就是最值钱的那条检查
			res.Embed.Where = fldWhere
			res.Embed.Detail = fmt.Sprintf(
				"内嵌的是 ModObj[%s]，类型参数应为 %s——"+
					"写错会让 heir 反推到别的类型上，模块名变成空串且不报错", got, want)
			return
		}
		res.Embed.OK = true
		res.Embed.Where = fldWhere
		res.Embed.Detail = ""
		if qualifier != "" && qualifier != "actor" {
			res.Warns = append(res.Warns,
				fmt.Sprintf("%s 处用的是 %s.ModObj 而不是 actor.ModObj（包别名？）", fldWhere, qualifier))
		}
		return
	}
}

// collectMethod 记下模块的一个公有方法，并读取它的 export: 标记。
//
// 只收公有方法：框架的反射注册只认公开方法，私有方法压根进不了调用表。
// 但私有方法上如果带了标记，那是个必须点破的错误——写的人以为它对外了，
// 实际上永远不会被调用到。
func collectMethod(fd *ast.FuncDecl, fset *token.FileSet, res *ModuleCheck) {
	where := pos(fset, fd.Pos())
	names, typos := parseExportMarkers(fd.Doc)

	for _, t := range typos {
		res.Warns = append(res.Warns, fmt.Sprintf(
			"%s 的注释 %q 像是写错大小写的 export: 标记，当前不会生效", where, t))
	}

	if !ast.IsExported(fd.Name.Name) {
		if len(names) > 0 {
			res.Warns = append(res.Warns, fmt.Sprintf(
				"%s 的 %s 是私有方法却带了 export: 标记——框架只反射公有方法，它永远不会被调用到",
				where, fd.Name.Name))
		}
		return
	}

	m := ModuleMethod{Name: fd.Name.Name, Where: where}
	if len(names) > 0 {
		m.Exported = true
		m.ExportName = names[0]
		if len(names) > 1 {
			// 一个方法只对应一个门面函数，多写的那些不会有任何效果
			m.Extra = names[1:]
			res.Warns = append(res.Warns, fmt.Sprintf(
				"%s 的 %s 上有 %d 个 export: 标记，只有第一个 %q 生效",
				where, m.Name, len(names), m.ExportName))
		}
		// 两个方法抢同一个门面名字，门面层只可能留下一个
		for _, prev := range res.Methods {
			if prev.Exported && prev.ExportName == m.ExportName {
				res.Warns = append(res.Warns, fmt.Sprintf(
					"%s 与 %s 都声明导出为 %q，门面层只能有一个", prev.Name, m.Name, m.ExportName))
			}
		}
	}
	res.Methods = append(res.Methods, m)
}

func checkCtor(fd *ast.FuncDecl, fset *token.FileSet, res *ModuleCheck) {
	where := pos(fset, fd.Pos())
	if res.Ctor.OK {
		return // 已经找到合规的了
	}
	res.Ctor.Where = where

	n := resultCount(fd.Type.Results)
	if n != 1 {
		res.Ctor.Detail = fmt.Sprintf("%s 有 %d 个返回值，规范要求恰好一个 actor.IModule", res.CtorName, n)
		return
	}
	ret := fd.Type.Results.List[0].Type
	qualifier, ok := iModuleQualifier(ret)
	if !ok {
		res.Ctor.Detail = fmt.Sprintf("%s 返回 %s，规范要求 actor.IModule",
			res.CtorName, exprString(ret))
		return
	}
	res.Ctor.OK = true
	res.Ctor.Detail = ""
	// 与内嵌检查保持同一口径：只认类型名，包名允许被起别名，但提醒一句
	if qualifier != "" && qualifier != "actor" {
		res.Warns = append(res.Warns,
			fmt.Sprintf("%s 处用的是 %s.IModule 而不是 actor.IModule（包别名？）", where, qualifier))
	}
}

// iModuleQualifier 认出 IModule 这个返回类型，返回它的包限定名。
// 与 modObjTypeArg 一样只锁类型名不锁包名——导入被起别名是合法写法。
func iModuleQualifier(e ast.Expr) (qualifier string, ok bool) {
	switch x := e.(type) {
	case *ast.SelectorExpr: // actor.IModule
		if x.Sel.Name != "IModule" {
			return "", false
		}
		if id, isIdent := x.X.(*ast.Ident); isIdent {
			qualifier = id.Name
		}
		return qualifier, true
	case *ast.Ident: // 同包内直接写 IModule
		return "", x.Name == "IModule"
	}
	return "", false
}

// modObjTypeArg 认出 ModObj[T] 这种内嵌，返回包限定名与类型实参。
//
// 单个类型参数在 AST 里是 IndexExpr；多个才是 IndexListExpr，
// 而 ModObj 只有一个参数，所以只处理前者。
func modObjTypeArg(e ast.Expr) (qualifier string, arg ast.Expr, ok bool) {
	ix, isIndex := e.(*ast.IndexExpr)
	if !isIndex {
		return "", nil, false
	}
	switch x := ix.X.(type) {
	case *ast.SelectorExpr: // actor.ModObj[...]
		if x.Sel.Name != "ModObj" {
			return "", nil, false
		}
		if id, isIdent := x.X.(*ast.Ident); isIdent {
			qualifier = id.Name
		}
	case *ast.Ident: // 同包内直接写 ModObj[...]
		if x.Name != "ModObj" {
			return "", nil, false
		}
	default:
		return "", nil, false
	}
	return qualifier, ix.Index, true
}

// resultCount 数真实的返回值个数。
// func f() (a, b int) 在 AST 里是一个 Field 带两个 Name，不能只数 Field。
func resultCount(fl *ast.FieldList) int {
	if fl == nil {
		return 0
	}
	n := 0
	for _, f := range fl.List {
		if len(f.Names) == 0 {
			n++
		} else {
			n += len(f.Names)
		}
	}
	return n
}

// exprString 把类型表达式还原成源码写法，用于错误信息与比对。
// 只覆盖类型里会出现的几种节点，够用即可。
func exprString(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.StarExpr:
		return "*" + exprString(x.X)
	case *ast.SelectorExpr:
		return exprString(x.X) + "." + x.Sel.Name
	case *ast.IndexExpr:
		return exprString(x.X) + "[" + exprString(x.Index) + "]"
	case *ast.ArrayType:
		return "[]" + exprString(x.Elt)
	case *ast.InterfaceType:
		return "interface{...}"
	default:
		return fmt.Sprintf("%T", e)
	}
}

func pos(fset *token.FileSet, p token.Pos) string {
	pp := fset.Position(p)
	return fmt.Sprintf("%s:%d", filepath.ToSlash(pp.Filename), pp.Line)
}

// runCheck 是 check 子命令的入口。
//
// 结论走 stdout（方便 grep 或接管道），过程说明与告警走 stderr。
// 三项全过退出码 0，任一项不过退出码 1——这样才能直接放进 CI 当闸门。
func runCheck(args []string) int {
	var opt Options
	c := newSubcmd("check", "<模块名> [路径]",
		"模块名给 bag / Bag / BagMod 都行，会归一到 BagMod。\n"+
			"不给路径时在当前目录下递归查找。")
	c.fs.BoolVar(&opt.SkipHidden, "skip-hidden", false, "跳过以 . 开头的目录与文件")
	pos, code := c.parse(args, 1, 2)
	if code != exitOK {
		return code
	}
	root := argOr(pos, 1, ".")

	res, err := CheckModule(root, pos[0], opt)
	if err != nil {
		return fail(err)
	}
	res.Report(os.Stdout, os.Stderr, root)
	if !res.AllOK() {
		return exitFail
	}
	return exitOK
}

// Report 把检查结果写出来。结论走 out，仅供参考的告警走 warn。
func (c *ModuleCheck) Report(out, warn io.Writer, root string) {
	fmt.Fprintf(out, "检查 %s（来自 %q），范围 %s\n\n", c.TypeName, c.Input, root)

	pass := 0
	for _, it := range []struct {
		title string
		item  CheckItem
	}{
		{fmt.Sprintf("struct %s", c.TypeName), c.Struct},
		{fmt.Sprintf("内嵌 actor.ModObj[*%s]", c.TypeName), c.Embed},
		{fmt.Sprintf("func %s(...) actor.IModule", c.CtorName), c.Ctor},
	} {
		mark := "✗"
		if it.item.OK {
			mark = "✓"
			pass++
		}
		fmt.Fprintf(out, "  %s  %s %s\n", mark, padRight(it.title, 36), it.item.Where)
		if !it.item.OK && it.item.Detail != "" {
			fmt.Fprintf(out, "     %s\n", it.item.Detail)
		}
	}

	// 公有方法与导出标记。这部分是信息不是判定：没标 export: 只说明它是
	// 模块内部方法，不算违规，所以不计入上面的 3 项。
	if len(c.Methods) > 0 {
		fmt.Fprintf(out, "\n公有方法 %d 个，其中导出函数 %d 个（→ 表示导出，· 表示模块内部方法）:\n",
			len(c.Methods), len(c.Exports()))
		for _, m := range c.Methods {
			mark, target := "·", "—"
			if m.Exported {
				mark, target = "→", m.ExportName
			}
			fmt.Fprintf(out, "  %s  %s %s %s\n",
				mark, padRight(m.Name, 16), padRight(target, 20), m.Where)
		}
	}

	for _, d := range c.Dups {
		fmt.Fprintf(out, "  !  另一处同名定义 %s\n", d)
	}
	if len(c.Dups) > 0 {
		fmt.Fprint(out, "     框架按类型名注册模块，同名的会在 AddModule 时互相覆盖\n")
	}
	for _, w := range c.Warns {
		fmt.Fprintf(warn, "注意: %s\n", w)
	}

	fmt.Fprintf(out, "\n%d/3 通过\n", pass)
}
