package player

func (that *PlayerEnt) HeroModAddHero(heroid int) {
	if _, err := that.GetModloader().ModInvoke("HeroMod", "AddHero", heroid); err != nil {
		println("HeroModAddHero err:", err.Error())
	}
}
