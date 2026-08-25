package bag

import (
	"actor"
	"actor/cmd/GameSvr/player"
)

func NewBagMod(p *player.PlayerEnt) actor.IModule {
	return &BagMod{
		host: p,
	}
}

type BagMod struct {
	host *player.PlayerEnt
	actor.ModObj[*BagMod]
}

// 增加物品
//	export: BagAddItem
func (that *BagMod) AddItem(itemid, count int) {

}

// 删除物品
//	export: BagRemoveItem
func (that *BagMod) RemoveItem(itemid, count int, cb func()) {

}
