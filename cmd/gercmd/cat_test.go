package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func cat(t *testing.T, path string, opt CatOptions) (string, int64) {
	t.Helper()
	var out bytes.Buffer
	n, err := PrintFile(path, opt, &out)
	if err != nil {
		t.Fatalf("PrintFile 失败: %v", err)
	}
	return out.String(), n
}

// TestCatByteExact 内容必须原样透传，一个字节不多不少。
//
// 这是这个命令唯一的硬要求：gercmd cat x > y 得到的必须是 x 的精确副本。
// 所以刻意用了混合内容——中文、CRLF、连续空行、行尾空格——
// 任何一处"顺手规整一下"都会在这里暴露。
func TestCatByteExact(t *testing.T) {
	dir := t.TempDir()
	raw := []byte("第一行\r\nsecond line   \n\n\n中文 with 混排\ttab\n末行没有换行符")
	p := writeFile(t, dir, "mixed.txt", raw)

	got, n := cat(t, p, CatOptions{})
	if got != string(raw) {
		t.Fatalf("内容被改动了\n got %q\nwant %q", got, string(raw))
	}
	if n != int64(len(raw)) {
		t.Fatalf("字节数 = %d, want %d", n, len(raw))
	}
}

// TestCatEmptyFile 空文件输出空，不是错误。
func TestCatEmptyFile(t *testing.T) {
	p := writeFile(t, t.TempDir(), "empty.txt", nil)
	got, n := cat(t, p, CatOptions{})
	if got != "" || n != 0 {
		t.Fatalf("空文件应当没有输出, got %q (n=%d)", got, n)
	}
}

// TestCatNumbered 行号从 1 开始，且不改动行的内容。
func TestCatNumbered(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "three.txt", []byte("alpha\nbeta\ngamma\n"))

	got, _ := cat(t, p, CatOptions{Number: true})
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("应当是 3 行, got %d: %q", len(lines), got)
	}
	for i, want := range []string{"alpha", "beta", "gamma"} {
		if !strings.HasSuffix(lines[i], "\t"+want) {
			t.Fatalf("第 %d 行内容不对: %q", i+1, lines[i])
		}
		if !strings.Contains(lines[i], strings.TrimSpace(lines[i][:7])) {
			t.Fatalf("第 %d 行没有行号: %q", i+1, lines[i])
		}
	}
	if !strings.HasPrefix(strings.TrimSpace(lines[0]), "1\t") {
		t.Fatalf("行号应当从 1 开始: %q", lines[0])
	}
}

// TestCatNumberedNoTrailingNewline 末行没有换行符时要补一个，
// 否则调用方打印的统计信息会跟内容粘在同一行。
func TestCatNumberedNoTrailingNewline(t *testing.T) {
	p := writeFile(t, t.TempDir(), "noeol.txt", []byte("only line, no newline"))
	got, _ := cat(t, p, CatOptions{Number: true})
	if !strings.HasSuffix(got, "\n") {
		t.Fatalf("加行号时末尾应当补换行, got %q", got)
	}
	if strings.Count(got, "\n") != 1 {
		t.Fatalf("不该多补换行, got %q", got)
	}
}

// TestCatVeryLongLine 单行超长也要能打出来。
//
// 这条专门盯着实现别用 bufio.Scanner——它默认单行上限 64KB，
// 压缩过的 JS、单行 JSON、长日志都会直接把它撑爆，而这类文件恰恰常要看。
func TestCatVeryLongLine(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("x", 300<<10) // 300KB 单行，远超 Scanner 的 64KB
	p := writeFile(t, dir, "long.txt", []byte(long+"\n"))

	for _, opt := range []CatOptions{{}, {Number: true}} {
		got, _ := cat(t, p, opt)
		if !strings.Contains(got, long) {
			t.Fatalf("超长行被截断了 (Number=%v), 长度 %d", opt.Number, len(got))
		}
	}
}

// TestCatRejectsBinary 二进制默认拒绝——直接打到终端会弄乱显示。
func TestCatRejectsBinary(t *testing.T) {
	dir := t.TempDir()
	// 前面放一段可读文本，NUL 藏在后面：证明判定是嗅探了一段而不是只看首字节
	data := append([]byte("MZ这看着像文本"), 0x00, 0x01, 0x02)
	p := writeFile(t, dir, "blob.bin", data)

	var out bytes.Buffer
	_, err := PrintFile(p, CatOptions{}, &out)
	if err == nil {
		t.Fatal("二进制文件应当被拒绝")
	}
	if !strings.Contains(err.Error(), "二进制") {
		t.Fatalf("错误信息没说清原因: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("被拒绝时不该已经吐出内容: %q", out.String())
	}
}

// TestCatForceBinary -f 强制输出，且仍然是字节精确的。
func TestCatForceBinary(t *testing.T) {
	dir := t.TempDir()
	data := []byte{0x00, 0x01, 0xFF, 'a', 'b', 0x00}
	p := writeFile(t, dir, "blob.bin", data)

	var out bytes.Buffer
	n, err := PrintFile(p, CatOptions{Force: true}, &out)
	if err != nil {
		t.Fatalf("-f 时不该报错: %v", err)
	}
	if !bytes.Equal(out.Bytes(), data) {
		t.Fatalf("字节不一致\n got %v\nwant %v", out.Bytes(), data)
	}
	if n != int64(len(data)) {
		t.Fatalf("字节数 = %d, want %d", n, len(data))
	}
}

// TestCatUTF8NotTranscoded 不做任何编码转换。文件是什么编码出来就是什么编码——
// 一转换就没法保证重定向到文件时字节不变。
func TestCatUTF8NotTranscoded(t *testing.T) {
	dir := t.TempDir()
	text := "锦瑟无端五十弦\n一弦一柱思华年\n"
	p := writeFile(t, dir, "poem.txt", []byte(text))

	got, n := cat(t, p, CatOptions{})
	if got != text {
		t.Fatal("UTF-8 内容被改动了")
	}
	if n != int64(len(text)) { // 每个汉字 3 字节
		t.Fatalf("字节数 = %d, want %d", n, len(text))
	}
}

// TestCatErrors 路径不存在、指向文件夹时要给出清楚的错误。
func TestCatErrors(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer

	if _, err := PrintFile(filepath.Join(dir, "不存在"), CatOptions{}, &out); err == nil {
		t.Fatal("路径不存在时应当报错")
	}

	_, err := PrintFile(dir, CatOptions{}, &out)
	if err == nil {
		t.Fatal("路径指向文件夹时应当报错")
	}
	if !strings.Contains(err.Error(), "是文件夹") {
		t.Fatalf("错误信息没说清原因: %v", err)
	}
}
