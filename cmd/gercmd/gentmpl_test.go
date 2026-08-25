package main

import (
	"strings"
	"testing"
)

// 模板化换来了灵活，代价是错误从编译期挪到了运行期。
// 这一组用例守的就是这个代价没有失控：换模板要真能生效，
// 模板写坏时要说清坏在哪，内嵌模板要真的被打进二进制。

// TestGenCustomTemplate 换模板能改变输出形态，且不用碰一行 Go 代码。
// 这是改用模板的全部理由——门面的形状不再焊死在生成器里。
func TestGenCustomTemplate(t *testing.T) {
	modFile, outDir := genFixture(t, genModSrc)
	custom := `package {{.Package}}

{{range .Funcs}}
func (that *{{$.Recv}}) {{.Name}}({{.SigParams}}) error {
	_, err := that.GetModloader().ModInvoke({{.InvokeArgs}})
	return err
}
{{end}}`
	tmpl := writeFile(t, t.TempDir(), "custom.tmpl", []byte(custom))

	res, err := GenerateExports(modFile, outDir, GenOptions{TemplateFile: tmpl, DryRun: true})
	if err != nil {
		t.Fatalf("自定义模板生成失败: %v", err)
	}
	src := string(res.Content)
	if !strings.Contains(src, "func (that *PlayerEnt) BagAddItem(itemid int, count int) error {") {
		t.Fatalf("自定义模板没生效:\n%s", src)
	}
	if strings.Contains(src, "println") {
		t.Fatal("不该还带着默认模板的内容")
	}
	// 语义分析的结果照旧可用：参数名消毒、类型渲染都不受模板影响
	if !strings.Contains(src, "BagCollide(p0 int, p1 string, p2 bool)") {
		t.Fatalf("换模板不该影响参数名消毒:\n%s", src)
	}
}

// TestGenCustomRecv 接收者类型也能换，不必非叫 PlayerEnt。
func TestGenCustomRecv(t *testing.T) {
	modFile, outDir := genFixture(t, genModSrc)
	res, err := GenerateExports(modFile, outDir, GenOptions{Recv: "RoleEnt", DryRun: true})
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if !strings.Contains(string(res.Content), "func (that *RoleEnt) BagAddItem") {
		t.Fatalf("-recv 没生效:\n%s", res.Content)
	}
}

// TestGenBadTemplateReportsClearly 模板写坏时要说清是模板的问题、坏在哪。
//
// 拼字符串时这些错误编译期就挡住了；换成模板之后只能靠运行期报错，
// 所以报错必须足够具体，否则改模板的人无从下手。
func TestGenBadTemplateReportsClearly(t *testing.T) {
	modFile, outDir := genFixture(t, genModSrc)
	dir := t.TempDir()

	for _, c := range []struct {
		name, body, wantIn string
	}{
		{
			name:   "字段名写错",
			body:   "package {{.Package}}\n{{.NoSuchField}}\n",
			wantIn: "NoSuchField",
		},
		{
			name:   "产出不是合法 Go",
			body:   "这不是 Go 代码 {{.Package}}\n",
			wantIn: "不是合法 Go 代码",
		},
		{
			name:   "模板语法错",
			body:   "package {{.Package}}\n{{range .Funcs}}\n", // 缺 end
			wantIn: "语法错误",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := writeFile(t, dir, strings.ReplaceAll(c.name, " ", "_")+".tmpl", []byte(c.body))
			_, err := GenerateExports(modFile, outDir, GenOptions{TemplateFile: f, DryRun: true})
			if err == nil {
				t.Fatal("应当报错")
			}
			if !strings.Contains(err.Error(), c.wantIn) {
				t.Fatalf("报错没说清原因，期望包含 %q: %v", c.wantIn, err)
			}
		})
	}

	// 产出不合法时还得带上原始产出，否则没法定位模板哪一行写坏了
	f := writeFile(t, dir, "notgo.tmpl", []byte("这不是 Go 代码 {{.Package}}\n"))
	_, err := GenerateExports(modFile, outDir, GenOptions{TemplateFile: f, DryRun: true})
	if err == nil || !strings.Contains(err.Error(), "原始产出") {
		t.Fatalf("报错里应当附上原始产出: %v", err)
	}
}

