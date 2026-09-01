package guest

import "strings"

const maxStartupDiagnosticBytes = 4 * 1024

func EscapeDiagnosticBytes(data []byte) string {
	length := min(len(data), maxStartupDiagnosticBytes)
	var output strings.Builder
	output.Grow(length)
	const hexadecimal = "0123456789abcdef"
	for _, value := range data[:length] {
		switch {
		case value == '\n':
			output.WriteString(`\n`)
		case value == '\r':
			output.WriteString(`\r`)
		case value == '\t':
			output.WriteString(`\t`)
		case value >= 0x20 && value <= 0x7e:
			output.WriteByte(value)
		default:
			output.WriteString(`\x`)
			output.WriteByte(hexadecimal[value>>4])
			output.WriteByte(hexadecimal[value&0x0f])
		}
	}
	if len(data) > maxStartupDiagnosticBytes {
		output.WriteString("...[truncated]")
	}
	return output.String()
}
