package codexapp

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestMessageCodecUsesCompactUnescapedUTF8JSONL(t *testing.T) {
	encoded, err := EncodeMessage(map[string]any{
		"id": json.Number("17"), "method": "工具<&>", "params": map[string]any{"text": "λ\n次"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "{\"id\":17,\"method\":\"工具<&>\",\"params\":{\"text\":\"λ\\n次\"}}\n"
	if string(encoded) != want {
		t.Fatalf("EncodeMessage = %q, want %q", encoded, want)
	}
	decoded, err := DecodeMessage(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if decoded["id"] != json.Number("17") || decoded["method"] != "工具<&>" {
		t.Fatalf("DecodeMessage = %#v", decoded)
	}
}

func TestDecodeMessageRejectsMalformedNonobjectTrailingAndOversizedInput(t *testing.T) {
	for _, encoded := range [][]byte{
		[]byte("{"),
		[]byte("[]\n"),
		[]byte("{} {}\n"),
		{0xff, '\n'},
		bytes.Repeat([]byte{'x'}, maxAppServerMessageSize+1),
	} {
		if _, err := DecodeMessage(encoded); err == nil {
			t.Fatalf("DecodeMessage accepted %q", encoded[:min(len(encoded), 32)])
		}
	}
}

func TestShortJSONIsCompactAndCharacterBounded(t *testing.T) {
	short, err := ShortJSON(map[string]any{"text": "λ<&>"})
	if err != nil {
		t.Fatal(err)
	}
	if short != `{"text":"λ<&>"}` {
		t.Fatalf("ShortJSON = %q", short)
	}
	long, err := ShortJSON(strings.Repeat("次", MaxErrorOutput+10))
	if err != nil {
		t.Fatal(err)
	}
	if count := len([]rune(long)); count != MaxErrorOutput {
		t.Fatalf("ShortJSON rune count = %d", count)
	}
	if !strings.HasPrefix(long, `"次次`) {
		t.Fatalf("ShortJSON prefix = %q", long[:min(len(long), 16)])
	}
}

func FuzzDecodeMessage(f *testing.F) {
	for _, seed := range [][]byte{[]byte("{}\n"), []byte("[]"), []byte("{\"id\":1}\n"), {0xff, 0}} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, encoded []byte) {
		message, err := DecodeMessage(encoded)
		if err != nil {
			return
		}
		roundTrip, err := EncodeMessage(message)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeMessage(roundTrip); err != nil {
			t.Fatal(err)
		}
	})
}
