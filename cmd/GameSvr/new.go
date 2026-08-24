package main

import (
	"actor"
	"actor/cmd/GameSvr/bag"
	"actor/cmd/GameSvr/hero"
	"actor/cmd/GameSvr/player"
)

func NewPlayerEnt(name string) *player.PlayerEnt {
	loader := actor.NewActorLoader(name)
	return &player.PlayerEnt{
		User: actor.NewUser(loader),
	}
}

func test() {
	p := NewPlayerEnt("playerName-zhangsan")
	p.GetModloader().AddModule(bag.NewBagMod(p))
	p.GetModloader().AddModule(hero.NewHeroMod(p))
}
