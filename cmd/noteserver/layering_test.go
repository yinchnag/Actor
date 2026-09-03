package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestLayering 把 README 里的依赖规范变成能跑的断言。
//
// 分层规范写在文档里只是"希望"，写成测试才是"约束"。这一条挡住的是最常见的
// 一种腐化：某个包为了图方便反手引一个上层包，编译能过、功能也正常，
// 但依赖图就此成了一团麻，等到想拆分或复用时才发现动不了。
//
// 只管**项目内部**的包（noteserver/src/...）。第三方依赖不在此列——
// comm 不引入内部包，但它当然可以用标准库。
func TestLayering(t *testing.T) {
	// 每个包**允许**引入的内部包。列表之外一律算违规。
	// 空列表表示"谁都不能引"，也就是依赖图的底层。
	allowed := map[string][]string{
		// 整个项目的数据源。它是底层，谁都能用它、它不用任何人。
		// 注意它连 logs 都不能引——comm 里只有数据，没有会失败的逻辑，
		// 也就没有该记的日志。
		"comm": {},
		// 日志出口。它包着 GCore，本身不依赖任何内部包，所以谁都能引。
		"logs": {},
		// 只放接口与它们返回的错误哨兵。
		"contract": {"comm"},
		// 存储对象。再往上的东西（mods/service）它不该认识。
		"databases": {"comm", "contract", "logs"},

		// 以下三个不在规范的七个文件夹里，属于第 8 条允许的补充，
		// 但同样受依赖约束管——它们都在 HTTP 侧，不许碰 mods/service。
		"bases":      {"comm", "logs"},
		"security":   {"comm", "logs"},
		"config":     {},
		"middleware": {"bases", "comm", "contract", "logs"},

		// 逻辑模块。**绝不能引 service**：那会跟 service→mods 成环，
		// 也会让模块之间越过导出函数直接互调（见 README 第 7 条）。
		"mods/auth": {"comm", "contract", "logs"},
		"mods/mail": {"comm", "contract", "logs"},
		"mods/note": {"comm", "contract", "logs"},

		// 路由。它通过自己声明的小接口拿 Hub 的能力，所以也不引 service。
		"router/auth":   {"bases", "comm", "contract", "security", "middleware", "logs"},
		"router/note":   {"bases", "comm", "contract", "security", "middleware", "logs"},
		"router/health": {"bases", "comm", "contract", "security", "middleware", "logs"},
		"router/mail":   {"bases", "comm", "contract", "security", "middleware", "logs"},

		// 最上层的终端，可以引所有包。
		"service": {"comm", "contract", "databases",
			"mods/auth", "mods/mail", "mods/note",
			"bases", "security", "config", "middleware", "logs"},
	}

	const prefix = "noteserver/src/"
	fset := token.NewFileSet()

	err := filepath.WalkDir("src", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") {
			return err
		}
		if strings.HasSuffix(p, "_test.go") {
			return nil // 测试文件可以为了造脚手架多引几个包
		}

		pkg := path.Dir(filepath.ToSlash(strings.TrimPrefix(p, "src"+string(filepath.Separator))))
		want, known := allowed[pkg]
		if !known {
			t.Errorf("%s 所在的包 %q 不在分层表里——新建包时请同时在这里登记它的依赖约束", p, pkg)
			return nil
		}

		f, perr := parser.ParseFile(fset, p, nil, parser.ImportsOnly)
		if perr != nil {
			t.Errorf("解析 %s 失败: %v", p, perr)
			return nil
		}
		for _, imp := range f.Imports {
			ip := strings.Trim(imp.Path.Value, `"`)
			if !strings.HasPrefix(ip, prefix) {
				continue // 标准库与第三方不管
			}
			dep := strings.TrimPrefix(ip, prefix)
			if dep == pkg {
				continue
			}
			if !slices.Contains(want, dep) {
				t.Errorf("%s: %s 不允许引入 %s\n    允许的只有: %v",
					p, pkg, dep, want)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 src 失败: %v", err)
	}
}

// TestModFileNaming 一个功能模块文件夹里，**_mod.go 与 **_mgr.go 各最多一个。
//
// 规范第 5 条。同一种有两个意味着同一类宿主上挂了两个 ModObj，也就是两条跨
// goroutine 的通道进同一个功能——真要细分该用 **_imp.go 由 Mod/Mgr 持有。
//
// 但两种并存是合法的：邮件功能就是 mgr（管存储与下发，用户不在线也要能跑）
// 加 mod（管用户在线期间的信箱视图，读不出他自己那条协程）。
func TestModFileNaming(t *testing.T) {
	dirs, err := filepath.Glob(filepath.Join("src", "mods", "*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range dirs {
		files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		var mods, mgrs []string
		for _, f := range files {
			switch base := filepath.Base(f); {
			case strings.HasSuffix(base, "_mod.go"):
				mods = append(mods, base)
			case strings.HasSuffix(base, "_mgr.go"):
				mgrs = append(mgrs, base)
			}
		}
		// 每种入口最多一个，两种加起来至少一个。
		//
		// 一个功能同时有 mgr 和 mod 是合法的（邮件就是：mgr 管存储与下发，
		// mod 管用户在线期间的视图）。但同一种不能有两个——那意味着同一类
		// 宿主上挂了两个 ModObj，也就是两条跨 goroutine 的通道进同一个功能。
		// 真要细分该用 **_imp.go 由 Mod/Mgr 持有。
		if len(mods) > 1 {
			t.Errorf("%s 里有多个 **_mod.go %v——细分功能请用 **_imp.go 由 Mod 持有", dir, mods)
		}
		if len(mgrs) > 1 {
			t.Errorf("%s 里有多个 **_mgr.go %v——细分功能请用 **_imp.go 由 Mgr 持有", dir, mgrs)
		}
		if len(mods)+len(mgrs) == 0 {
			t.Errorf("%s 里没有 **_mod.go 或 **_mgr.go——功能模块文件夹必须有入口", dir)
		}
	}
}

// TestRouterFileNaming 每个路由文件夹至少有一个 **_rut.go。规范第 6 条。
func TestRouterFileNaming(t *testing.T) {
	dirs, err := filepath.Glob(filepath.Join("src", "router", "*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range dirs {
		files, _ := filepath.Glob(filepath.Join(dir, "*_rut.go"))
		if len(files) == 0 {
			t.Errorf("%s 里没有 **_rut.go——路由文件夹必须有一个", dir)
		}
	}
}

// TestCommFileNaming comm 下的文件一律 **_comm.go。规范第 1 条。
//
// 这条是视检发现的漏网之鱼（consts.go 少了后缀），所以补成测试。
// 规范里"常量数据无命名要求"说的是常量**标识符**可以随便取，
// 不是说文件名可以随便取——两件事，别混。
func TestCommFileNaming(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("src", "comm", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("src/comm 下一个 .go 都没有？")
	}
	for _, f := range files {
		base := filepath.Base(f)
		if strings.HasSuffix(base, "_test.go") {
			continue
		}
		if !strings.HasSuffix(base, "_comm.go") {
			t.Errorf("src/comm/%s 不符合规范——本包的文件一律以 _comm.go 结尾", base)
		}
	}
}

// TestCommStructsAreSnap comm 里导出的 struct 一律以 Snap 结尾。规范第 1 条。
//
// Snap 是个标记，含义是"这个值可以跨模块传递"。少了它，别的模块就无从判断
// 手上这个类型该不该往外传——而一旦传错，模块之间就开始共享可变状态，
// actor 的保证从那一刻起失效。所以这条不能靠自觉。
func TestCommStructsAreSnap(t *testing.T) {
	fset := token.NewFileSet()
	files, _ := filepath.Glob(filepath.Join("src", "comm", "*.go"))
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, f, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Errorf("解析 %s 失败: %v", f, err)
			continue
		}
		ast.Inspect(parsed, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			if _, isStruct := ts.Type.(*ast.StructType); !isStruct {
				return true // 只管 struct，类型别名之类不在此列
			}
			if !ts.Name.IsExported() {
				return true // 私有类型出不了这个包，管不着
			}
			if !strings.HasSuffix(ts.Name.Name, "Snap") {
				t.Errorf("%s: comm 里导出的 struct %s 必须以 Snap 结尾——"+
					"不带 Snap 的类型不该跨模块传递", f, ts.Name.Name)
			}
			return true
		})
	}
}

