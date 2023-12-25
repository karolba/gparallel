package main

type Line struct {
	// characters in a Line are represented as strings (and not runes) because we treat escape sequences as parts of
	// their corresponding characters.
	characters []string

	// Track this to not introduce unnecessary line breaks in the output - even if a line doesn't fit on the virtual
	// screen
	isLineWrappedFromPreviousLine bool

	// TODO: colors
}

type Screen struct {
	lines                []Line
	width, height        uint16
	positionX, positionY uint16

	parser EscapeSequenceParser

	queuedScrollbackOutput []byte
}

func NewScreen(width uint16, height uint16) *Screen {
	screen := &Screen{width: width, height: height}
	screen.parser = NewEscapeSequenceParser(&EscapeSequenceParserOutput{
		outNormalCharacter:              nil,
		outRelativeMoveCursorVertical:   nil,
		outRelativeMoveCursorHorizontal: nil,
		outAbsoluteMoveCursorVertical:   nil,
		outAbsoluteMoveCursorHorizontal: nil,
		outDeleteLeft:                   nil,
		outUnhandledEscapeSequence:      nil,
	})
	return screen
}

func (s *Screen) Advance(b []byte) {
	s.parser.Advance(b)
}

func (s *Screen) Resize(width, height uint16) {
	// todo
}

func (s *Screen) End() {
	// todo
}
