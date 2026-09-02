package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"

	gast "github.com/yinchnag/GCore/ast"
)

// 依据模块文件生成 player 包里的门面函数。
//
// 分工是刻意的：这个文件只算语义——哪些方法要生成、类型在输出包里怎么写、
// 参数名会不会跟生成代码撞车、需要哪些导入。最后长什么样交给模板
// （templates/export.tmpl，可用 -tmpl 换掉）。
//
// 这么分的好处是门面的形状不再焊死在 Go 代码里：想把 println 换成项目日志、
// 想加埋点、想让门面把 error 传出去，改模板就行，不用碰生成器也不用重新编译。

//go:embed templates/export.tmpl
var defaultTemplate string

// GenOptions 控制生成行为。
type GenOptions struct {
	// All 为所有公有方法生成；默认只为带 export: 标记的生成。
	All bool
	// Force 覆盖已存在的文件。默认不覆盖——门面文件里往往有手写代码。
	Force bool
	// DryRun 只把内容打到 stdout，不落盘。
	DryRun bool
	// TemplateFile 自定义模板路径，空则用内嵌的默认模板。
	TemplateFile string
	// Recv 门面方法的接收者类型名。
	Recv string
	// Naming 命名约定。零值表示用 defaultNaming()。
	Naming Naming
}

// SkippedMethod 是被跳过的方法及原因。
type SkippedMethod struct {
	Name   string
	Where  string
	Reason string
}

// GenResult 是一次生成的结果。
type GenResult struct {
	ModType string // 模块类型名，如 BagMod
	OutPath string // 输出文件路径
	OutPkg  string // 输出文件的包名
	Funcs   []string
	Skipped []SkippedMethod
	Content []byte
	Existed bool // 输出文件原先就存在
	Written bool // 这次是否真的写了盘
}

// defaultRecv 是门面方法的接收者类型的默认值，取自 GameSvr 那一支模板。
//
// 它只是个默认值，别当成规定：noteserver 那一支的宿主是 Hub，所有 //go:generate
// 里都显式写了 -recv。新项目照着自己的宿主传就行，不必改这里。
const defaultRecv = "PlayerEnt"

// naming 返回本次生成用的命名约定，零值时退回默认。
func (that GenOptions) naming() Naming { return that.Naming.withDefaults() }

// GenerateExports 读模块文件，为它的公有方法生成门面函数并写进 outDir。
func GenerateExports(modFile, outDir string, opt GenOptions) (*GenResult, error) {
	doc, err := gast.GetFileDoc(modFile)
	if err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %w", modFile, err)
	}

	modType, err := findModuleType(doc)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", modFile, err)
	}
	outPkg, err := outputPackage(outDir)
	if err != nil {
		return nil, err
	}
	tmpl, err := loadTemplate(opt.TemplateFile)
	if err != nil {
		return nil, err
	}

	recv := opt.Recv
	if recv == "" {
		recv = defaultRecv
	}
	res := &GenResult{
		ModType: modType,
		OutPath: filepath.Join(outDir, opt.naming().ExportFileName(filepath.Base(modFile))),
		OutPkg:  outPkg,
	}
	data := FacadeFile{
		Package: outPkg,
		Recv:    recv,
		ModType: modType,
		Source:  filepath.Base(modFile),
	}

	// 门面函数名的前缀：BagMod → Bag。用户写的 export: BagAddItem 就是这么来的。
	prefix := opt.naming().TrimTypeSuffix(modType)
	r := newTypeRenderer(fileImports(doc.File), outPkg)

	for i := range doc.Funcs {
		method := doc.Funcs[i]
		// FuncDoc.Recv 已剥掉指针与泛型实参，口径见 TestReceiverTypeName
		if method.Recv != modType || !method.Exported || method.Decl == nil {
			continue
		}
		where := posOf(method.Position())
		// 标记的识别依赖注释的原始行结构，所以取原始 CommentGroup 而非 FuncDoc.Doc
		names, _ := parseExportMarkers(method.Decl.Doc)

		if !opt.All && len(names) == 0 {
			res.Skipped = append(res.Skipped, SkippedMethod{
				Name: method.Name, Where: where,
				Reason: "没有 export: 标记（用 -all 可为所有公有方法生成）"})
			continue
		}
		fnName := prefix + method.Name
		if len(names) > 0 {
			fnName = names[0] // 标记里写了名字就以它为准
		}

		fn, err := buildFacadeFunc(method.Decl, modType, fnName, r)
		if err != nil {
			// 单个方法生成不了不影响其它方法，但原因必须说清
			res.Skipped = append(res.Skipped, SkippedMethod{
				Name: method.Name, Where: where, Reason: err.Error()})
			continue
		}
		data.Funcs = append(data.Funcs, fn)
		res.Funcs = append(res.Funcs, fnName)
	}

	if len(data.Funcs) == 0 {
		return res, nil // 没有可生成的，交给调用方决定怎么说
	}

	// 导入排序：不排的话每次生成顺序都不同，白白产生 diff
	for _, path := range r.needed {
		data.Imports = append(data.Imports, path)
	}
	sort.Strings(data.Imports)

	src, err := renderFile(tmpl, data)
	if err != nil {
		return nil, err
	}
	res.Content = src

	if _, statErr := os.Stat(res.OutPath); statErr == nil {
		res.Existed = true
	}
	if opt.DryRun {
		return res, nil
	}
	if res.Existed && !opt.Force {
		return res, fmt.Errorf("%s 已存在。门面文件里常有手写代码，不会自动覆盖——"+
			"确认要覆盖请加 -force，或先用 -n 看看要生成什么", res.OutPath)
	}
	if err := os.WriteFile(res.OutPath, src, 0o644); err != nil {
		return nil, fmt.Errorf("写 %s 失败: %w", res.OutPath, err)
	}
	res.Written = true
	return res, nil
}

