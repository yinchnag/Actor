package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// verify 解决的是"生成物与模块脱节"这类问题——它们的共同特征是**编译得过**：
//
//   - 改了模块方法的签名，忘了重新生成，门面还是旧的；
//   - 改了模块文件名（比如 auth_mod.go 改成 auth_mgr.go），旧的生成物留在原地，
//     内容是对的、只是过期了，安静地继续参与编译；
//   - 删了一个模块，它的门面还在；
//   - 把方法上的 export: 标记删了，生成物却没删——那份残留还在参与编译、
//     还对外暴露着一个已经不该导出的方法。这一类最阴：模块还在，
//     它的输出路径就还被登记着，孤儿检查天然抓不到（见 verifyOne 里的 leftover）。
//
// 反过来，"模块只有内部方法、不产生任何门面"是**合法**的，不是错误——
// verify 会把它单独报成 noexport，且永远不算失败，-strict 也不升级它。
//
// 之前这件事靠使用方在测试里手写一张"模块文件 → 模板 → 生成物"的清单。那张清单
// 本身就是新的人为出错点：加了模块忘了登记，检查就默默漏掉它——**检查工具自己
// 需要人来维护，等于没有检查**。所以改成由 gercmd 自己发现：
//
//  1. 递归找 **_mod.go / **_mgr.go；
//  2. 读它们各自 //go:generate 里的 gercmd gen 参数（那本来就是唯一事实来源，
//     模板、接收者、输出目录全写在那儿）；
//  3. 按同样的参数重新生成一遍，与磁盘上的文件逐字节比；
//  4. 反过来扫输出目录，报告没有任何模块对应的多余 **_export.go。
//
// 全程没有需要手工维护的清单。

// verifyItem 是一个模块的校验结果。
type verifyItem struct {
	ModFile string // 模块文件路径
	OutPath string // 期望的生成物路径
	Status  string // 见下面的状态常量
	Detail  string
}

// 校验结果的状态。
//
// noexport 与 nodirective 都表现为"没有生成物"，但含义**相反**，所以必须分开：
// 前者是校验过了、模块确实不该产生门面（只有内部方法，完全合法）；
// 后者是压根没校验，多半是漏写了 //go:generate。混成一个状态的话，-strict
// 想拦第二种，就会连第一种一起误伤——一个合法的纯内部模块能让整个校验变红。
const (
	statusOK          = "ok"          // 生成物与模块一致
	statusStale       = "stale"       // 内容对不上，要重新生成
	statusMissing     = "missing"     // 该有生成物却没有
	statusLeftover    = "leftover"    // 不该有生成物却有——标记删了、文件没删
	statusNoExport    = "noexport"    // 没有 export: 标记，正确地不产生文件（合法）
	statusNoDirective = "nodirective" // 没有 //go:generate，无从校验
	statusError       = "error"       // 指令解析或生成失败
)

// verifyReport 汇总一次校验。
type verifyReport struct {
	Items   []verifyItem
	Orphans []string // 输出目录里没有模块对应的 **_export.go
	Warns   []string
}

// Problems 返回真正算失败的条数。
//
// strict 只升级 nodirective——它想拦的是"漏写了 go:generate"。noexport 无论
// 如何都不算失败：模块只有内部方法、不产生任何门面是合法设计，
// 把它算成失败等于禁止这种模块存在。
func (that *verifyReport) Problems(strict bool) int {
	n := len(that.Orphans)
	for _, it := range that.Items {
		switch it.Status {
		case statusOK, statusNoExport:
			// 都不算问题
		case statusNoDirective:
			if strict {
				n++
			}
		default: // stale / missing / leftover / error
			n++
		}
	}
	return n
}

