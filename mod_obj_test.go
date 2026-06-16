package actor

import "testing"

type PingMessage struct{}

type testStorage struct {
	saved  int
	loaded int
}

func (that *testStorage) Init() {}
func (that *testStorage) Save() { that.saved++ }
func (that *testStorage) Load() { that.loaded++ }

type EchoMod struct {
	ModObj[*EchoMod]
	Hit int
}

func NewEchoMod() *EchoMod {
	m := &EchoMod{}
	m.Init()
	return m
}

func (that *EchoMod) Add(a, b int) int {
	that.Hit = a + b
	return that.Hit
}

func (that *EchoMod) OnPing(msg PingMessage) {
	_ = msg
	that.Hit++
}

func TestModObjInvokeAndMeta(t *testing.T) {
	RegisterProtocol(1001, PingMessage{})
	m := NewEchoMod()

	out, err := m.Invoke("Add", 2, 3)
	if err != nil {
		t.Fatalf("invoke failed: %v", err)
	}
	if len(out) != 1 || int(out[0].Int()) != 5 {
		t.Fatalf("unexpected invoke result")
	}

	method := m.GetMetaHandler(1001)
	if method != "OnPing" {
		t.Fatalf("unexpected meta handler: %s", method)
	}
}

func TestModObjDirtySave(t *testing.T) {
	m := NewEchoMod()
	s := &testStorage{}
	m.SetStorage(s)
	m.SetDirty(true)
	m.Update(16)

	if s.saved != 1 {
		t.Fatalf("expected save to run once, got %d", s.saved)
	}
	if m.IsDirty() {
		t.Fatalf("dirty flag should be reset")
	}
}
