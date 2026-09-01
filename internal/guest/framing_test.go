package guest

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

func TestFrameExactWireRoundTripAndExtraction(t *testing.T) {
	payload := append([]byte{0, 1, 2, 255}, []byte("CodexOS\x00")...)
	frame := Frame{MessageType: 0xbeef, RequestID: 0xdeadbeef, Payload: payload}
	encoded, err := EncodeFrame(frame)
	if err != nil {
		t.Fatal(err)
	}

	expectedHeader := make([]byte, frameHeaderSize)
	copy(expectedHeader, "CXOS")
	binary.LittleEndian.PutUint16(expectedHeader[4:6], 1)
	binary.LittleEndian.PutUint16(expectedHeader[6:8], 0xbeef)
	binary.LittleEndian.PutUint32(expectedHeader[8:12], 0xdeadbeef)
	binary.LittleEndian.PutUint32(expectedHeader[12:16], uint32(len(payload)))
	if !bytes.Equal(encoded, append(expectedHeader, payload...)) {
		t.Fatalf("wire encoding differs: %x", encoded)
	}

	decoded, err := ReadFrame(bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	assertFrameEqual(t, decoded, frame)

	second := Frame{MessageType: 7, RequestID: 3, Payload: []byte("second")}
	secondEncoded, err := EncodeFrame(second)
	if err != nil {
		t.Fatal(err)
	}
	buffer := append(append([]byte(nil), encoded...), secondEncoded...)
	extracted, complete, err := ExtractFrame(&buffer)
	if err != nil || !complete {
		t.Fatalf("ExtractFrame() = (_, %v, %v)", complete, err)
	}
	assertFrameEqual(t, extracted, frame)
	extracted, complete, err = ExtractFrame(&buffer)
	if err != nil || !complete {
		t.Fatalf("second ExtractFrame() = (_, %v, %v)", complete, err)
	}
	assertFrameEqual(t, extracted, second)
	if len(buffer) != 0 {
		t.Fatalf("buffer retains %d bytes", len(buffer))
	}
}

func TestExtractFrameAcceptsEveryValidPrefix(t *testing.T) {
	encoded, err := EncodeFrame(Frame{MessageType: 8, RequestID: 43, Payload: []byte("fragmented")})
	if err != nil {
		t.Fatal(err)
	}
	for length := range len(encoded) {
		buffer := append([]byte(nil), encoded[:length]...)
		_, complete, err := ExtractFrame(&buffer)
		if err != nil || complete {
			t.Fatalf("prefix %d: complete=%v err=%v", length, complete, err)
		}
	}
}

func TestReadFrameHandlesByteAtATimeReader(t *testing.T) {
	frame := Frame{MessageType: 9, RequestID: 17, Payload: []byte("fragmented payload")}
	encoded, err := EncodeFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := ReadFrame(&singleByteReader{reader: bytes.NewReader(encoded)})
	if err != nil {
		t.Fatal(err)
	}
	assertFrameEqual(t, decoded, frame)
}

func TestFrameAcceptsMaximumPayload(t *testing.T) {
	payload := make([]byte, MaxPayloadSize)
	encoded, err := EncodeFrame(Frame{Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) != frameHeaderSize+MaxPayloadSize {
		t.Fatalf("encoded length = %d", len(encoded))
	}
}

func TestFramingRejectsMalformedAndIncompleteData(t *testing.T) {
	tests := []struct {
		name string
		data []byte
		want string
	}{
		{name: "magic", data: header("NOPE", 1, 0), want: "magic"},
		{name: "version", data: header("CXOS", 2, 0), want: "version"},
		{name: "large", data: header("CXOS", 1, MaxPayloadSize+1), want: "exceeds"},
		{name: "payload closure", data: append(header("CXOS", 1, 4), 'a'), want: "payload"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ReadFrame(bytes.NewReader(test.data))
			var framingErr *FramingError
			if !errors.As(err, &framingErr) || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ReadFrame() error = %v, want FramingError containing %q", err, test.want)
			}
		})
	}

	buffer := []byte("N")
	if _, _, err := ExtractFrame(&buffer); err == nil || !strings.Contains(err.Error(), "magic") {
		t.Fatalf("ExtractFrame() error = %v", err)
	}

	tooLarge := make([]byte, MaxPayloadSize+1)
	if _, err := EncodeFrame(Frame{Payload: tooLarge}); err == nil {
		t.Fatal("EncodeFrame accepted an oversized payload")
	}
}

func FuzzExtractFrame(f *testing.F) {
	for _, seed := range [][]byte{nil, []byte("C"), []byte("CXOS"), header("CXOS", 1, 0), []byte("bad")} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		buffer := append([]byte(nil), input...)
		frame, complete, err := ExtractFrame(&buffer)
		if err != nil || !complete {
			return
		}
		encoded, err := EncodeFrame(frame)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.HasPrefix(input, encoded) {
			t.Fatal("successful extraction did not consume the encoded frame prefix")
		}
	})
}

func header(magic string, version uint16, payloadLength int) []byte {
	header := make([]byte, frameHeaderSize)
	copy(header[:4], magic)
	binary.LittleEndian.PutUint16(header[4:6], version)
	binary.LittleEndian.PutUint32(header[12:16], uint32(payloadLength))
	return header
}

func assertFrameEqual(t *testing.T, got, want Frame) {
	t.Helper()
	if got.MessageType != want.MessageType || got.RequestID != want.RequestID || !bytes.Equal(got.Payload, want.Payload) {
		t.Fatalf("frame = %#v, want %#v", got, want)
	}
}

type singleByteReader struct {
	reader *bytes.Reader
}

func (r *singleByteReader) Read(buffer []byte) (int, error) {
	if len(buffer) > 1 {
		buffer = buffer[:1]
	}
	return r.reader.Read(buffer)
}
