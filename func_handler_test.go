package actor

import (
	"reflect"
	"testing"
)

func TestFuncHandlerFields(t *testing.T) {
	h := FuncHandler{
		ModName:    "ShopMod",
		MethodName: "Buy",
		Ref:        reflect.ValueOf(func(int) {}),
		MsgID:      1001,
		NumIn:      1,
		NumOut:     0,
	}

	if h.ModName != "ShopMod" || h.MethodName != "Buy" {
		t.Fatalf("unexpected handler meta")
	}
	if h.NumIn != 1 || h.NumOut != 0 || h.MsgID != 1001 {
		t.Fatalf("unexpected handler signature")
	}
}
