package operator

import (
	"io"

	"github.com/charmbracelet/x/term"
)

// SupportsTUI matches the Python entry point's automatic display decision:
// both streams must be terminals and TERM must name a usable terminal.
func SupportsTUI(input io.Reader, output io.Writer, terminalName string) bool {
	if terminalName == "" || terminalName == "dumb" {
		return false
	}
	inputFile, inputOK := input.(interface{ Fd() uintptr })
	outputFile, outputOK := output.(interface{ Fd() uintptr })
	return inputOK && outputOK &&
		term.IsTerminal(inputFile.Fd()) && term.IsTerminal(outputFile.Fd())
}