// loadTemplate 加载模板：给了路径就用文件里的，否则用内嵌的默认模板。
func loadTemplate(path string) (*template.Template, error) {
	text, name := defaultTemplate, "export.tmpl(内嵌)"
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("读取模板 %s 失败: %w", path, err)
		}
		text, name = string(b), path
	}
	tmpl, err := template.New(name).Parse(text)
	if err != nil {
		return nil, fmt.Errorf("模板 %s 有语法错误: %w", name, err)
	}
	return tmpl, nil
}

// renderFile 渲染模板并交给 go/format 排版。
//
// 模板里的缩进不用讲究——排版全交给 gofmt，这是用模板做代码生成时最省心的一点。
// 反过来说，模板一旦写出语法不合法的 Go，会在这里被 format.Source 挡下，
// 所以出错信息里要带上原始产出，否则没法定位模板哪里写坏了。
func renderFile(tmpl *template.Template, data FacadeFile) ([]byte, error) {
	var b bytes.Buffer
	if err := tmpl.Execute(&b, data); err != nil {
		return nil, fmt.Errorf("模板 %s 执行失败: %w", tmpl.Name(), err)
	}
	out, err := format.Source(b.Bytes())
	if err != nil {
		return nil, fmt.Errorf("模板 %s 产出的不是合法 Go 代码: %w\n───── 原始产出 ─────\n%s",
			tmpl.Name(), err, b.String())
	}
	return out, nil
}

// findModuleType 找出文件里内嵌了 ModObj[*自己] 的那个 struct。
func findModuleType(doc *gast.FileDoc) (string, error) {
	var found []string
	for _, sd := range doc.Structs {
		for _, fld := range sd.Fields {
			if !fld.Embedded {
				continue // 具名字段不是内嵌
			}
			if _, arg, ok := modObjTypeArg(fld.TypeExpr); ok {
				if exprString(arg) == "*"+sd.Name {
					found = append(found, sd.Name)
				}
			}
		}
	}
	switch len(found) {
	case 0:
		return "", fmt.Errorf("没找到内嵌 ModObj[*自己] 的模块类型")
	case 1:
		return found[0], nil
	default:
		return "", fmt.Errorf("文件里有多个模块类型 %v，一个文件请只放一个", found)
	}
}

// outputPackage 取输出目录的包名：优先读该目录下已有的 .go 文件，
// 空目录则退回用目录名。读已有文件更可靠——目录名和包名不一定一致。
func outputPackage(outDir string) (string, error) {
	info, err := os.Stat(outDir)
	if err != nil {
		return "", fmt.Errorf("无法访问输出目录 %s: %w", outDir, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s 不是文件夹", outDir)
	}
	entries, err := os.ReadDir(outDir)
	if err != nil {
		return "", fmt.Errorf("读取 %s 失败: %w", outDir, err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !hasGoSuffix(e.Name()) {
			continue
		}
		f, perr := parser.ParseFile(fset, filepath.Join(outDir, e.Name()), nil, parser.PackageClauseOnly)
		if perr == nil && f.Name != nil {
			return f.Name.Name, nil
		}
	}
	return filepath.Base(filepath.Clean(outDir)), nil
}

// runGen 是 gen 子命令的入口。
func runGen(args []string) int {
	var opt GenOptions
	c := newSubcmd("gen", "<模块文件> <输出文件夹>",
		"读模块文件里的公有方法，在输出文件夹生成门面函数。\n"+
			"文件名把 mod 换成 export：bag_mod.go → bag_export.go。\n"+
			"输出形态由模板决定，默认模板见 cmd/gercmd/templates/export.tmpl。")
	c.fs.BoolVar(&opt.All, "all", false, "为所有公有方法生成，而不只是带 export: 标记的")
	c.fs.BoolVar(&opt.Force, "force", false, "覆盖已存在的输出文件")
	c.fs.BoolVar(&opt.DryRun, "n", false, "只打印生成内容，不写文件")
	c.fs.StringVar(&opt.TemplateFile, "tmpl", "", "自定义模板文件，默认用内嵌模板")
	c.fs.StringVar(&opt.Recv, "recv", defaultRecv, "门面方法的接收者类型名")
	opt.Naming.bind(c.fs)
	pos, code := c.parse(args, 2, 2)
	if code != exitOK {
		return code
	}

	res, err := GenerateExports(pos[0], pos[1], opt)
	if err != nil {
		return fail(err)
	}

	// 跳过的方法要说清原因，否则用户只会看到"少生成了几个"却不知道为什么
	for _, s := range res.Skipped {
		fmt.Fprintf(os.Stderr, "跳过 %s（%s）: %s\n", s.Name, s.Where, s.Reason)
	}
	if len(res.Funcs) == 0 {
		fmt.Fprintf(os.Stderr, "%s 里没有需要生成的方法，未产生文件\n", res.ModType)
		return exitOK
	}

	if opt.DryRun {
		os.Stdout.Write(res.Content)
		fmt.Fprintf(os.Stderr, "\n（-n 预览，未写文件）%s 将生成 %d 个门面函数: %s\n",
			res.OutPath, len(res.Funcs), strings.Join(res.Funcs, ", "))
		return exitOK
	}
	action := "生成"
	if res.Existed {
		action = "覆盖"
	}
	fmt.Fprintf(os.Stderr, "%s %s，共 %d 个门面函数: %s\n",
		action, res.OutPath, len(res.Funcs), strings.Join(res.Funcs, ", "))
	return exitOK
}
