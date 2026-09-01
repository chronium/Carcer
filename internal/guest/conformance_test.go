package guest

import (
	"bytes"
	"encoding/base64"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPythonWireConformance(t *testing.T) {
	files := []SnapshotFile{
		{Path: "seed/kernel.c", Content: []byte{'s', 'r', 'c', 0, 255}},
		{Path: "seed/empty", Content: nil},
	}
	snapshot, err := EncodeSourceSnapshot(files)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := EncodeFrame(Frame{MessageType: 0xbeef, RequestID: 0xdeadbeef, Payload: []byte{0, 255, 'x'}})
	if err != nil {
		t.Fatal(err)
	}
	hostResponse, err := CreateHostServiceResponse(37, 0xa0b0c0d0, []byte{0, 255, 'x'})
	if err != nil {
		t.Fatal(err)
	}
	hostResponseWire, err := EncodeFrame(hostResponse)
	if err != nil {
		t.Fatal(err)
	}
	invocation, err := EncodeInvokeRequest("编译", [][]byte{nil, {0, 255, 'x'}})
	if err != nil {
		t.Fatal(err)
	}

	const script = `
import base64, importlib, pathlib, struct, sys, types
root = pathlib.Path(sys.argv[1])
package = types.ModuleType("harness")
package.__path__ = [str(root / "harness")]
sys.modules["harness"] = package
framing = importlib.import_module("harness.framing")
snapshot = importlib.import_module("harness.source_snapshot")
host = importlib.import_module("harness.host_service_protocol")
tool = importlib.import_module("harness.tool_protocol")
files = (
    snapshot.SnapshotFile("seed/kernel.c", b"src\x00\xff"),
    snapshot.SnapshotFile("seed/empty", b""),
)
wire = framing.encode_frame(framing.Frame(0xbeef, 0xdeadbeef, b"\x00\xffx"))
print(base64.b64encode(wire).decode("ascii"))
print(base64.b64encode(snapshot.encode_source_snapshot(files)).decode("ascii"))
response = host.create_host_service_response(37, 0xa0b0c0d0, b"\x00\xffx")
print(base64.b64encode(framing.encode_frame(response)).decode("ascii"))
print(base64.b64encode(tool._encode_invoke_request("编译", (b"", b"\x00\xffx"))).decode("ascii"))
name = "服务".encode("utf-8")
host_payload = struct.pack("<H", len(name)) + name + struct.pack("<H", 2)
host_payload += struct.pack("<I", 0) + struct.pack("<I", 3) + b"\x00\xffx"
host_request = framing.Frame(0x0003, 41, host_payload)
print(base64.b64encode(framing.encode_frame(host_request)).decode("ascii"))
tool_list = struct.pack("<H", 2) + struct.pack("<H", 4) + b"read"
tool_list += struct.pack("<H", 6) + "编译".encode("utf-8")
print(base64.b64encode(tool_list).decode("ascii"))
print(base64.b64encode(struct.pack("<I", 23) + b"result\x00\xff").decode("ascii"))
`
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate conformance test")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "../.."))
	command := exec.Command("python3", "-c", script, root)
	output, err := command.Output()
	if err != nil {
		t.Fatalf("Python reference failed: %v", err)
	}
	lines := strings.Fields(string(output))
	if len(lines) != 7 {
		t.Fatalf("Python reference returned %q", output)
	}
	pythonFrame, err := base64.StdEncoding.DecodeString(lines[0])
	if err != nil {
		t.Fatal(err)
	}
	pythonSnapshot, err := base64.StdEncoding.DecodeString(lines[1])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(frame, pythonFrame) {
		t.Fatalf("frame differs from Python: Go %x Python %x", frame, pythonFrame)
	}
	decodedFrame, err := ReadFrame(bytes.NewReader(pythonFrame))
	if err != nil {
		t.Fatal(err)
	}
	assertFrameEqual(t, decodedFrame, Frame{MessageType: 0xbeef, RequestID: 0xdeadbeef, Payload: []byte{0, 255, 'x'}})
	if !bytes.Equal(snapshot, pythonSnapshot) {
		t.Fatalf("snapshot differs from Python: Go %x Python %x", snapshot, pythonSnapshot)
	}
	decodedSnapshot, err := DecodeSourceSnapshot(pythonSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	assertSnapshotEqual(t, decodedSnapshot, files)
	pythonHostResponse, err := base64.StdEncoding.DecodeString(lines[2])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(hostResponseWire, pythonHostResponse) {
		t.Fatalf("host response differs from Python: Go %x Python %x", hostResponseWire, pythonHostResponse)
	}
	pythonInvocation, err := base64.StdEncoding.DecodeString(lines[3])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(invocation, pythonInvocation) {
		t.Fatalf("tool invocation differs from Python: Go %x Python %x", invocation, pythonInvocation)
	}
	pythonHostRequest, err := base64.StdEncoding.DecodeString(lines[4])
	if err != nil {
		t.Fatal(err)
	}
	hostFrame, err := ReadFrame(bytes.NewReader(pythonHostRequest))
	if err != nil {
		t.Fatal(err)
	}
	hostRequest, err := DecodeHostServiceRequest(hostFrame)
	if err != nil {
		t.Fatal(err)
	}
	if hostRequest.RequestID != 41 || hostRequest.ServiceName != "服务" || len(hostRequest.Arguments) != 2 || len(hostRequest.Arguments[0]) != 0 || !bytes.Equal(hostRequest.Arguments[1], []byte{0, 255, 'x'}) {
		t.Fatalf("Python host request decoded as %#v", hostRequest)
	}
	pythonToolList, err := base64.StdEncoding.DecodeString(lines[5])
	if err != nil {
		t.Fatal(err)
	}
	tools, err := ParseToolList(pythonToolList)
	if err != nil || len(tools) != 2 || tools[0] != "read" || tools[1] != "编译" {
		t.Fatalf("Python tool list decoded as %#v, %v", tools, err)
	}
	pythonToolResult, err := base64.StdEncoding.DecodeString(lines[6])
	if err != nil {
		t.Fatal(err)
	}
	result, err := DecodeToolResult(pythonToolResult)
	if err != nil || result.Status != 23 || !bytes.Equal(result.Output, []byte{'r', 'e', 's', 'u', 'l', 't', 0, 255}) {
		t.Fatalf("Python tool result decoded as %#v, %v", result, err)
	}
}