// genDirective 从模块文件里找出 //go:generate 的 gercmd gen 参数。
//
// 返回的 baseDir 是这些参数所相对的目录。go:generate 里常见
// `go -C <dir> run ./cmd/gercmd gen ...` 这种写法——-C 会让 go 先切目录，
// 于是后面的路径都相对那个目录，而不是相对模块文件。不还原这一步，
// 校验就只能在某个特定目录下运行才对得上。
func genDirective(modFile string) (args []string, baseDir string, ok bool) {
	raw, err := os.ReadFile(modFile)
	if err != nil {
		return nil, "", false
	}
	dir := filepath.Dir(modFile)

	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "//go:generate") {
			continue
		}
		fields := strings.Fields(line)

		// 找 gercmd 之后的那个 gen。不直接找 "gen"，是因为路径里也可能出现它。
		seenTool := false
		for i, f := range fields {
			if strings.Contains(f, "gercmd") {
				seenTool = true
				continue
			}
			if seenTool && f == "gen" {
				args = fields[i+1:]
				ok = true
				break
			}
		}
		if !ok {
			continue
		}

		// 还原 -C：它相对的是含指令的文件所在目录
		baseDir = dir
		for i, f := range fields {
			if f == "-C" && i+1 < len(fields) {
				baseDir = filepath.Join(dir, fields[i+1])
				break
			}
		}
		return args, baseDir, true
	}
	return nil, "", false
}

// parseGenArgs 把 gen 的参数解析成选项与两个位置参数。
//
// 刻意复用 gen 自己的 flag 定义，而不是另写一份解析：两边一旦分岔，
// 校验就会用一套参数、真正生成用另一套，那比不校验更糟。
func parseGenArgs(args []string) (opt GenOptions, modArg, outArg string, err error) {
	c := newSubcmd("gen", "", "")
	c.fs.SetOutput(&bytes.Buffer{}) // 解析失败由我们自己报，别打到 stderr
	c.fs.BoolVar(&opt.All, "all", false, "")
	c.fs.BoolVar(&opt.Force, "force", false, "")
	c.fs.BoolVar(&opt.DryRun, "n", false, "")
	c.fs.StringVar(&opt.TemplateFile, "tmpl", "", "")
	c.fs.StringVar(&opt.Recv, "recv", defaultRecv, "")
	opt.Naming.bind(c.fs) // 指令里可能带 -mod-suffix 之类，不认就解析不了
	if err = c.fs.Parse(args); err != nil {
		return opt, "", "", fmt.Errorf("解析 go:generate 参数失败: %w", err)
	}
	pos := c.fs.Args()
	if len(pos) != 2 {
		return opt, "", "", fmt.Errorf("go:generate 里的 gen 应当有 2 个位置参数，实际 %d 个", len(pos))
	}
	opt.DryRun = true // 校验绝不落盘
	return opt, pos[0], pos[1], nil
}

