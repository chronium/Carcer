package guest

import (
	"encoding/binary"
	"fmt"
	"unicode/utf8"
)

const (
	ListToolsRequest    = uint16(0x0001)
	ListToolsResponse   = uint16(0x8001)
	InvokeToolRequest   = uint16(0x0002)
	InvokeToolResponse  = uint16(0x8002)
	HostServiceRequest  = uint16(0x0003)
	HostServiceResponse = uint16(0x8003)

	maxProtocolNames = 255
	maxArguments     = 64
	maxTools         = 256
)

type HostRequest struct {
	RequestID   uint32
	ServiceName string
	Arguments   [][]byte
}

type HostServiceProtocolError struct {
	Reason string
}

func (e *HostServiceProtocolError) Error() string { return e.Reason }

func DecodeHostServiceRequest(frame Frame) (HostRequest, error) {
	if frame.MessageType != HostServiceRequest {
		return HostRequest{}, &HostServiceProtocolError{Reason: fmt.Sprintf("expected HOST_SERVICE_REQUEST, got message type 0x%04x", frame.MessageType)}
	}
	if frame.RequestID == 0 {
		return HostRequest{}, &HostServiceProtocolError{Reason: "host-service request ID must be non-zero"}
	}
	if len(frame.Payload) > MaxPayloadSize {
		return HostRequest{}, &HostServiceProtocolError{Reason: "host-service request payload exceeds 16 MiB"}
	}

	decoder := protocolDecoder{data: frame.Payload}
	nameLength, err := decoder.uint16("service name length")
	if err != nil {
		return HostRequest{}, err
	}
	if nameLength == 0 {
		return HostRequest{}, &HostServiceProtocolError{Reason: "service name must not be empty"}
	}
	if nameLength > maxProtocolNames {
		return HostRequest{}, &HostServiceProtocolError{Reason: "service name exceeds 255 encoded bytes"}
	}
	encodedName, err := decoder.bytes(int(nameLength), "service name")
	if err != nil {
		return HostRequest{}, err
	}
	if !utf8.Valid(encodedName) {
		return HostRequest{}, &HostServiceProtocolError{Reason: "service name is not valid UTF-8"}
	}
	argumentCount, err := decoder.uint16("argument count")
	if err != nil {
		return HostRequest{}, err
	}
	if argumentCount > maxArguments {
		return HostRequest{}, &HostServiceProtocolError{Reason: "argument count exceeds 64"}
	}

	arguments := make([][]byte, 0, argumentCount)
	for range int(argumentCount) {
		argumentLength, err := decoder.uint32("argument length")
		if err != nil {
			return HostRequest{}, err
		}
		argument, err := decoder.bytes64(uint64(argumentLength), "argument")
		if err != nil {
			return HostRequest{}, err
		}
		// Preserve the distinction between an absent argument slice and an
		// explicitly present empty argument. Provenance uses that distinction
		// when recording the source snapshot supplied to the build service.
		copied := make([]byte, len(argument))
		copy(copied, argument)
		arguments = append(arguments, copied)
	}
	if !decoder.done() {
		return HostRequest{}, &HostServiceProtocolError{Reason: "unexpected trailing data in host-service request"}
	}
	return HostRequest{RequestID: frame.RequestID, ServiceName: string(encodedName), Arguments: arguments}, nil
}

func CreateHostServiceResponse(requestID, status uint32, output []byte) (Frame, error) {
	if requestID == 0 {
		return Frame{}, fmt.Errorf("host-service response request ID must be a non-zero uint32")
	}
	if len(output) > MaxPayloadSize-4 {
		return Frame{}, fmt.Errorf("host-service response payload exceeds 16 MiB")
	}
	payload := make([]byte, 4+len(output))
	binary.LittleEndian.PutUint32(payload[:4], status)
	copy(payload[4:], output)
	return Frame{MessageType: HostServiceResponse, RequestID: requestID, Payload: payload}, nil
}

type ToolProtocolError struct {
	Reason string
}

func (e *ToolProtocolError) Error() string { return e.Reason }

type ToolResult struct {
	Status uint32
	Output []byte
}

