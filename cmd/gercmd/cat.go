package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
)

const (
	// sniffLen 判定是否为二进制时嗅探的字节数。
	sniffLen = 8 << 10
	// readBufSize 读缓冲，必须不小于 sniffLen，否则 Peek 会因缓冲区装不下而失败。
	readBufSize = 64 << 10
)

// CatOptions 控制打印行为。
type CatOptions struct {
	// Number 在每行前加行号。
	Number bool
	// Force 即便看着像二进制也照样输出。
	Force bool
}

// runCat 是 cat 子命令的入口。
//
// 它和列举类命令形状不同——收的是文件而不是文件夹，也没有 -r/-skip-hidden，
// 所以没有硬套 runList；共用的是 subcmd 那套参数处理和输出约定。
func runCat(args []string) int {
	var opt CatOptions
	c := newSubcmd("cat", "<文件>", "按字节原样输出，不做编码转换。")
	c.fs.BoolVar(&opt.Number, "n", false, "每行前加行号")
	c.fs.BoolVar(&opt.Force, "f", false, "即便看着像二进制也照样输出")
	pos, code := c.parse(args, 1, 1)
	if code != exitOK {
		return code
	}

	// 内容走 stdout，统计走 stderr——这样 gercmd cat x > y 拿到的是干净的副本
	n, err := PrintFile(pos[0], opt, os.Stdout)
	if err != nil {
		return fail(err)
	}
	countLine(n, "字节")
	return exitOK
}

// PrintFile 把 path 的内容原样写进 out，返回写出的字节数。
//
// 内容按字节原样透传，不做任何编码转换：文件是什么编码，出来就是什么编码。
// 在 GBK 控制台上看 UTF-8 文件会花屏，那是终端的事，工具不该自作主张去转——
// 一转就没法保证重定向到文件时字节不变了。
func PrintFile(path string, opt CatOptions, out io.Writer) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("无法访问 %s: %w", path, err)
	}
	if info.IsDir() {
		return 0, fmt.Errorf("%s 是文件夹，不是文件", path)
	}
	if !info.Mode().IsRegular() {
		// 设备、管道、套接字之类：读它们可能永久阻塞，不是这个命令该干的事
		return 0, fmt.Errorf("%s 不是常规文件", path)
	}

	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("打开 %s 失败: %w", path, err)
	}
	defer f.Close()

	// 缓冲区要大于 sniffLen，下面的 Peek 才能一次拿够
	r := bufio.NewReaderSize(f, readBufSize)

	if !opt.Force {
		// Peek 不消费数据，文件比 sniffLen 短时返回 EOF/ErrBufferFull，
		// 但已读到的部分仍然有效，拿来判定足够了。
		head, _ := r.Peek(sniffLen)
		if bytes.IndexByte(head, 0) >= 0 {
			// 用 NUL 判定：git 判断二进制用的也是这个启发式。
			// 文本文件里几乎不可能出现 NUL，而各种可执行格式开头就有。
			return 0, fmt.Errorf("%s 像是二进制文件（前 %d 字节里有 NUL 字节）。"+
				"直接打到终端会弄乱显示，确认要看就加 -f", path, len(head))
		}
	}

	if opt.Number {
		return printNumbered(r, out)
	}
	// 不加行号时直接流式拷贝：不受行长限制，也不会把大文件整个读进内存
	return io.Copy(out, r)
}

// printNumbered 逐行加行号输出。
//
// 用 bufio.Reader.ReadString 而不是 bufio.Scanner：Scanner 默认单行上限 64KB，
// 遇到压缩过的 JS、单行 JSON 或长日志会直接报错退出，而这类文件恰恰常常需要看。
func printNumbered(r *bufio.Reader, out io.Writer) (int64, error) {
	w := bufio.NewWriter(out)
	var written int64
	lineNo := 0

	for {
		line, err := r.ReadString('\n')
		if len(line) > 0 {
			lineNo++
			n, werr := fmt.Fprintf(w, "%6d\t%s", lineNo, line)
			written += int64(n)
			if werr != nil {
				return written, werr
			}
			// 末行没有换行符时补一个，否则后面的统计会跟内容粘在一起
			if !strings.HasSuffix(line, "\n") {
				if werr := w.WriteByte('\n'); werr != nil {
					return written, werr
				}
				written++
			}
		}
		if err == io.EOF {
			return written, w.Flush()
		}
		if err != nil {
			w.Flush()
			return written, err
		}
	}
}
