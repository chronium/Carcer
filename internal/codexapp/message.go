package codexapp

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"
)

const (
	MaxErrorOutput          = 64 * 1024
	maxAppServerMessageSize = 16 * 1024 * 1024
)

func EncodeMessage(message map[string]any) ([]byte, error) {
	if message == nil {
		return nil, &Error{Reason: "Codex app-server message is not an object"}
	}
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(message); err != nil {
		return nil, &Error{Reason: "could not encode Codex app-server message", Err: err}
	}
	encoded := output.Bytes()
	if len(encoded) > maxAppServerMessageSize {
		return nil, &Error{Reason: "Codex app-server message exceeds size limit"}
	}
	return append([]byte(nil), encoded...), nil
}

func DecodeMessage(encoded []byte) (map[string]any, error) {
	if len(encoded) > maxAppServerMessageSize {
		return nil, &Error{Reason: "Codex app-server message exceeds size limit"}
	}
	if !utf8.Valid(encoded) {
		return nil, &Error{Reason: "failed to read Codex app-server output: invalid UTF-8"}
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, &Error{Reason: "Codex app-server emitted malformed JSON", Err: err}
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, &Error{Reason: "Codex app-server emitted malformed JSON"}
	}
	message, ok := value.(map[string]any)
	if !ok {
		return nil, &Error{Reason: "Codex app-server message is not an object"}
	}
	return message, nil
}

func ShortJSON(value any) (string, error) {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "", &Error{Reason: "could not encode Codex app-server error", Err: err}
	}
	encoded := bytes.TrimSuffix(output.Bytes(), []byte{'\n'})
	if utf8.RuneCount(encoded) <= MaxErrorOutput {
		return string(encoded), nil
	}
	end := 0
	for range MaxErrorOutput {
		_, width := utf8.DecodeRune(encoded[end:])
		end += width
	}
	return string(encoded[:end]), nil
}
