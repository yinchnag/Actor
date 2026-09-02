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
		"comm": {},
		// 只放接口与它们返回的错误哨兵。
		"contract": {"comm"},
		// 存储对象。再往上的东西（mods/service）它不该认识。
		"databases": {"comm", "contract"},

		// 以下三个不在规范的七个文件夹里，属于第 8 条允许的补充，
		// 但同样受依赖约束管——它们都在 HTTP 侧，不许碰 mods/service。
		"bases":      {"comm"},
		"security":   {"comm"},
		"config":     {},
		"middleware": {"bases", "comm", "contract"},

		// 逻辑模块。**绝不能引 service**：那会跟 service→mods 成环，
		// 也会让模块之间越过导出函数直接互调（见 README 第 7 条）。
		"mods/auth": {"comm", "contract"},
		"mods/note": {"comm", "contract"},

		// 路由。它通过自己声明的小接口拿 Hub 的能力，所以也不引 service。
		"router/auth":   {"bases", "comm", "contract", "security", "middleware"},
		"router/note":   {"bases", "comm", "contract", "security", "middleware"},
		"router/health": {"bases", "comm", "contract", "security", "middleware"},

		// 最上层的终端，可以引所有包。
		"service": {"comm", "contract", "databases", "mods/auth", "mods/note",
			"bases", "security", "config", "middleware"},
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

// TestModFileNaming 一个功能模块文件夹只能有一个 **_mod.go 或 **_mgr.go。
//
// 规范第 5 条。两个入口意味着两个 ModObj，也就是两条跨 goroutine 的通道进同一个
// 功能——真要细分该用 **_imp.go 由 Mod/Mgr 持有，而不是再开一个模块。
func TestModFileNaming(t *testing.T) {
	dirs, err := filepath.Glob(filepath.Join("src", "mods", "*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range dirs {
		files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
		entries := make([]string, 0, 2)
		for _, f := range files {
			base := filepath.Base(f)
			if strings.HasSuffix(base, "_mod.go") || strings.HasSuffix(base, "_mgr.go") {
				entries = append(entries, base)
			}
		}
		switch len(entries) {
		case 1: // 正好一个，合规
		case 0:
			t.Errorf("%s 里没有 **_mod.go 或 **_mgr.go——功能模块文件夹必须有且只有一个入口", dir)
		default:
			t.Errorf("%s 里有多个模块入口 %v——细分功能请用 **_imp.go 由 Mod/Mgr 持有", dir, entries)
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
