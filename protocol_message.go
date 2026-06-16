package actor

type ProtocolMessage struct {
	messageID int
	message   IMessage
	user      IUser
}

func NewProtocolMessage(messageID int, message IMessage, user IUser) *ProtocolMessage {
	return &ProtocolMessage{messageID: messageID, message: message, user: user}
}

func (that *ProtocolMessage) GetMessageID() int {
	return that.messageID
}

func (that *ProtocolMessage) GetMessage() IMessage {
	return that.message
}

func (that *ProtocolMessage) GetUser() IUser {
	return that.user
}

var _ IProtocol = (*ProtocolMessage)(nil)
