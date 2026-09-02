package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// verify 的价值全在"能不能抓到那几种编译得过的错"，所以这组测试大半围绕失败场景
// 造：过期、缺失、孤儿残留、标记删了而生成物还在、缺指令。只验"正常情况通过"
// 是没有意义的。
//
// 另一半同样重要：**合法情况不能被误判**。没有 export: 标记的模块不产生生成物，
// 那是设计而不是错误；把它算成失败等于禁止这种模块存在，而使用方（noteserver）
// 正跑着 -strict。

// verifyFixture 造一个最小的可校验工程：一个模块文件（带 go:generate）
// 加一个输出目录，返回工程根目录。
func verifyFixture(t *testing.T, modName string, extra map[string]string) string {
	t.Helper()
	root := t.TempDir()

	must := func(rel, content string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// go:generate 里不带 -C，路径就相对模块文件所在目录
	must(filepath.Join("mods", modName), `package mods

import "actor"

//go:generate gercmd gen -force -recv Host `+modName+` ../out

type DemoMod struct {
	actor.ModObj[*DemoMod]
}

func NewDemoMod() actor.IModule { return nil }

// Ping 干点什么
//
//	export: DemoPing
func (that *DemoMod) Ping(n int) (int, error) { return n, nil }
`)
	must(filepath.Join("out", "keep.go"), "package out\n")

	for rel, content := range extra {
		must(rel, content)
	}
	return root
}

// generateInto 按 fixture 的 go:generate 真正生成一次，让基线是"最新的"。
func generateInto(t *testing.T, root, modName string) {
	t.Helper()
	res, err := GenerateExports(filepath.Join(root, "mods", modName),
		filepath.Join(root, "out"), GenOptions{Recv: "Host", Force: true})
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if err := os.WriteFile(res.OutPath, res.Content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func runVerifyOn(t *testing.T, root string) *verifyReport {
	t.Helper()
	rep, err := Verify(filepath.Join(root, "mods"), Options{})
	if err != nil {
		t.Fatalf("Verify 失败: %v", err)
	}
	return rep
}

// TestVerifyUpToDate 生成物是最新的时候不该报问题。
func TestVerifyUpToDate(t *testing.T) {
	root := verifyFixture(t, "demo_mod.go", nil)
	generateInto(t, root, "demo_mod.go")

	rep := runVerifyOn(t, root)
	if n := rep.Problems(true); n != 0 {
		t.Fatalf("不该有问题，got %d: %+v", n, rep.Items)
	}
	if len(rep.Items) != 1 || rep.Items[0].Status != "ok" {
		t.Fatalf("应当有 1 个 ok 的模块: %+v", rep.Items)
	}
}

// TestVerifyDetectsStale 改了模块签名而没重新生成 → stale。
//
// 这是最难自己发现的一种：生成物内容看着完全正常，只是描述的是旧签名。
func TestVerifyDetectsStale(t *testing.T) {
	root := verifyFixture(t, "demo_mod.go", nil)
	generateInto(t, root, "demo_mod.go")

	// 改签名：多一个参数
	p := filepath.Join(root, "mods", "demo_mod.go")
	src, _ := os.ReadFile(p)
	changed := strings.Replace(string(src),
		"func (that *DemoMod) Ping(n int) (int, error)",
		"func (that *DemoMod) Ping(n int, s string) (int, error)", 1)
	if changed == string(src) {
		t.Fatal("测试数据没改动，替换串对不上了")
	}
	if err := os.WriteFile(p, []byte(changed), 0o644); err != nil {
		t.Fatal(err)
	}

	rep := runVerifyOn(t, root)
	if rep.Items[0].Status != "stale" {
		t.Fatalf("应当报 stale, got %q（%s）", rep.Items[0].Status, rep.Items[0].Detail)
	}
	if rep.Problems(false) == 0 {
		t.Fatal("stale 必须算失败")
	}
}

// TestVerifyDetectsMissing 生成物不存在 → missing。
func TestVerifyDetectsMissing(t *testing.T) {
	root := verifyFixture(t, "demo_mod.go", nil)
	// 故意不生成

	rep := runVerifyOn(t, root)
	if rep.Items[0].Status != "missing" {
		t.Fatalf("应当报 missing, got %q", rep.Items[0].Status)
	}
}

// TestVerifyDetectsOrphan 输出目录里有没人认领的 **_export.go → 残留。
//
// 这条对应的现实场景是"模块文件改了名"：auth_mod.go 改成 auth_mgr.go 之后
// 生成物换了名字，旧的那份不会自己消失，而且它编译得过。
func TestVerifyDetectsOrphan(t *testing.T) {
	root := verifyFixture(t, "demo_mod.go", map[string]string{
		filepath.Join("out", "old_export.go"): "package out\n",
	})
	generateInto(t, root, "demo_mod.go")

	rep := runVerifyOn(t, root)
	if len(rep.Orphans) != 1 || !strings.HasSuffix(rep.Orphans[0], "old_export.go") {
		t.Fatalf("应当报出 old_export.go 是残留, got %v", rep.Orphans)
	}
	if rep.Problems(false) == 0 {
		t.Fatal("残留必须算失败")
	}
}

// TestVerifyMgrSuffix **_mgr.go 也要被扫到，生成物名字剥掉 _mgr。
func TestVerifyMgrSuffix(t *testing.T) {
	root := verifyFixture(t, "demo_mgr.go", nil)
	generateInto(t, root, "demo_mgr.go")

	rep := runVerifyOn(t, root)
	if len(rep.Items) != 1 {
		t.Fatalf("应当扫到 1 个模块: %+v", rep.Items)
	}
	if rep.Items[0].Status != "ok" {
		t.Fatalf("got %q（%s）", rep.Items[0].Status, rep.Items[0].Detail)
	}
	if !strings.HasSuffix(rep.Items[0].OutPath, "demo_export.go") {
		t.Fatalf("生成物应当叫 demo_export.go（剥掉 _mgr）, got %s", rep.Items[0].OutPath)
	}
}

// TestGenDirectiveResolvesDashC go:generate 里的 -C 要被还原。
//
// 不还原的话，带 -C 的指令（本仓库 noteserver 就是）只能在某个特定目录下
// 校验才对得上，换个目录就全报错——那种工具没人会用。
func TestGenDirectiveResolvesDashC(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "a", "b", "x_mod.go")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	src := "package b\n\n//go:generate go -C ../.. run ./cmd/gercmd gen -recv Hub a/b/x_mod.go a/out\n"
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	args, baseDir, ok := genDirective(p)
	if !ok {
		t.Fatal("没解析出 gen 指令")
	}
	if want := filepath.Clean(root); filepath.Clean(baseDir) != want {
		t.Errorf("baseDir = %q, want %q", baseDir, want)
	}
	_, modArg, outArg, err := parseGenArgs(args)
	if err != nil {
		t.Fatalf("解析参数失败: %v", err)
	}
	if modArg != "a/b/x_mod.go" || outArg != "a/out" {
		t.Errorf("位置参数 = %q %q", modArg, outArg)
	}
}

// --- 零导出路径：两个洞的回归测试 ---
//
// 早先这条路径完全没有覆盖，而它藏着两个方向相反的错：
// 有残留时漏报（因为跳过的模块把自己的输出路径登记进了 expected，给残留打掩护），
// 没残留时误报（因为 -strict 把"没有 export 标记"和"缺 go:generate"混为一谈）。

// noExportFixture 造一个有 go:generate、但没有任何 export: 标记的模块。
// 这是完全合法的：模块可以只有内部方法，不对外产生任何门面。
func noExportFixture(t *testing.T, extra map[string]string) string {
	t.Helper()
	root := t.TempDir()
	must := func(rel, content string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must(filepath.Join("mods", "quiet_mod.go"), `package mods

import "actor"

//go:generate gercmd gen -force -recv Host quiet_mod.go ../out

type QuietMod struct {
	actor.ModObj[*QuietMod]
}

func NewQuietMod() actor.IModule { return nil }

// Ping 是纯内部方法，故意不带 export: 标记
func (that *QuietMod) Ping(n int) (int, error) { return n, nil }
`)
	must(filepath.Join("out", "keep.go"), "package out\n")
	for rel, content := range extra {
		must(rel, content)
	}
	return root
}

// TestVerifyNoExportIsLegal 没有 export: 标记的模块是合法的，
// **即使开着 -strict 也不该算失败**。
//
// 这条对应的现实后果很直接：noteserver 的 TestFacadesUpToDate 跑的就是
// verify -strict，混判的话，那个项目一旦加一个纯内部模块，测试立刻红。
func TestVerifyNoExportIsLegal(t *testing.T) {
	rep := runVerifyOn(t, noExportFixture(t, nil))

	if len(rep.Items) != 1 {
		t.Fatalf("应当扫到 1 个模块: %+v", rep.Items)
	}
	if rep.Items[0].Status != statusNoExport {
		t.Fatalf("状态应当是 %s, got %q（%s）",
			statusNoExport, rep.Items[0].Status, rep.Items[0].Detail)
	}
	if n := rep.Problems(false); n != 0 {
		t.Errorf("不开 strict 时不该算失败, got %d", n)
	}
	if n := rep.Problems(true); n != 0 {
		t.Errorf("**开了 strict 也不该算失败**——没有 export: 标记是合法设计，"+
			"跟\"缺 go:generate\"是两回事, got %d", n)
	}
}

// TestVerifyDetectsLeftoverAfterMarkerRemoved 把 export: 标记删了、
// 生成物却还在磁盘上 —— 必须报出来。
//
// 这是"零导出"路径上最容易发生的一步：标记一删，那份残留仍然参与编译、
// 仍然对外暴露着一个已经不该导出的方法。而它恰恰躲得过孤儿检查——
// 模块还在，它的输出路径就还登记在 expected 里，等于给残留打掩护。
// 反过来把模块文件整个删掉倒能查出来，也就是说漏洞开在更常见的那条路上。
func TestVerifyDetectsLeftoverAfterMarkerRemoved(t *testing.T) {
	root := noExportFixture(t, map[string]string{
		// 标记还在时生成的那份，现在成了残留
		filepath.Join("out", "quiet_export.go"): "package out\n\nfunc HostPing() {}\n",
	})

	rep := runVerifyOn(t, root)
	if rep.Items[0].Status != statusLeftover {
		t.Fatalf("状态应当是 %s, got %q（%s）",
			statusLeftover, rep.Items[0].Status, rep.Items[0].Detail)
	}
	if n := rep.Problems(false); n == 0 {
		t.Fatal("残留必须算失败，且不该依赖 -strict")
	}
	// 诊断要说到点子上：这里的原因是标记被删了，不是文件改了名
	if !strings.Contains(rep.Items[0].Detail, "export:") {
		t.Errorf("诊断没说清原因: %q", rep.Items[0].Detail)
	}
	// 不能同时又被孤儿检查报一遍——那句提示在这个场景下是错的诊断
	for _, o := range rep.Orphans {
		if strings.HasSuffix(o, "quiet_export.go") {
			t.Errorf("同一个文件被报了两次（还带着错误的诊断）: %v", rep.Orphans)
		}
	}
}

// TestVerifyNoDirectiveStillUpgradedByStrict 拆分状态之后，-strict 原本要拦的
// 那种情况仍然要拦住 —— 别为了修误报把真报也一起修没了。
func TestVerifyNoDirectiveStillUpgradedByStrict(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "mods", "plain_mod.go")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("package mods\n\ntype PlainMod struct{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rep := runVerifyOn(t, root)
	if rep.Items[0].Status != statusNoDirective {
		t.Fatalf("状态应当是 %s, got %q", statusNoDirective, rep.Items[0].Status)
	}
	if rep.Problems(false) != 0 {
		t.Error("默认不该算失败")
	}
	if rep.Problems(true) != 1 {
		t.Error("-strict 时缺指令仍要算失败")
	}
}
