package service

import (
	"fmt"
	"reflect"
)

// unwrap 把框架的 ([]reflect.Value, error) 还原成 (T, error)。
//
// 它是生成的门面文件唯一依赖的手写函数。抽出来而不是让模板把这段逻辑
// 铺进每个门面，有两个理由：生成的文件不必再导入 fmt/reflect（模板没法
// 往 .Imports 里塞东西），以及这段逻辑本身有三处容易写错、值得被单测覆盖。
//
// 三处不能省：
//
//  1. 两层错误都要看。invokeErr 是投递/调度层面的失败，out[1] 才是模块方法
//     自己返回的业务错误——漏掉哪一层，都会让一类失败变成"成功返回零值"。
//  2. 返回值个数要独立校验。它和 err 是两种不同的故障：前者意味着模块方法
//     的签名和门面对不上（多半是改了签名没重新生成）。
//  3. 类型断言用逗号 ok。门面跑在 HTTP 协程上，不是 actor 协程，
//     断言失败 panic 掉的是请求处理协程。
func unwrap[T any](out []reflect.Value, invokeErr error, what string) (T, error) {
	var zero T
	if invokeErr != nil {
		return zero, invokeErr
	}
	if len(out) != 2 {
		return zero, fmt.Errorf("%s 返回值个数异常: %d，门面与模块签名不一致（改了签名要重新生成）", what, len(out))
	}
	if bizErr, _ := out[1].Interface().(error); bizErr != nil {
		return zero, bizErr
	}
	v, ok := out[0].Interface().(T)
	if !ok {
		return zero, fmt.Errorf("%s 返回类型异常: %T", what, out[0].Interface())
	}
	return v, nil
}
