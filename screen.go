package main

import (
	"modernc.org/mathutil"
)

func ensureAtLeastLength[T any](slice []T, atLeastLength uint16) []T {
	if len(slice) < int(atLeastLength) {
		slice = append(slice, make([]T, int(atLeastLength)-len(slice))...)
	}
	return slice
}

type Line struct {
	// characters in a Line are represented as strings (and not runes) because we treat escape sequences as parts of
	// their corresponding characters.
	characters []string

	// Track this to not introduce unnecessary line breaks in the output - even if a line doesn't fit on the virtual
	// screen
	endsWithNewline bool

	// TODO: colors
}

func (l *Line) getCharacter(i uint16) string {
	l.characters = ensureAtLeastLength(l.characters, i+1)

	return l.characters[i]
}

func (l *Line) appendToCharacter(i uint16, val string) {
	l.characters = ensureAtLeastLength(l.characters, i+1)

	l.characters[i] += val
}

func (l *Line) setCharacter(i uint16, val string) {
	l.characters = ensureAtLeastLength(l.characters, i+1)

	l.characters[i] = val
}

type Screen struct {
	lines                []Line
	width, height        uint16
	positionX, positionY uint16

	parser EscapeSequenceParser

	queuedScrollbackOutput []byte
}

func (s *Screen) getLine(line uint16) *Line {
	assert("line index is less than total height", line < s.height)

	s.lines = ensureAtLeastLength(s.lines, line+1)
	return &s.lines[line]
}

func (s *Screen) currentLine() *Line {
	return s.getLine(s.positionY)
}

func (s *Screen) scrollDownOneLine() {
	s.sendLineToScrollbackBuffer(s.getLine(0))

	// BIG TODO: this will grow []Lines indefinitely
	s.lines = s.lines[1:]

	s.positionY -= 1
}

func (s *Screen) wrapCurrentLine() {
	s.currentLine().endsWithNewline = false
	if s.positionY >= s.height {
		s.scrollDownOneLine()
	}
	s.positionY += 1
	s.positionX = 0
}

func (s *Screen) outNormalCharacter(b rune) {
	if s.positionX >= s.width {
		s.wrapCurrentLine()
	}
	s.currentLine().setCharacter(s.positionX, string(b))
	s.positionX += 1
}

func (s *Screen) outRelativeMoveCursorVertical(howMany int) {
	assert("unimplemented", howMany == 1)
	// TODO!!!
	s.currentLine().endsWithNewline = true
	if s.positionY >= s.height {
		s.scrollDownOneLine()
	}
	s.positionY += 1
}

func (s *Screen) outRelativeMoveCursorHorizontal(howMany int) {
}

func (s *Screen) outAbsoluteMoveCursorVertical(y int) {
}

func (s *Screen) outAbsoluteMoveCursorHorizontal(x int) {
	s.positionX = uint16(x)
	s.positionX = mathutil.ClampUint16(s.positionX, 0, s.width)
}

func (s *Screen) outDeleteLeft(howMany int) {
	for i := 0; i < howMany; i += 1 {
		if s.positionX <= 0 {
			break
		}
		s.positionX -= 1
		s.currentLine().setCharacter(s.positionX, "")
	}
}

func (s *Screen) outUnhandledEscapeSequence(seq string) {
	// append to the current character but don't move the cursor forward
	s.currentLine().appendToCharacter(s.positionX, seq)
}

func NewScreen(width uint16, height uint16) *Screen {
	screen := &Screen{width: width, height: height}
	screen.parser = NewEscapeSequenceParser(screen)
	return screen
}

func (s *Screen) Advance(b []byte) {
	s.parser.Advance(b)
}

func (s *Screen) Resize(width, height uint16) {
	// todo
}

func (s *Screen) appendToScrollback(str string) {
	s.queuedScrollbackOutput = append(s.queuedScrollbackOutput, []byte(str)...)
}

func (s *Screen) sendLineToScrollbackBuffer(line *Line) {
	for _, character := range line.characters {
		s.appendToScrollback(character)
	}
	if line.endsWithNewline {
		s.appendToScrollback("\n")
	}
}

func (s *Screen) End() {
	for _, line := range s.lines {
		s.sendLineToScrollbackBuffer(&line)
	}
	s.lines = []Line{}
}
