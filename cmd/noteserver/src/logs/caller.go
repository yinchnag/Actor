package logs

import (
	"runtime"
	"strings"

	"github.com/sirupsen/logrus"
	utils "github.com/yinchnag/GCore"
)

// 修正 caller。
//
// logrus 的 ReportCaller 是"跳过 logrus 自己的帧，取下一帧"，于是本包这层薄包装
// 就成了它眼里的调用方——所有日志的位置都会显示成 logs.go 的某一行，
// 定位能力当场归零。日志失去定位价值，比没有包装更糟。
//
// 所以在这里挂一个 hook 把 entry.Caller 改回真正的调用点。两个细节：
//
//   - **只跳过 logrus 与本包的帧**，不跳 GCore 的。GCore 自己也用这个 logger
//     打日志（比如设置时区那条），跳掉它的帧会让那些日志指向更外层，同样是错的。
//   - **必须排在其它 hook 前面**。GCore 用 lfshook 把日志写文件，那也是一个 hook；
//     logrus 按注册顺序触发，我们后注册的话，文件里留下的仍是错的 caller，
//     只有控制台是对的——那种"两份日志对不上"的状态最难排查。
//     logrus 没有插队的 API，所以直接改 Hooks 这个 map。
func init() {
	h := callerFix{}
	for _, lv := range logrus.AllLevels {
		utils.Logger.Hooks[lv] = append([]logrus.Hook{h}, utils.Logger.Hooks[lv]...)
	}
}

type callerFix struct{}

func (callerFix) Levels() []logrus.Level { return logrus.AllLevels }

func (callerFix) Fire(e *logrus.Entry) error {
	// ReportCaller 关掉时 entry.Caller 是 nil，格式化器也不会打位置，不用管
	if e.Caller == nil {
		return nil
	}
	if f := realCaller(); f != nil {
		e.Caller = f
	}
	return nil
}

// realCaller 往外走，找到第一个既不属于 logrus、也不属于本包的帧。
func realCaller() *runtime.Frame {
	var pcs [24]uintptr
	n := runtime.Callers(1, pcs[:])
	frames := runtime.CallersFrames(pcs[:n])
	for {
		f, more := frames.Next()
		if !skipFrame(f.Function) {
			return &f
		}
		if !more {
			return nil
		}
	}
}

func skipFrame(fn string) bool {
	return strings.Contains(fn, "github.com/sirupsen/logrus") ||
		strings.Contains(fn, "noteserver/src/logs")
}