func ParseToolList(payload []byte) ([]string, error) {
	if len(payload) < 2 {
		return nil, &ToolProtocolError{Reason: "list response is missing the tool count"}
	}
	toolCount := int(binary.LittleEndian.Uint16(payload[:2]))
	if toolCount > maxTools {
		return nil, &ToolProtocolError{Reason: fmt.Sprintf("list response contains %d tools; maximum is 256", toolCount)}
	}
	offset := 2
	tools := make([]string, 0, toolCount)
	for range toolCount {
		if len(payload)-offset < 2 {
			return nil, &ToolProtocolError{Reason: "list response has a truncated name length"}
		}
		nameLength := int(binary.LittleEndian.Uint16(payload[offset : offset+2]))
		offset += 2
		if nameLength == 0 {
			return nil, &ToolProtocolError{Reason: "list response contains an empty tool name"}
		}
		if nameLength > maxProtocolNames {
			return nil, &ToolProtocolError{Reason: "list response tool name exceeds 255 bytes"}
		}
		if nameLength > len(payload)-offset {
			return nil, &ToolProtocolError{Reason: "list response has a truncated tool name"}
		}
		name := payload[offset : offset+nameLength]
		offset += nameLength
		if !utf8.Valid(name) {
			return nil, &ToolProtocolError{Reason: "list response contains invalid UTF-8"}
		}
		tools = append(tools, string(name))
	}
	if offset != len(payload) {
		return nil, &ToolProtocolError{Reason: "list response contains unexpected trailing data"}
	}
	return tools, nil
}

func EncodeInvokeRequest(name string, arguments [][]byte) ([]byte, error) {
	if !utf8.ValidString(name) {
		return nil, fmt.Errorf("tool name is not valid UTF-8")
	}
	if name == "" {
		return nil, fmt.Errorf("tool name must not be empty")
	}
	if len(name) > maxProtocolNames {
		return nil, fmt.Errorf("tool name exceeds 255 UTF-8 bytes")
	}
	if len(arguments) > maxArguments {
		return nil, fmt.Errorf("an invocation may contain at most 64 arguments")
	}

	size := 2 + len(name) + 2
	for _, argument := range arguments {
		if len(argument) > MaxPayloadSize-4-size {
			return nil, fmt.Errorf("invocation payload exceeds the 16 MiB frame limit")
		}
		size += 4 + len(argument)
	}
	payload := make([]byte, size)
	binary.LittleEndian.PutUint16(payload[:2], uint16(len(name)))
	copy(payload[2:], name)
	offset := 2 + len(name)
	binary.LittleEndian.PutUint16(payload[offset:offset+2], uint16(len(arguments)))
	offset += 2
	for _, argument := range arguments {
		binary.LittleEndian.PutUint32(payload[offset:offset+4], uint32(len(argument)))
		offset += 4
		copy(payload[offset:], argument)
		offset += len(argument)
	}
	return payload, nil
}

func DecodeToolResult(payload []byte) (ToolResult, error) {
	if len(payload) < 4 {
		return ToolResult{}, &ToolProtocolError{Reason: "invoke response is missing its status"}
	}
	return ToolResult{Status: binary.LittleEndian.Uint32(payload[:4]), Output: append([]byte(nil), payload[4:]...)}, nil
}

type protocolDecoder struct {
	data   []byte
	offset int
}

func (d *protocolDecoder) uint16(description string) (uint16, error) {
	value, err := d.bytes(2, description)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(value), nil
}

func (d *protocolDecoder) uint32(description string) (uint32, error) {
	value, err := d.bytes(4, description)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(value), nil
}

func (d *protocolDecoder) bytes(length int, description string) ([]byte, error) {
	return d.bytes64(uint64(length), description)
}

func (d *protocolDecoder) bytes64(length uint64, description string) ([]byte, error) {
	if length > uint64(len(d.data)-d.offset) {
		return nil, &HostServiceProtocolError{Reason: "truncated " + description}
	}
	end := d.offset + int(length)
	value := d.data[d.offset:end]
	d.offset = end
	return value, nil
}

func (d *protocolDecoder) done() bool { return d.offset == len(d.data) }
