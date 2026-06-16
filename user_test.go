package actor

import "testing"

func TestUserSendMessage(t *testing.T) {
	loader := NewActorLoader("u")
	user := NewUser(loader)

	msg := NewProtocolMessage(1, "hello", user)
	user.SendMessage(msg)

	if user.SentCount() != 1 {
		t.Fatalf("expected one sent message")
	}
	if user.GetModloader() != loader {
		t.Fatalf("unexpected loader")
	}
}
