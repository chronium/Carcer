package guest

import (
	"encoding/binary"
	"fmt"
	"io"
)

const (
	ProtocolVersion = uint16(1)
	MaxPayloadSize  = 16 * 1024 * 1024
	frameHeaderSize = 16
)

var frameMagic = [4]byte{'C', 'X', 'O', 'S'}

// Frame is one version 1 message on the guest serial protocol.
type Frame struct {
	MessageType uint16
	RequestID   uint32
	Payload     []byte
}

// FramingError reports malformed or incomplete frame data.
type FramingError struct {
	Reason string
}

func (e *FramingError) Error() string { return e.Reason }

// EncodeFrame returns the exact version 1 wire representation of frame.
func EncodeFrame(frame Frame) ([]byte, error) {
	if len(frame.Payload) > MaxPayloadSize {
		return nil, &FramingError{Reason: "payload exceeds the 16 MiB version 1 limit"}
	}

	encoded := make([]byte, frameHeaderSize+len(frame.Payload))
	copy(encoded[:4], frameMagic[:])
	binary.LittleEndian.PutUint16(encoded[4:6], ProtocolVersion)
	binary.LittleEndian.PutUint16(encoded[6:8], frame.MessageType)
	binary.LittleEndian.PutUint32(encoded[8:12], frame.RequestID)
	binary.LittleEndian.PutUint32(encoded[12:16], uint32(len(frame.Payload)))
	copy(encoded[frameHeaderSize:], frame.Payload)
	return encoded, nil
}

// ReadFrame reads and validates one frame. Transport deadlines and cancellation
// are owned by the caller because io.Reader has no cancellation contract.
func ReadFrame(reader io.Reader) (Frame, error) {
	header := make([]byte, frameHeaderSize)
	if _, err := io.ReadFull(reader, header); err != nil {
		return Frame{}, &FramingError{Reason: fmt.Sprintf("connection closed while reading frame header: %v", err)}
	}
	messageType, requestID, payloadLength, err := decodeHeader(header)
	if err != nil {
		return Frame{}, err
	}

	payload := make([]byte, payloadLength)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return Frame{}, &FramingError{Reason: fmt.Sprintf("connection closed while reading frame payload: %v", err)}
	}
	return Frame{MessageType: messageType, RequestID: requestID, Payload: payload}, nil
}

// ExtractFrame removes one complete frame from the front of an incremental
// buffer. It returns complete=false when the bytes seen so far are a valid
// frame prefix.
func ExtractFrame(buffer *[]byte) (frame Frame, complete bool, err error) {
	data := *buffer
	prefixLength := min(len(data), len(frameMagic))
	for index := range prefixLength {
		if data[index] != frameMagic[index] {
			return Frame{}, false, &FramingError{Reason: fmt.Sprintf("invalid frame magic %q", data[:prefixLength])}
		}
	}
	if len(data) < frameHeaderSize {
		return Frame{}, false, nil
	}

	messageType, requestID, payloadLength, err := decodeHeader(data[:frameHeaderSize])
	if err != nil {
		return Frame{}, false, err
	}
	frameLength := frameHeaderSize + int(payloadLength)
	if len(data) < frameLength {
		return Frame{}, false, nil
	}
	payload := append([]byte(nil), data[frameHeaderSize:frameLength]...)
	*buffer = data[frameLength:]
	return Frame{MessageType: messageType, RequestID: requestID, Payload: payload}, true, nil
}

func decodeHeader(header []byte) (uint16, uint32, uint32, error) {
	if string(header[:4]) != string(frameMagic[:]) {
		return 0, 0, 0, &FramingError{Reason: fmt.Sprintf("invalid frame magic %q", header[:4])}
	}
	version := binary.LittleEndian.Uint16(header[4:6])
	if version != ProtocolVersion {
		return 0, 0, 0, &FramingError{Reason: fmt.Sprintf("unsupported protocol version %d", version)}
	}
	payloadLength := binary.LittleEndian.Uint32(header[12:16])
	if payloadLength > MaxPayloadSize {
		return 0, 0, 0, &FramingError{Reason: fmt.Sprintf("payload length %d exceeds the 16 MiB version 1 limit", payloadLength)}
	}
	return binary.LittleEndian.Uint16(header[6:8]), binary.LittleEndian.Uint32(header[8:12]), payloadLength, nil
}