// TestGenMissingTemplateFile 模板文件不存在要明确报错，而不是悄悄用默认模板。
func TestGenMissingTemplateFile(t *testing.T) {
	modFile, outDir := genFixture(t, genModSrc)
	_, err := GenerateExports(modFile, outDir,
		GenOptions{TemplateFile: "根本不存在.tmpl", DryRun: true})
	if err == nil || !strings.Contains(err.Error(), "读取模板") {
		t.Fatalf("模板文件不存在时应当明确报错, got %v", err)
	}
}

// TestGenDefaultTemplateEmbedded 内嵌模板必须真的被打进二进制。
// 忘了 //go:embed 的话，脱离源码目录运行就会拿到空模板。
func TestGenDefaultTemplateEmbedded(t *testing.T) {
	if !strings.Contains(defaultTemplate, "ModInvoke") {
		t.Fatal("内嵌的默认模板是空的或者不对")
	}
	// 模板顶部要写明有哪些字段可用，否则改模板的人只能靠猜
	for _, field := range []string{"FacadeFile", "FacadeFunc", ".SigParams", ".InvokeArgs", ".RetVars"} {
		if !strings.Contains(defaultTemplate, field) {
			t.Errorf("默认模板的字段说明里缺 %s", field)
		}
	}
}

// TestFacadeHelpers 直接测给模板用的那几个便捷方法。
// 模板里的拼接规则就靠它们，写错了会体现在每一个生成的函数上。
func TestFacadeHelpers(t *testing.T) {
	f := FacadeFunc{
		Name: "BagGetItem", Method: "GetItem", ModType: "BagMod",
		Params: []FacadeParam{{Name: "id", Type: "int"}, {Name: "kind", Type: "string"}},
		Results: []FacadeResult{
			{Var: "r0", Type: "int", Index: 0},
			{Var: "r1", Type: "error", Index: 1},
		},
	}
	if got, want := f.SigParams(), "id int, kind string"; got != want {
		t.Errorf("SigParams = %q, want %q", got, want)
	}
	if got, want := f.ResultSig(), " (int, error)"; got != want {
		t.Errorf("ResultSig = %q, want %q", got, want)
	}
	if got, want := f.InvokeArgs(), `"BagMod", "GetItem", id, kind`; got != want {
		t.Errorf("InvokeArgs = %q, want %q", got, want)
	}
	if got, want := f.RetVars(), "r0, r1"; got != want {
		t.Errorf("RetVars = %q, want %q", got, want)
	}
	if !f.HasResults() || f.NumResults() != 2 {
		t.Error("HasResults/NumResults 不对")
	}

	// 边界：无参无返回
	empty := FacadeFunc{Name: "BagReset", Method: "Reset", ModType: "BagMod"}
	if empty.SigParams() != "" || empty.ResultSig() != "" || empty.RetVars() != "" {
		t.Error("无参无返回时几个拼接方法都该是空串")
	}
	if empty.HasResults() {
		t.Error("无返回值时 HasResults 应为 false")
	}
	if got, want := empty.InvokeArgs(), `"BagMod", "Reset"`; got != want {
		t.Errorf("InvokeArgs = %q, want %q", got, want)
	}

	// 单返回值不加括号
	one := FacadeFunc{Results: []FacadeResult{{Var: "r0", Type: "int"}}}
	if got, want := one.ResultSig(), " int"; got != want {
		t.Errorf("单返回值 ResultSig = %q, want %q", got, want)
	}

	// 没有注释时 DocText 是空串，不能是一个孤零零的换行
	if empty.DocText() != "" {
		t.Errorf("无注释时 DocText 应为空串, got %q", empty.DocText())
	}
	withDoc := FacadeFunc{Doc: []string{"// 增加物品", "// 第二行"}}
	if got, want := withDoc.DocText(), "// 增加物品\n// 第二行\n"; got != want {
		t.Errorf("DocText = %q, want %q", got, want)
	}
}
