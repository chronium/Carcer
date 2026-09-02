package operator

import (
	"bytes"
	"os"
	"testing"
)

func TestSupportsTUIRequiresBothTTYStreamsAndUsableTerm(t *testing.T) {
	terminal, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("pseudo-terminal unavailable: %v", err)
	}
	defer terminal.Close()
	if !SupportsTUI(terminal, terminal, "xterm-256color") {
		t.Fatal("a pseudo-terminal pair with a usable TERM was rejected")
	}

	pipeReader, pipeWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pipeReader.Close()
	defer pipeWriter.Close()
	if SupportsTUI(pipeReader, terminal, "xterm-256color") {
		t.Fatal("non-terminal stdin was accepted")
	}
	if SupportsTUI(terminal, pipeWriter, "xterm-256color") {
		t.Fatal("non-terminal stdout was accepted")
	}
	if SupportsTUI(terminal, terminal, "") || SupportsTUI(terminal, terminal, "dumb") {
		t.Fatal("an unsupported TERM was accepted")
	}
	if SupportsTUI(bytes.NewReader(nil), &bytes.Buffer{}, "xterm-256color") {
		t.Fatal("streams without file descriptors were accepted")
	}
}
