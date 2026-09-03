package guest

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
)

func TestSourceSnapshotRoundTripPreservesBinaryFiles(t *testing.T) {
	files := []SnapshotFile{
		{Path: "seed/kernel.c", Content: []byte{'s', 'o', 'u', 'r', 'c', 'e', 0, 255}},
		{Path: "seed/empty.bin", Content: nil},
	}
	encoded, err := EncodeSourceSnapshot(files)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeSourceSnapshot(encoded)
	if err != nil {
		t.Fatal(err)
	}
	assertSnapshotEqual(t, decoded, files)
}

func TestSourceSnapshotRejectsMalformedUnsafeAndBoundedInput(t *testing.T) {
	valid, err := EncodeSourceSnapshot([]SnapshotFile{{Path: "seed/a", Content: []byte("data")}})
	if err != nil {
		t.Fatal(err)
	}
	duplicateRecord := append([]byte{6, 0}, []byte("seed/a")...)
	duplicateRecord = append(duplicateRecord, 0, 0, 0, 0)
	duplicate := append([]byte{2, 0}, duplicateRecord...)
	duplicate = append(duplicate, duplicateRecord...)

	tests := []struct {
		name string
		data []byte
		want string
	}{
		{name: "truncated", data: []byte{1}, want: "truncated"},
		{name: "trailing", data: append(valid, 'x'), want: "trailing"},
		{name: "duplicate", data: duplicate, want: "duplicate"},
		{name: "invalid UTF-8", data: snapshotRecord([]byte{255}, nil), want: "UTF-8"},
		{name: "content bound", data: contentLengthRecord(maxSnapshotContent + 1), want: "64 KiB"},
	}
	for _, path := range []string{"seed/../outside", "/seed/kernel.c", "seed//kernel.c", "seed/bad\x00name", "seed/bad\nname"} {
		tests = append(tests, struct {
			name string
			data []byte
			want string
		}{name: path, data: snapshotRecord([]byte(path), nil), want: "unsafe"})
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := DecodeSourceSnapshot(test.data); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("DecodeSourceSnapshot() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestSourceSnapshotAcceptsExactFileAndContentBounds(t *testing.T) {
	files := make([]SnapshotFile, maxSnapshotFiles)
	for index := range files {
		files[index] = SnapshotFile{Path: fmt.Sprintf("seed/file-%03d", index)}
	}
	files[0].Content = make([]byte, maxSnapshotContent)
	encoded, err := EncodeSourceSnapshot(files)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeSourceSnapshot(encoded)
	if err != nil {
		t.Fatal(err)
	}
	assertSnapshotEqual(t, decoded, files)

	tooMany := append(append([]SnapshotFile(nil), files...), SnapshotFile{Path: "seed/overflow"})
	if _, err := EncodeSourceSnapshot(tooMany); err == nil {
		t.Fatal("snapshot with 129 files was accepted")
	}
	files[0].Content = make([]byte, maxSnapshotContent+1)
	if _, err := EncodeSourceSnapshot(files); err == nil {
		t.Fatal("snapshot exceeding 64 KiB was accepted")
	}
}

func FuzzDecodeSourceSnapshot(f *testing.F) {
	valid, err := EncodeSourceSnapshot([]SnapshotFile{{Path: "seed/kernel.c", Content: []byte("source")}})
	if err != nil {
		f.Fatal(err)
	}
	f.Add(valid)
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, input []byte) {
		files, err := DecodeSourceSnapshot(input)
		if err != nil {
			return
		}
		reencoded, err := EncodeSourceSnapshot(files)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(input, reencoded) {
			t.Fatal("accepted source snapshot does not have a canonical encoding")
		}
	})
}

func snapshotRecord(path, content []byte) []byte {
	encoded := make([]byte, 2+2+len(path)+4+len(content))
	binary.LittleEndian.PutUint16(encoded[:2], 1)
	binary.LittleEndian.PutUint16(encoded[2:4], uint16(len(path)))
	copy(encoded[4:], path)
	offset := 4 + len(path)
	binary.LittleEndian.PutUint32(encoded[offset:offset+4], uint32(len(content)))
	copy(encoded[offset+4:], content)
	return encoded
}

func contentLengthRecord(length int) []byte {
	encoded := snapshotRecord([]byte("seed/a"), nil)
	binary.LittleEndian.PutUint32(encoded[len(encoded)-4:], uint32(length))
	return encoded
}

func assertSnapshotEqual(t *testing.T, got, want []SnapshotFile) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d files, want %d", len(got), len(want))
	}
	for index := range got {
		if got[index].Path != want[index].Path || !bytes.Equal(got[index].Content, want[index].Content) {
			t.Fatalf("file %d = %#v, want %#v", index, got[index], want[index])
		}
	}
}
