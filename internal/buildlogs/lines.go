package buildlogs

import (
	"io"
	"strconv"
	"strings"
)

// controlStripper removes terminal control sequences and control characters
// (except tab, line feed and carriage return) from a byte stream. It runs
// before redaction, so a known value split by colour codes is still found, and
// it keeps its state across writes.
type controlStripper struct {
	dst   io.Writer
	state stripState
	run   int // bytes swallowed by the current escape string
	out   []byte
}

type stripState uint8

const (
	stripText         stripState = iota
	stripEscape                  // after ESC
	stripIntermediate            // ESC followed by 0x20-0x2F bytes
	stripCSI                     // ESC [ ... final byte 0x40-0x7E
	stripString                  // OSC/DCS/SOS/PM/APC until BEL or ESC \
	stripStringEscape            // ESC inside a string
)

const (
	// maxEscapeString abandons an unterminated escape string, so output can
	// never be hidden indefinitely by one.
	maxEscapeString    = 4096
	escapeByte         = 0x1b
	stringTerminator   = 0x07
	csiFinalLowBound   = 0x40
	csiFinalHighBound  = 0x7e
	intermediateLow    = 0x20
	intermediateHigh   = 0x2f
	firstPrintableByte = 0x20
	deleteByte         = 0x7f
)

func (s *controlStripper) Write(p []byte) (int, error) {
	s.out = s.out[:0]
	for _, c := range p {
		s.step(c)
	}
	if len(s.out) > 0 {
		if _, err := s.dst.Write(s.out); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func (s *controlStripper) step(c byte) {
	switch s.state {
	case stripText:
		s.text(c)
	case stripEscape:
		switch {
		case c == '[':
			s.state = stripCSI
		case c == ']' || c == 'P' || c == 'X' || c == '^' || c == '_':
			s.state, s.run = stripString, 0
		case c >= intermediateLow && c <= intermediateHigh:
			s.state = stripIntermediate
		case c < firstPrintableByte:
			s.state = stripText
			s.text(c)
		default:
			s.state = stripText // A two-byte sequence such as ESC 7.
		}
	case stripIntermediate:
		switch {
		case c >= intermediateLow && c <= intermediateHigh:
		case c < firstPrintableByte:
			s.state = stripText
			s.text(c)
		default:
			s.state = stripText
		}
	case stripCSI:
		switch {
		case c >= csiFinalLowBound && c <= csiFinalHighBound:
			s.state = stripText
		case c < firstPrintableByte:
			// A control byte aborts the sequence; a line break must survive.
			s.state = stripText
			s.text(c)
		}
	case stripString:
		s.run++
		switch {
		case c == stringTerminator:
			s.state = stripText
		case c == escapeByte:
			s.state = stripStringEscape
		case c == '\n' || s.run > maxEscapeString:
			s.state = stripText
			s.text(c)
		}
	case stripStringEscape:
		if c == '\\' {
			s.state = stripText
		} else {
			s.state = stripString
		}
	}
}

func (s *controlStripper) text(c byte) {
	switch {
	case c == escapeByte:
		s.state = stripEscape
	case c == '\t' || c == '\n' || c == '\r' || (c >= firstPrintableByte && c != deleteByte):
		s.out = append(s.out, c)
	}
}

// lineSplitter turns redacted text into bounded lines. CRLF and LF end a line;
// a lone CR keeps only the text after it, as a terminal would for progress bars.
// Invalid UTF-8 becomes U+FFFD. A line longer than maxBytes is cut with a marker
// and the rest of it is discarded and counted, so memory stays bounded.
type lineSplitter struct {
	maxBytes int
	emit     func(text string)
	buf      []byte
	cut      bool
	dropped  int64
	cr       bool
}

func (s *lineSplitter) Write(p []byte) (int, error) {
	for _, c := range p {
		if s.cr {
			s.cr = false
			if c != '\n' {
				s.reset() // Lone CR: the terminal would overwrite this line.
			}
		}
		switch c {
		case '\n':
			s.end()
		case '\r':
			s.cr = true
		default:
			if len(s.buf) < s.maxBytes {
				s.buf = append(s.buf, c)
			} else {
				s.cut = true
				s.dropped++
			}
		}
	}
	return len(p), nil
}

// flush emits a partial line, for example when output pauses or at Close.
func (s *lineSplitter) flush() {
	s.cr = false
	if len(s.buf) > 0 || s.cut {
		s.end()
	}
}

func (s *lineSplitter) end() {
	text := strings.ToValidUTF8(string(s.buf), "�")
	if s.cut {
		text += " … [line truncated: " + strconv.FormatInt(s.dropped, 10) + " more bytes not stored]"
	}
	s.reset()
	s.emit(text)
}

func (s *lineSplitter) reset() {
	s.buf = s.buf[:0]
	s.cut = false
	s.dropped = 0
}