// verifyOne 校验一个模块的生成物。
func verifyOne(modFile string, nm Naming) verifyItem {
	it := verifyItem{ModFile: filepath.ToSlash(modFile)}

	args, baseDir, ok := genDirective(modFile)
	if !ok {
		it.Status = statusNoDirective
		it.Detail = "没有 //go:generate 的 gercmd gen 指令，无从校验"
		return it
	}

	opt, modArg, outArg, err := parseGenArgs(args)
	if err != nil {
		it.Status = statusError
		it.Detail = err.Error()
		return it
	}
	// 指令里的路径都相对 baseDir
	modPath := filepath.Join(baseDir, modArg)
	outDir := filepath.Join(baseDir, outArg)
	if opt.TemplateFile != "" {
		opt.TemplateFile = filepath.Join(baseDir, opt.TemplateFile)
	}

	// 指令里没写的字段继承命令行传进来的那套
	if len(opt.Naming.ModFileSuffixes) == 0 {
		opt.Naming.ModFileSuffixes = nm.ModFileSuffixes
	}
	if opt.Naming.ExportSuffix == "" {
		opt.Naming.ExportSuffix = nm.ExportSuffix
	}
	res, err := GenerateExports(modPath, outDir, opt)
	if err != nil {
		it.Status = statusError
		it.Detail = err.Error()
		return it
	}
	it.OutPath = filepath.ToSlash(res.OutPath)

	if len(res.Funcs) == 0 {
		// 模块里没有 export: 标记，本来就不该有生成物——但必须看一眼磁盘。
		//
		// "把方法上的 export: 删了、忘了删生成物"是很常见的一步。那份残留还在
		// 参与编译、还对外暴露着一个已经不该导出的方法，而它又永远不会被
		// findOrphans 抓到：这个模块把自己的输出路径登记进了 expected，
		// 等于给残留打了掩护。所以在这里就地判掉。
		//
		// 诊断也只有在这里才说得准。孤儿检查那句"多半是改了模块文件名之后忘了
		// 删旧的生成物"在这个场景下是错的，会把人引向改文件名的方向。
		if _, statErr := os.Stat(res.OutPath); statErr == nil {
			it.Status = statusLeftover
			it.Detail = "模块里已经没有 export: 标记，生成物却还在——" +
				"删掉它，或者把标记加回来"
			return it
		}
		it.Status = statusNoExport
		it.Detail = "没有带 export: 标记的方法，不产生生成物（合法）"
		return it
	}

	onDisk, err := os.ReadFile(res.OutPath)
	if err != nil {
		it.Status = statusMissing
		it.Detail = "生成物不存在，跑一次 go generate"
		return it
	}
	if normalizeEOL(onDisk) != normalizeEOL(res.Content) {
		it.Status = statusStale
		it.Detail = "内容与当前模块签名不一致，跑一次 go generate"
		return it
	}
	it.Status = statusOK
	return it
}

