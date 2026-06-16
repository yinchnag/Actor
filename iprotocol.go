package actor

// IMessage is a lightweight message abstraction for this stage.
type IMessage interface{}

type IUser interface {
	SendMessage(IProtocol)
	GetModloader() IModLoader
}

type IProtocol interface {
	GetMessageID() int
	GetMessage() IMessage
	GetUser() IUser
}
