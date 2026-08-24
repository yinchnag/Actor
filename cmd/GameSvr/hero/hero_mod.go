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

func (that *HeroMod) AddHero(heroid int) {}
