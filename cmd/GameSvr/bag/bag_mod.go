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

func (that *BagMod) AddItem(itemid, count int) {

}

func (that *BagMod) RemoveItem(itemid, count int, cb func()) {

}
