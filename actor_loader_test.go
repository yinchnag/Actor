package actor

import (
	"sync"
	"testing"
	"time"
)

type CalcMessage struct {
	A int
	B int
}

type CalcMod struct {
	ModObj[*CalcMod]
	Last int
}

func NewCalcMod() *CalcMod {
	m := &CalcMod{}
	m.Init()
	return m
}

func (that *CalcMod) Sum(a, b int) int {
	return a + b
}

func (that *CalcMod) OnCalc(msg CalcMessage) {
	that.Last = msg.A + msg.B
}

func TestActorLoaderModInvoke(t *testing.T) {
	loader := NewActorLoader("user")
	loader.Init()
	mod := NewCalcMod()
	loader.AddModule(mod)

	out, err := loader.ModInvoke("CalcMod", "Sum", 4, 6)
	if err != nil {
		t.Fatalf("mod invoke failed: %v", err)
	}
	if len(out) != 1 || int(out[0].Int()) != 10 {
		t.Fatalf("unexpected invoke result")
	}
}

func TestActorLoaderCrossGoroutineInvoke(t *testing.T) {
	owner := NewActorLoader("owner")
	caller := NewActorLoader("caller")
	owner.Init()
	caller.Init()

	mod := NewCalcMod()
	owner.AddModule(mod)

	var wg sync.WaitGroup
	wg.Add(1)
	go owner.RunUpdateLoop(&wg)

	time.Sleep(20 * time.Millisecond)
	// Invoke is sent to owner's goroutine via channel (test goroutine != owner's goroutine).
	out, err := owner.ModInvoke("CalcMod", "Sum", 7, 8)
	if err != nil {
		owner.Close()
		wg.Wait()
		t.Fatalf("cross invoke failed: %v", err)
	}
	if len(out) != 1 || int(out[0].Int()) != 15 {
		owner.Close()
		wg.Wait()
		t.Fatalf("unexpected cross invoke result")
	}

	owner.Close()
	wg.Wait()
}

func TestActorLoaderOnMessageHandler(t *testing.T) {
	RegisterProtocol(3001, CalcMessage{})
	loader := NewActorLoader("user")
	loader.Init()
	mod := NewCalcMod()
	loader.AddModule(mod)

	msg := NewProtocolMessage(3001, CalcMessage{A: 3, B: 9}, nil)
	loader.OnMessageHandler(msg)

	if mod.Last != 12 {
		t.Fatalf("unexpected message dispatch result: %d", mod.Last)
	}
}
