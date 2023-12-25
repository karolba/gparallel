package main

import (
	"bytes"
	"fmt"
	"log"
	"strconv"

	"github.com/danielgatis/go-vte"
)

// https://vt100.net/docs/vt510-rm/chapter4.html
const ESC = "\033"
const OSC_START = "\033]" // "Operating system command"
const CSI_START = "\033[" // "Control sequence introducer"
const DCS_START = "\033P" // "Device control string"

type EscapeSequenceParser struct {
	vteParser *vte.Parser
}

type EscapeSequenceParserOutput interface {
	outNormalCharacter(b rune)
	outRelativeMoveCursorVertical(howMany int)
	outRelativeMoveCursorHorizontal(howMany int)
	outAbsoluteMoveCursorVertical(y int)
	outAbsoluteMoveCursorHorizontal(x int)
	outDeleteLeft(howMany int)
	outUnhandledEscapeSequence(s string)
}

type vtePerformer struct{ out EscapeSequenceParserOutput }

func NewEscapeSequenceParser(outOpts EscapeSequenceParserOutput) EscapeSequenceParser {
	return EscapeSequenceParser{vteParser: vte.NewParser(&vtePerformer{
		out: outOpts,
	})}
}

func (escapeSequenceParser EscapeSequenceParser) Advance(bytes []byte) {
	for _, b := range bytes {
		escapeSequenceParser.vteParser.Advance(b)
	}
}

// Draw a character to the screen and update states
func (p *vtePerformer) Print(r rune) {
	//if r == ' ' {
	//	log.Printf("[Print] space\n")
	//} else {
	//	log.Printf("[Print] '%c'\n", r)
	//}

	p.out.outNormalCharacter(r)
}

// Execute a C0 or C1 control function
func (p *vtePerformer) Execute(b byte) {
	if b == '\t' {
		// TODO: this... it's not even a tab
		p.out.outNormalCharacter(' ')
		p.out.outNormalCharacter(' ')
		p.out.outNormalCharacter(' ')
		p.out.outNormalCharacter(' ')
		p.out.outNormalCharacter(' ')
		p.out.outNormalCharacter(' ')
		p.out.outNormalCharacter(' ')
		p.out.outNormalCharacter(' ')
		//log.Printf("[Execute] tab\n")
	} else if b == '\n' {
		p.out.outAbsoluteMoveCursorHorizontal(0)
		p.out.outRelativeMoveCursorVertical(1)
	} else if b == '\v' {
		log.Printf("[Execute] TODO: vertical tab\n")
	} else if b == '\r' {
		p.out.outAbsoluteMoveCursorHorizontal(0)
	} else if b == '\b' {
		p.out.outDeleteLeft(1)
		//log.Printf("[Execute] delete\n")
	} else {
		log.Printf("TODO: Execute '%c' (%b)\n", b, b)
	}

	//fmt.Printf("TODO: Execute '%c'", b)
}

// Pass bytes as part of a device control string to the handle chosen in hook. C0 controls will also be passed to the handler.
func (p *vtePerformer) Put(b byte) {
	p.out.outUnhandledEscapeSequence(string(b))
	//log.Printf("[Put] %02x %c\n", b, rune(b))

	//fmt.Printf("%c", b)
}

// Called when a device control string is terminated.
// The previously selected handler should be notified that the DCS has terminated.
func (p *vtePerformer) Unhook() {
	//log.Printf("[Unhook]\n")
}

func paramsToString[T uint16 | byte](params [][]T) string {
	var joinedParams bytes.Buffer
	for i, paramSet := range params {
		if i != 0 {
			joinedParams.WriteByte(';')
		}

		for j, param := range paramSet {
			if j != 0 {
				joinedParams.WriteByte(':')
			}
			joinedParams.WriteString(strconv.Itoa(int(param)))
		}
	}
	return joinedParams.String()
}

func splitIntermediates(intermediates []byte) (privateMarkers []byte, realIntemediates []byte) {
	for _, b := range intermediates {
		if b >= 0x30 && b <= 0x3f {
			privateMarkers = append(privateMarkers, b)
		} else {
			realIntemediates = append(realIntemediates, b)
		}
	}
	return privateMarkers, realIntemediates
}

// Invoked when a final character arrives in first part of device control string.
//
// The control function should be determined from the private marker, final character, and execute with a parameter list.
// A handler should be selected for remaining characters in the string; the handler function should subsequently be called
// by put for every character in the control string.
//
// The ignore flag indicates that more than two intermediates arrived and subsequent characters were ignored.
func (p *vtePerformer) Hook(params [][]uint16, intermediates []byte, ignore bool, final rune) {
	//log.Printf("[Hook] params=%v, intermediates=%v, ignore=%v, r=%c\n", params, intermediates, ignore, final)
	privateMarkers, realIntemediates := splitIntermediates(intermediates)

	p.out.outUnhandledEscapeSequence(fmt.Sprintf("%s%s%s%s%c",
		DCS_START,
		privateMarkers,
		paramsToString(params),
		realIntemediates,
		final))
}

