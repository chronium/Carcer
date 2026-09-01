package guest

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func TestHostServiceRequestAndResponseCodecs(t *testing.T) {
	name := "编译"
	arguments := [][]byte{nil, {0, 255, 'x'}, bytes.Repeat([]byte{7}, 256)}
	payload := make([]byte, 2+len(name)+2)
	binary.LittleEndian.PutUint16(payload[:2], uint16(len(name)))
	copy(payload[2:], name)
	offset := 2 + len(name)
	binary.LittleEndian.PutUint16(payload[offset:offset+2], uint16(len(arguments)))
	for _, argument := range arguments {
		encoded := make([]byte, 4+len(argument))
		binary.LittleEndian.PutUint32(encoded[:4], uint32(len(argument)))
		copy(encoded[4:], argument)
		payload = append(payload, encoded...)
	}

	request, err := DecodeHostServiceRequest(Frame{MessageType: HostServiceRequest, RequestID: 0x12345678, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if request.RequestID != 0x12345678 || request.ServiceName != name || len(request.Arguments) != len(arguments) {
		t.Fatalf("request = %#v", request)
	}
	for index := range arguments {
		if !bytes.Equal(request.Arguments[index], arguments[index]) {
			t.Fatalf("argument %d differs", index)
		}
	}

	response, err := CreateHostServiceResponse(37, 0xa0b0c0d0, []byte{0, 255})
	if err != nil {
		t.Fatal(err)
	}
	if response.MessageType != HostServiceResponse || response.RequestID != 37 || binary.LittleEndian.Uint32(response.Payload[:4]) != 0xa0b0c0d0 || !bytes.Equal(response.Payload[4:], []byte{0, 255}) {
		t.Fatalf("response = %#v", response)
	}
}

func TestHostServiceRejectsMalformedRequests(t *testing.T) {
	validEmpty := []byte{1, 0, 'x', 0, 0}
	longName := append([]byte{0, 1}, bytes.Repeat([]byte{'x'}, 256)...)
	longName = append(longName, 0, 0)
	tooManyArguments := []byte{1, 0, 'x', 65, 0}
	tests := []struct {
		name  string
		frame Frame
		want  string
	}{
		{name: "type", frame: Frame{MessageType: InvokeToolRequest, RequestID: 1}, want: "message type"},
		{name: "id", frame: Frame{MessageType: HostServiceRequest}, want: "non-zero"},
		{name: "empty", frame: Frame{MessageType: HostServiceRequest, RequestID: 1, Payload: []byte{0, 0}}, want: "empty"},
		{name: "truncated name", frame: Frame{MessageType: HostServiceRequest, RequestID: 1, Payload: []byte{4, 0, 'a'}}, want: "truncated service name"},
		{name: "UTF-8", frame: Frame{MessageType: HostServiceRequest, RequestID: 1, Payload: []byte{1, 0, 255, 0, 0}}, want: "UTF-8"},
		{name: "name limit", frame: Frame{MessageType: HostServiceRequest, RequestID: 1, Payload: longName}, want: "255"},
		{name: "argument limit", frame: Frame{MessageType: HostServiceRequest, RequestID: 1, Payload: tooManyArguments}, want: "64"},
		{name: "trailing", frame: Frame{MessageType: HostServiceRequest, RequestID: 1, Payload: append(validEmpty, 'x')}, want: "trailing"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := DecodeHostServiceRequest(test.frame); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("DecodeHostServiceRequest() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestToolProtocolCodecs(t *testing.T) {
	payload := []byte{2, 0, 4, 0, 'r', 'e', 'a', 'd', 6, 0}
	payload = append(payload, []byte("编译")...)
	tools, err := ParseToolList(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 || tools[0] != "read" || tools[1] != "编译" {
		t.Fatalf("tools = %#v", tools)
	}

	arguments := [][]byte{nil, {0, 255}}
	invocation, err := EncodeInvokeRequest("编译", arguments)
	if err != nil {
		t.Fatal(err)
	}
	if got := binary.LittleEndian.Uint16(invocation[:2]); got != 6 {
		t.Fatalf("encoded name length = %d", got)
	}
	resultPayload := append([]byte{52, 18, 0, 0}, []byte("output")...)
	result, err := DecodeToolResult(resultPayload)
	if err != nil || result.Status != 0x1234 || string(result.Output) != "output" {
		t.Fatalf("DecodeToolResult() = %#v, %v", result, err)
	}
}

func TestToolProtocolRejectsMalformedAndOversizedValues(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload []byte
		want    string
	}{
		{name: "count", payload: nil, want: "tool count"},
		{name: "name length", payload: []byte{1, 0}, want: "name length"},
		{name: "empty", payload: []byte{1, 0, 0, 0}, want: "empty"},
		{name: "truncated", payload: []byte{1, 0, 2, 0, 'a'}, want: "truncated"},
		{name: "UTF-8", payload: []byte{1, 0, 1, 0, 255}, want: "UTF-8"},
		{name: "trailing", payload: []byte{0, 0, 1}, want: "trailing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ParseToolList(test.payload); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ParseToolList() error = %v, want %q", err, test.want)
			}
		})
	}
	if _, err := EncodeInvokeRequest("", nil); err == nil {
		t.Fatal("empty tool name accepted")
	}
	if _, err := EncodeInvokeRequest(string([]byte{255}), nil); err == nil {
		t.Fatal("invalid UTF-8 tool name accepted")
	}
	if _, err := EncodeInvokeRequest(strings.Repeat("x", 256), nil); err == nil {
		t.Fatal("oversized tool name accepted")
	}
	if _, err := EncodeInvokeRequest("x", make([][]byte, maxArguments+1)); err == nil {
		t.Fatal("too many arguments accepted")
	}
	if _, err := EncodeInvokeRequest("x", [][]byte{make([]byte, MaxPayloadSize)}); err == nil {
		t.Fatal("oversized invocation accepted")
	}
	if _, err := DecodeToolResult([]byte{0, 0, 0}); err == nil {
		t.Fatal("truncated tool result accepted")
	}
	if _, err := CreateHostServiceResponse(0, 0, nil); err == nil {
		t.Fatal("zero response request ID accepted")
	}
}
