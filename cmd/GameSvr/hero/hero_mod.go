package hero

import (
	"actor"
	"actor/cmd/GameSvr/player"
)

func NewHeroMod(p *player.PlayerEnt) actor.IModule {
	return &HeroMod{
		host: p,
	}
}

type HeroMod struct {
	host *player.PlayerEnt
	actor.ModObj[*HeroMod]
}

// 新增一个英雄
//	export: HeroModAddHero
func (that *HeroMod) AddHero(heroid int) {}
