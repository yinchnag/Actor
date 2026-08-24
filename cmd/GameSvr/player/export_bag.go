package player

// 增加物品
func (that *PlayerEnt) BagAddItem(itemid, count int) {
	if _, err := that.GetModloader().ModInvoke("BagMod", "AddItem", itemid, count); err != nil {
		println("BagAddItem err:", err.Error())
	}
}

func (that *PlayerEnt) BagRemoveItem(itemid, count int, cb func()) {
	if _, err := that.GetModloader().ModInvoke("BagMod", "RemoveItem", itemid, count, cb); err != nil {
		println("BagRemoveItem err:", err.Error())
	}
}
