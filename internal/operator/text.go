package operator

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// EscapeTerminalText preserves printable Unicode while making terminal control
// characters inert. Newlines may be retained only where the caller owns the
// surrounding layout.
func EscapeTerminalText(value string, preserveNewlines bool) string {
	var escaped strings.Builder
	for len(value) > 0 {
		r, size := utf8.DecodeRuneInString(value)
		if r == utf8.RuneError && size == 1 {
			fmt.Fprintf(&escaped, `\x%02x`, value[0])
			value = value[1:]
			continue
		}
		value = value[size:]
		switch r {
		case '\n':
			if preserveNewlines {
				escaped.WriteByte('\n')
			} else {
				escaped.WriteString(`\n`)
			}
		case '\r':
			escaped.WriteString(`\r`)
		case '\t':
			escaped.WriteString(`\t`)
		default:
			if r <= 0x1f || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
				fmt.Fprintf(&escaped, `\x%02x`, r)
			} else {
				escaped.WriteRune(r)
			}
		}
	}
	return escaped.String()
}

// ExitInterviewQuestion recognizes the plain-console "ask TEXT" form and
// preserves the question after trimming only the separator whitespace. It
// reports false for every other command or an empty question.
func ExitInterviewQuestion(commandLine string) (string, bool) {
	commandLine = strings.TrimRight(commandLine, "\r\n")
	words := strings.Fields(commandLine)
	if len(words) == 0 || words[0] != "ask" {
		return "", false
	}
	stripped := strings.TrimLeftFunc(commandLine, unicode.IsSpace)
	if len(stripped) == 3 {
		return "", false
	}
	separator, _ := utf8.DecodeRuneInString(stripped[3:])
	if !unicode.IsSpace(separator) {
		return "", false
	}
	question := strings.TrimLeftFunc(stripped[3:], unicode.IsSpace)
	if question == "" {
		return "", false
	}
	return question, true
}