// normalizeEOL 统一行尾。仓库在 Windows 上按 autocrlf 检出是 CRLF，
// 而生成器始终输出 LF——不归一化的话，"生成物是否最新"这个判断会跟着
// 每个人的 git 配置飘。
func normalizeEOL(b []byte) string {
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

// Verify 扫描 root 下的模块并校验它们的生成物。
func Verify(root string, opt Options) (*verifyReport, error) {
	rep := &verifyReport{}
	nm := opt.naming()
	var warns strings.Builder

	err := walkTree(root, Options{Recursive: true, SkipHidden: opt.SkipHidden}, &warns,
		func(e walkEntry) error {
			if !e.D.Type().IsRegular() || !nm.IsModuleFile(e.D.Name()) {
				return nil
			}
			rep.Items = append(rep.Items, verifyOne(e.Path, nm))
			return nil
		})
	if err != nil {
		return nil, err
	}
	if s := warns.String(); s != "" {
		rep.Warns = append(rep.Warns, strings.Split(strings.TrimRight(s, "\n"), "\n")...)
	}

	sort.Slice(rep.Items, func(i, j int) bool { return rep.Items[i].ModFile < rep.Items[j].ModFile })
	rep.Orphans = findOrphans(rep.Items, nm)
	return rep, nil
}

// findOrphans 反过来扫输出目录：没有任何模块对应的 **_export.go 就是残留。
//
// 只扫"确实有模块往里生成过"的目录，不是满仓库找——否则别的项目的生成物
// 会被误报成残留。
//
// 注意所有带 OutPath 的条目都会进 expected，**包括 noexport / leftover 的**。
// 这不是漏网：那两种情况已经由 verifyOne 就地判过了（leftover 是失败状态），
// 这里再报一次只会重复，而且会用上一句错误的诊断——"多半是改了模块文件名"
// 在"标记被删了"的场景下会把人引向错误的方向。
func findOrphans(items []verifyItem, nm Naming) []string {
	expected := map[string]bool{}
	dirs := map[string]bool{}
	for _, it := range items {
		if it.OutPath == "" {
			continue
		}
		p := filepath.Clean(it.OutPath)
		expected[p] = true
		dirs[filepath.Dir(p)] = true
	}

	var orphans []string
	for dir := range dirs {
		found, err := filepath.Glob(filepath.Join(dir, nm.ExportGlob()))
		if err != nil {
			continue
		}
		for _, f := range found {
			if !expected[filepath.Clean(f)] {
				orphans = append(orphans, filepath.ToSlash(f))
			}
		}
	}
	sort.Strings(orphans)
	return orphans
}

// statusMark 给状态挑个记号。
//
// nodirective 的记号跟着 -strict 走：不开 strict 时它只是条提示（·），
// 开了就是失败（✗）。记号跟退出码对不上的话，人会照着记号判断，那比没有记号更糟。
func statusMark(status string, strict bool) string {
	switch status {
	case statusOK:
		return "✓"
	case statusNoExport:
		return "·"
	case statusNoDirective:
		if strict {
			return "✗"
		}
		return "·"
	default: // stale / missing / leftover / error
		return "✗"
	}
}

// runVerify 是 verify 子命令的入口。
func runVerify(args []string) int {
	var opt Options
	var strict bool
	c := newSubcmd("verify", "[路径]",
		"递归找 **_mod.go / **_mgr.go，读它们 //go:generate 里的 gercmd gen 参数，\n"+
			"重新生成一遍与磁盘上的门面比对；并报告没有模块对应的多余 **_export.go。\n"+
			"不给路径时检查当前目录。全程不写盘。")
	c.fs.BoolVar(&opt.SkipHidden, "skip-hidden", false, "跳过以 . 开头的目录与文件")
	c.fs.BoolVar(&strict, "strict", false,
		"把\"缺 //go:generate 指令\"也算失败（不影响没有 export: 标记的模块，那是合法的）")
	opt.Naming.bind(c.fs)
	pos, code := c.parse(args, 0, 1)
	if code != exitOK {
		return code
	}
	root := argOr(pos, 0, ".")

	nm := opt.naming()
	rep, err := Verify(root, opt)
	if err != nil {
		return fail(err)
	}

	fmt.Printf("校验门面生成物，范围 %s\n\n", root)
	for _, w := range rep.Warns {
		fmt.Fprintf(os.Stderr, "警告: %s\n", w)
	}

	if len(rep.Items) == 0 {
		fmt.Printf("  没找到 %s\n", nm.ModFilePattern())
	}
	for _, it := range rep.Items {
		mark := statusMark(it.Status, strict)
		fmt.Printf("  %s  %-44s %s\n", mark, padRight(it.ModFile, 44), it.OutPath)
		if it.Detail != "" {
			fmt.Printf("     %s\n", it.Detail)
		}
	}
	for _, o := range rep.Orphans {
		fmt.Printf("  ✗  %s\n     没有任何模块对应它——多半是改了模块文件名之后忘了删旧的生成物\n", o)
	}

	checked, noExport, noDirective := 0, 0, 0
	for _, it := range rep.Items {
		switch it.Status {
		case statusNoDirective:
			noDirective++
		case statusNoExport:
			noExport++
			checked++ // 它是校验过的：生成器跑了，结论是"不该有生成物"
		default:
			checked++
		}
	}

	n := rep.Problems(strict)
	fmt.Println()
	if n == 0 {
		// 三个数分开说。noexport 是校验过的合法结果，nodirective 才是"没校验"——
		// 全是后者时报一句"都是最新的"是误导，那种情况下一个都没验。
		fmt.Printf("%d 个模块已校验，生成物都是最新的", checked)
		if noExport > 0 {
			fmt.Printf("（其中 %d 个没有 export: 标记，本就不产生生成物）", noExport)
		}
		if noDirective > 0 {
			fmt.Printf("；%d 个未校验，缺 //go:generate（用 -strict 把它算失败）", noDirective)
		}
		fmt.Println()
		return exitOK
	}
	fmt.Printf("%d 处需要处理\n", n)
	return exitFail
}