// Dispatch an operating system command.
func (p *vtePerformer) OscDispatch(params [][]byte, bellTerminated bool) {
	//log.Printf("[OscDispatch] params=%v, bellTerminated=%v\n", params, bellTerminated)
	p.out.outUnhandledEscapeSequence(fmt.Sprintf("%s%s",
		OSC_START,
		bytes.Join(params, []byte{';'})))
}

func numericParams(input [][]uint16) []int {
	// TODO: make this nicer
	var result []int

	for _, row := range input {
		var value int
		for _, digit := range row {
			value = value*10 + int(digit)
		}
		result = append(result, value)
	}

	if len(result) == 0 {
		return []int{0}
	}
	return result
}

// A final character has arrived for a CSI sequence
//
// The ignore flag indicates that either more than two intermediates arrived or the number of parameters exceeded
// the maximum supported length, and subsequent characters were ignored.
func (p *vtePerformer) CsiDispatch(params [][]uint16, intermediates []byte, ignore bool, final rune) {
	privateMarkers, realIntemediates := splitIntermediates(intermediates)

	// Cursor Up (CUU): ESC [ Ⓝ A - https://terminalguide.namepad.de/seq/csi_ca/
	if bytes.Equal(intermediates, []byte{}) && final == 'A' {
		p.out.outRelativeMoveCursorVertical(-numericParams(params)[0])
		return
	}

	// Cursor Down (CUD): ESC [ Ⓝ B - https://terminalguide.namepad.de/seq/csi_cb/
	if bytes.Equal(intermediates, []byte{}) && final == 'B' {
		p.out.outRelativeMoveCursorVertical(numericParams(params)[0])
		return
	}

	// Cursor Right (CUF): ESC [ Ⓝ C - https://terminalguide.namepad.de/seq/csi_cc/
	if bytes.Equal(intermediates, []byte{}) && final == 'C' {
		p.out.outRelativeMoveCursorHorizontal(numericParams(params)[0])
		return
	}

	// Cursor Left (CUB): ESC [ Ⓝ D - https://terminalguide.namepad.de/seq/csi_cd/
	if bytes.Equal(intermediates, []byte{}) && final == 'D' {
		p.out.outRelativeMoveCursorHorizontal(-numericParams(params)[0])
		return
	}

	// Set Cursor Position (CUP): ESC [ Ⓝ ; Ⓝ H - https://terminalguide.namepad.de/seq/csi_ch/
	if bytes.Equal(intermediates, []byte{}) && final == 'H' {
		coords := numericParams(params)
		// The coordinates in here are 1-based, but we use 0-based coordinates - hence the minus one
		x := getOrDefault(coords, 0) - 1
		y := getOrDefault(coords, 1) - 1
		p.out.outAbsoluteMoveCursorHorizontal(x)
		p.out.outAbsoluteMoveCursorHorizontal(y)
		return
	}

	// Cursor Horizontal Position Absolute (HPA) (TODO more details)
	if bytes.Equal(intermediates, []byte{}) && (final == 'G' || final == '`') {
		// The coordinates in here are 1-based, but we use 0-based coordinates - hence the minus one
		p.out.outAbsoluteMoveCursorHorizontal(numericParams(params)[0] - 1)
		return
	}

	// Cursor Vertical Position Absolute (VPA) (TODO more details)
	if bytes.Equal(intermediates, []byte{}) && final == 'd' {
		// The coordinates in here are 1-based, but we use 0-based coordinates - hence the minus one
		p.out.outAbsoluteMoveCursorVertical(numericParams(params)[0] - 1)
		return
	}

	log.Printf("[UnhandledCsiDispatch] params=%v, intermediates=%v, ignore=%v, r=%c\n", params, intermediates, ignore, final)

	p.out.outUnhandledEscapeSequence(fmt.Sprintf("%s%s%s%s%c",
		CSI_START,
		privateMarkers,
		paramsToString(params),
		realIntemediates,
		final))
}

// The final character of an escape sequence has arrived.
// The ignore flag indicates that more than two intermediates arrived and subsequent characters were ignored.
func (p *vtePerformer) EscDispatch(intermediates []byte, ignore bool, final byte) {
	//log.Printf("[EscDispatch] intermediates=%v, ignore=%v, byte=%02x\n", intermediates, ignore, final)

	p.out.outUnhandledEscapeSequence(fmt.Sprintf("%s%s%c",
		ESC,
		intermediates,
		final))
}
