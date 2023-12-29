package terminalscreen

type Character struct {
	rune rune

	// TODO: dedup those
	extraEscapeSequences string

	sgr SGRList
}

func (c *Character) addSGR(sgr SelectGraphicRenditionAttribute) {
	if sgr.isUnsetAll() {
		c.sgr = []SelectGraphicRenditionAttribute{}
	} else {
		sgr.addToSGRAttributeList(&c.sgr)
	}
}

func (c *Character) clearSGR() {
	c.sgr = []SelectGraphicRenditionAttribute{}
}