// TestModsInterfacesAreNotifyOnly mods 包里声明的接口，方法一律不许有返回值。
//
// 这条钉的是整套设计里最容易悄悄腐化的一点。模块要调别的模块时，在自己包里声明
// 一个只含所需方法的小接口、由 Hub 注入（规范第 7 条）——那些接口就是**模块之间
// 所有调用的出口**，把它们卡住，等于把"模块之间只能单向通知"变成编译期约束。
//
// 为什么不能有返回值：框架的事件循环是单消费者，一个模块方法只要在等另一个
// actor 的返回值，它自己的队列就停着不动。两个模块互相等就是死锁，而框架的环
// 检测只覆盖同步调用——写成同步之后，能救你的只有 3 秒超时。
//
// 注意这**不限制**模块自己的公有方法：MailboxMod.List 有返回值是对的，
// 它的调用方是 rut，跑在 gin 的请求协程上，不会被任何模块调用。
// 界线在"谁调它"，而 mods 里声明的接口按定义就是给模块自己调的。
func TestModsInterfacesAreNotifyOnly(t *testing.T) {
	fset := token.NewFileSet()
	files, _ := filepath.Glob(filepath.Join("src", "mods", "*", "*.go"))
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, f, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Errorf("解析 %s 失败: %v", f, err)
			continue
		}
		ast.Inspect(parsed, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok {
				return true
			}
			it, ok := ts.Type.(*ast.InterfaceType)
			if !ok {
				return true
			}
			for _, m := range it.Methods.List {
				fn, ok := m.Type.(*ast.FuncType)
				if !ok || len(m.Names) == 0 {
					continue // 内嵌接口之类
				}
				if fn.Results != nil && len(fn.Results.List) > 0 {
					t.Errorf("%s: 接口 %s 的方法 %s 有返回值——"+
						"模块之间只能单向通知，带返回值会让调用方的事件循环停下来等，"+
						"两个模块互相等就是死锁",
						f, ts.Name.Name, m.Names[0].Name)
				}
			}
			return true
		})
	}
}
