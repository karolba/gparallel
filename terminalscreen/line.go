package terminalscreen

type Line struct {
	index int

	// characters in a Line are represented as strings (and not runes) because we treat escape sequences as parts of
	// their corresponding characters.
	characters []Character
}

func NewLine(index int) *Line {
	return &Line{index: index}
}

func (l *Line) characterAt(i int) *Character {
	l.characters = ensureAtLeastLength(l.characters, i+1)
	return &l.characters[i]
}

func (l *Line) lengthWithoutTrailingSpacesAndEmptyRunes() int {
	var emptyRune rune

	for i := len(l.characters) - 1; i >= 0; i-- {
		if l.characters[i].rune == ' ' {
			continue
		}
		if l.characters[i].rune == emptyRune {
			continue
		}
		if l.characters[i].extraEscapeSequences != "" {
			continue
		}
		return i + 1
	}
	return 0
}
