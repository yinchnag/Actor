package main

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

// TestFacadesUpToDate 守住"改了模块忘了重新生成"这条路。
//
// 生成的门面是路由与 actor 模块之间唯一的桥。它一旦跟模块签名对不上，症状不是
// 编译错误而是运行期的 module not found / 返回值个数异常——也就是说，不设这道
// 闸门，漂移只会在跑到那条接口时才暴露。
//
// 这里**不列清单**。早先的版本在测试里手写一张"模块文件 → 模板 → 生成物"的表，
// 但那张表本身就是新的人为出错点：加了模块忘了登记，检查就默默漏掉它——
// 检查工具自己需要人来维护，等于没有检查。现在交给 gercmd verify：它自己递归找
// **_mod.go / **_mgr.go，自己读它们 //go:generate 里的参数，自己重新生成比对，
// 并反过来报告没有模块对应的多余 **_export.go。
//
// -strict 让"模块没有 go:generate 指令"也算失败。本项目要求每个模块都能被校验，
// 所以开着；GameSvr 那种手工生成的项目不开就是了。
func TestFacadesUpToDate(t *testing.T) {
	// gercmd 在父模块里，且依赖 GCore——用 go -C 切过去跑，
	// 免得为一个构建期工具把 GCore 拖进本示例的依赖图。
	cmd := exec.Command("go", "-C", "../..", "run", "./cmd/gercmd",
		"verify", "-strict", "cmd/noteserver/src")

	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()

	out := stdout.String()
	if err == nil {
		t.Log(strings.TrimSpace(out))
		return
	}
	// 跑不起来（拿不到父模块的依赖）就跳过，而不是失败：这个测试的价值是
	// "有条件时挡住漂移"，不是"没条件时红一片"。分辨方法是看有没有正常输出——
	// verify 判定失败时一定打了报告，而工具链问题不会。
	if !strings.Contains(out, "校验门面生成物") {
		t.Skipf("跑不了 gercmd，跳过（%v）:\n%s", err, stderr.String())
	}
	t.Errorf("门面与模块不一致，跑一次 go generate ./src/mods/...\n\n%s", strings.TrimSpace(out))
}
