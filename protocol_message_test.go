package actor

import "testing"

func TestProtocolMessageGetters(t *testing.T) {
	loader := NewActorLoader("u")
	user := NewUser(loader)
	msg := NewProtocolMessage(1001, "payload", user)

	if msg.GetMessageID() != 1001 {
		t.Fatalf("unexpected message id")
	}
	if msg.GetMessage().(string) != "payload" {
		t.Fatalf("unexpected payload")
	}
	if msg.GetUser() != user {
		t.Fatalf("unexpected user")
	}
}
