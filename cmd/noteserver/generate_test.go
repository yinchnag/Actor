package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestGeneratedFacadesUpToDate 守住"改了模块方法忘了重新生成"这条路。
//
// 生成的门面是路由与 actor 模块之间唯一的桥。它一旦跟模块签名对不上，
// 症状不是编译错误而是运行期的 module not found / 返回值个数异常——
// 也就是说，不设这道闸门，漂移只会在跑到那条接口时才暴露。
//
// 做法是把生成器再跑一遍（-n 只打印不落盘），跟磁盘上的文件逐字节比。
// 与 gercmd 自己的 TestGenCompiles 是两件事：那个验"生成的代码能编译"，
// 这个验"磁盘上的文件确实是当前模块签名生成出来的"。
func TestGeneratedFacadesUpToDate(t *testing.T) {
	cases := []struct {
		tmpl, mod, out string
	}{
		{"shard_export.tmpl", "auth_mod.go", "auth_export.go"},
		{"user_export.tmpl", "note_mod.go", "note_export.go"},
	}

	for _, c := range cases {
		t.Run(c.out, func(t *testing.T) {
			// gercmd 在父模块里，且依赖 GCore——用 go -C 切过去跑，
			// 免得为一个构建期工具把 GCore 拖进本示例的依赖图。
			// 参数路径都相对仓库根，因为 -C 之后工作目录就是那里。
			cmd := exec.Command("go", "-C", "../..", "run", "./cmd/gercmd", "gen", "-n",
				"-tmpl", "cmd/noteserver/templates/"+c.tmpl,
				"-recv", "Hub",
				"cmd/noteserver/src/mods/"+c.mod,
				"cmd/noteserver/src/service")

			// 只取 stdout：生成的内容走 stdout，而"（-n 预览，未写文件）…"
			// 那行摘要走 stderr，混在一起比就永远对不上。
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			outBytes, err := cmd.Output()
			if err != nil {
				// 拿不到生成器就跳过，而不是失败：CI 上可能没有父模块的依赖，
				// 而这个测试的价值是"有条件时挡住漂移"，不是"没条件时红一片"。
				t.Skipf("跑不了 gercmd，跳过（%v）:\n%s", err, stderr.String())
			}

			want := normalize(string(outBytes))
			gotBytes, err := os.ReadFile(filepath.Join("src", "service", c.out))
			if err != nil {
				t.Fatalf("读 %s 失败: %v", c.out, err)
			}
			got := normalize(string(gotBytes))

			if got != want {
				t.Errorf("src/service/%s 与当前模块签名不一致，请重新生成:\n"+
					"    go generate ./src/mods/\n\n"+
					"磁盘上 %d 行，重新生成 %d 行",
					c.out, strings.Count(got, "\n"), strings.Count(want, "\n"))
				// 打出第一处差异，省得人肉 diff
				if line, g, w := firstDiff(got, want); line >= 0 {
					t.Errorf("第 %d 行不同:\n  磁盘: %q\n  应为: %q", line+1, g, w)
				}
			}
		})
	}
}

func normalize(s string) string {
	return strings.ReplaceAll(strings.TrimRight(s, "\r\n \t"), "\r\n", "\n")
}

func firstDiff(a, b string) (int, string, string) {
	la, lb := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := 0; i < len(la) || i < len(lb); i++ {
		var x, y string
		if i < len(la) {
			x = la[i]
		}
		if i < len(lb) {
			y = lb[i]
		}
		if x != y {
			return i, x, y
		}
	}
	return -1, "", ""
}
