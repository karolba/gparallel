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

type EscapeSequenceParserOutput struct {
	outNormalCharacter              func(rune)
	outRelativeMoveCursorVertical   func(howMany int)
	outRelativeMoveCursorHorizontal func(howMany int)
	outAbsoluteMoveCursorVertical   func(howMany int)
	outAbsoluteMoveCursorHorizontal func(howMany int)
	outDeleteLeft                   func(howMany int)
	outUnhandledEscapeSequence      func(string)
}

func NewEscapeSequenceParser(outOpts *EscapeSequenceParserOutput) EscapeSequenceParser {
	return EscapeSequenceParser{vteParser: vte.NewParser(outOpts)}
}

func (escapeSequenceParser EscapeSequenceParser) Advance(bytes []byte) {
	for _, b := range bytes {
		escapeSequenceParser.vteParser.Advance(b)
	}
}

// Draw a character to the screen and update states
func (p *EscapeSequenceParserOutput) Print(r rune) {
	//if r == ' ' {
	//	log.Printf("[Print] space\n")
	//} else {
	//	log.Printf("[Print] '%c'\n", r)
	//}

	p.outNormalCharacter(r)
}

// Execute a C0 or C1 control function
func (p *EscapeSequenceParserOutput) Execute(b byte) {
	if b == '\t' {
		// TODO: this... it's not even a tab
		p.outNormalCharacter(' ')
		p.outNormalCharacter(' ')
		p.outNormalCharacter(' ')
		p.outNormalCharacter(' ')
		p.outNormalCharacter(' ')
		p.outNormalCharacter(' ')
		p.outNormalCharacter(' ')
		p.outNormalCharacter(' ')
		//log.Printf("[Execute] tab\n")
	} else if b == '\n' {
		log.Printf("[Execute] newline\n")
	} else if b == '\v' {
		log.Printf("[Execute] vertical tab\n")
	} else if b == '\r' {
		//log.Printf("[Execute] carriage return\n")
		p.outAbsoluteMoveCursorHorizontal(0)
	} else if b == '\b' {
		p.outDeleteLeft(1)
		//log.Printf("[Execute] delete\n")
	} else {
		log.Printf("[Execute] %02x\n", b)
	}

	fmt.Printf("%c", b)
}

// Pass bytes as part of a device control string to the handle chosen in hook. C0 controls will also be passed to the handler.
func (p *EscapeSequenceParserOutput) Put(b byte) {
	p.outUnhandledEscapeSequence(string(b))
	//log.Printf("[Put] %02x %c\n", b, rune(b))

	//fmt.Printf("%c", b)
}

// Called when a device control string is terminated.
// The previously selected handler should be notified that the DCS has terminated.
func (p *EscapeSequenceParserOutput) Unhook() {
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
func (p *EscapeSequenceParserOutput) Hook(params [][]uint16, intermediates []byte, ignore bool, final rune) {
	//log.Printf("[Hook] params=%v, intermediates=%v, ignore=%v, r=%c\n", params, intermediates, ignore, final)
	privateMarkers, realIntemediates := splitIntermediates(intermediates)

	p.outUnhandledEscapeSequence(fmt.Sprintf("%s%s%s%s%c",
		DCS_START,
		privateMarkers,
		paramsToString(params),
		realIntemediates,
		final))
}

// Dispatch an operating system command.
func (p *EscapeSequenceParserOutput) OscDispatch(params [][]byte, bellTerminated bool) {
	//log.Printf("[OscDispatch] params=%v, bellTerminated=%v\n", params, bellTerminated)
	p.outUnhandledEscapeSequence(fmt.Sprintf("%s%s",
		OSC_START,
		bytes.Join(params, []byte{';'})))
}

// A final character has arrived for a CSI sequence
//
// The ignore flag indicates that either more than two intermediates arrived or the number of parameters exceeded
// the maximum supported length, and subsequent characters were ignored.
func (p *EscapeSequenceParserOutput) CsiDispatch(params [][]uint16, intermediates []byte, ignore bool, final rune) {
	privateMarkers, realIntemediates := splitIntermediates(intermediates)
	s := fmt.Sprintf("%s%s%s%s%c", CSI_START, privateMarkers, paramsToString(params), realIntemediates, final)

	//log.Printf("[CsiDispatch] params=%v, intermediates=%v, ignore=%v, r=%c, guessed=%s\n", params, intermediates, ignore, final, s[1:])
	p.outUnhandledEscapeSequence(s)
}

// The final character of an escape sequence has arrived.
// The ignore flag indicates that more than two intermediates arrived and subsequent characters were ignored.
func (p *EscapeSequenceParserOutput) EscDispatch(intermediates []byte, ignore bool, final byte) {
	//log.Printf("[EscDispatch] intermediates=%v, ignore=%v, byte=%02x\n", intermediates, ignore, final)

	p.outUnhandledEscapeSequence(fmt.Sprintf("%s%s%c",
		ESC,
		intermediates,
		final))
}
