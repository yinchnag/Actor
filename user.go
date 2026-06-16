package actor

import "sync"

type User struct {
	loader IModLoader
	mu     sync.Mutex
	sent   []IProtocol
}

func NewUser(loader IModLoader) *User {
	return &User{loader: loader}
}

func (that *User) SendMessage(msg IProtocol) {
	that.mu.Lock()
	defer that.mu.Unlock()
	that.sent = append(that.sent, msg)
}

func (that *User) GetModloader() IModLoader {
	return that.loader
}

func (that *User) SentCount() int {
	that.mu.Lock()
	defer that.mu.Unlock()
	return len(that.sent)
}

var _ IUser = (*User)(nil)
