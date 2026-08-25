#!/usr/bin/env python3
"""把 cmd/gercmd 编译成可执行文件。

用法:
    python build.py                      # 编译当前平台
    python build.py --strip              # 去掉调试信息，体积更小
    python build.py --os linux --arch amd64
    python build.py --all                # 常见平台各编一份
    python build.py --test               # 先跑测试再编译
    python build.py -o D:/tools/gercmd.exe
"""

import argparse
import os
import shutil
import subprocess
import sys
from pathlib import Path

# 本机控制台默认是 GBK，直接 print 中文会抛 UnicodeEncodeError 或输出乱码。
# 这一步必须在任何输出之前做。reconfigure 是 Python 3.7+ 的接口，
# 老版本就退回原样——顶多乱码，不至于让编译脚本本身崩掉。
for stream in (sys.stdout, sys.stderr):
    try:
        stream.reconfigure(encoding="utf-8")
    except (AttributeError, ValueError):
        pass

# 脚本在 cmd/gercmd/ 下，往上两级就是仓库根
SCRIPT_DIR = Path(__file__).resolve().parent
REPO_ROOT = SCRIPT_DIR.parent.parent
PKG = "./cmd/gercmd"
BIN_NAME = "gercmd"

# --all 覆盖的目标。够用即可，要别的用 --os/--arch 单独指定。
ALL_TARGETS = [
    ("windows", "amd64"),
    ("linux", "amd64"),
    ("linux", "arm64"),
    ("darwin", "amd64"),
    ("darwin", "arm64"),
]


def die(msg, code=2):
    print(f"错误: {msg}", file=sys.stderr)
    sys.exit(code)


def human_size(n):
    for unit in ("B", "KB", "MB", "GB"):
        if n < 1024 or unit == "GB":
            return f"{n:.1f} {unit}" if unit != "B" else f"{n} B"
        n /= 1024.0


def run(cmd, env=None, verbose=False):
    """在仓库根目录下执行命令，失败时返回非零。"""
    if verbose:
        print("  $", " ".join(cmd))
    # cwd 固定在仓库根：go build 的包路径是相对模块根解析的，
    # 从别处调用这个脚本时才不会找不到包。
    return subprocess.call(cmd, cwd=str(REPO_ROOT), env=env)


def check_env():
    if shutil.which("go") is None:
        die("找不到 go，请确认它在 PATH 里")
    if not (REPO_ROOT / "go.mod").is_file():
        die(f"{REPO_ROOT} 下没有 go.mod——脚本可能被挪出 cmd/gercmd/ 了")
    ver = subprocess.run(
        ["go", "version"], cwd=str(REPO_ROOT),
        capture_output=True, text=True,
    )
    return ver.stdout.strip() if ver.returncode == 0 else "go (版本未知)"


def build_one(goos, goarch, out_path, strip, verbose):
    """编一份出来。goos/goarch 传 None 表示用本机默认值。"""
    env = os.environ.copy()
    if goos:
        env["GOOS"] = goos
    if goarch:
        env["GOARCH"] = goarch
    # 交叉编译时关掉 cgo：本机通常没有目标平台的 C 交叉工具链，
    # 开着会在链接阶段莫名其妙地失败。本工具是纯 Go，关掉没有副作用。
    if goos or goarch:
        env["CGO_ENABLED"] = "0"

    # -trimpath 把本机的绝对路径从二进制里剥掉：既是可复现构建的前提，
    # 也避免把 D:/cloud/Actor 这种本地路径随二进制发出去。
    cmd = ["go", "build", "-trimpath"]
    if strip:
        # -s 去符号表，-w 去 DWARF 调试信息。体积能小三成左右，
        # 代价是崩溃栈没有行号、dlv 也调不了，所以不做默认。
        cmd += ["-ldflags", "-s -w"]
    cmd += ["-o", str(out_path), PKG]

    label = f"{goos or '本机'}/{goarch or ''}".rstrip("/")
    print(f"编译 {label} ...")
    if run(cmd, env=env, verbose=verbose) != 0:
        return False

    if not out_path.is_file():
        print(f"  go build 报告成功，但没找到 {out_path}", file=sys.stderr)
        return False
    print(f"  ok  {out_path}  ({human_size(out_path.stat().st_size)})")
    return True


def exe_suffix(goos):
    """目标是 Windows 就带 .exe。按目标平台判断，不是按本机平台。"""
    target = goos or sys.platform
    return ".exe" if target.startswith("win") else ""


def main():
    ap = argparse.ArgumentParser(
        description="把 cmd/gercmd 编译成可执行文件",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=__doc__,
    )
    ap.add_argument("-o", "--output", help="输出路径（默认：脚本所在目录）")
    ap.add_argument("--os", dest="goos", help="目标 GOOS，如 linux、windows、darwin")
    ap.add_argument("--arch", dest="goarch", help="目标 GOARCH，如 amd64、arm64")
    ap.add_argument("--all", action="store_true", help="为常见平台各编一份")
    ap.add_argument("--strip", action="store_true",
                    help="去掉符号表与调试信息，体积更小但没法调试")
    ap.add_argument("--test", action="store_true", help="编译前先跑一遍测试")
    ap.add_argument("-v", "--verbose", action="store_true", help="打印实际执行的命令")
    args = ap.parse_args()

    if args.all and (args.output or args.goos or args.goarch):
        die("--all 会产出多个文件，不能和 -o/--os/--arch 一起用")

    print(check_env())
    print(f"仓库根 {REPO_ROOT}")

    if args.test:
        print("跑测试 ...")
        if run(["go", "test", PKG, "-count=1"], verbose=args.verbose) != 0:
            die("测试没过，已中止编译", code=1)

    if args.all:
        ok = 0
        for goos, goarch in ALL_TARGETS:
            out = SCRIPT_DIR / f"{BIN_NAME}_{goos}_{goarch}{exe_suffix(goos)}"
            if build_one(goos, goarch, out, args.strip, args.verbose):
                ok += 1
        print(f"\n{ok}/{len(ALL_TARGETS)} 个平台编译成功")
        sys.exit(0 if ok == len(ALL_TARGETS) else 1)

    if args.output:
        out = Path(args.output)
        if not out.is_absolute():
            # 相对路径按调用者的当前目录解析，而不是脚本目录——
            # 命令行里敲 -o build/gercmd 时，人期望的是自己所在的位置
            out = Path.cwd() / out
    else:
        out = SCRIPT_DIR / f"{BIN_NAME}{exe_suffix(args.goos)}"

    out.parent.mkdir(parents=True, exist_ok=True)
    sys.exit(0 if build_one(args.goos, args.goarch, out, args.strip, args.verbose) else 1)


if __name__ == "__main__":
    main()
