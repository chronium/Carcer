package guest

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func TestToolClientListInvokeAndRequestIDRollover(t *testing.T) {
	serial, peer, wait := connectedSerial(t)
	defer peer.Close()
	dispatcher := NewSerialProtocolDispatcher(serial, DispatcherOptions{})
	if err := dispatcher.StartReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dispatcher.Close() })
	client := NewToolClient(dispatcher)
	client.nextID = ^uint32(0)
	client.timeout = time.Second

	listed := make(chan struct {
		tools []string
		err   error
	}, 1)
	go func() {
		tools, err := client.ListTools(context.Background())
		listed <- struct {
			tools []string
			err   error
		}{tools, err}
	}()
	request := readPeerFrame(t, peer)
	if request.RequestID != ^uint32(0) || request.MessageType != ListToolsRequest {
		t.Fatalf("list request = %#v", request)
	}
	writePeerFrame(t, peer, Frame{MessageType: ListToolsResponse, RequestID: request.RequestID, Payload: []byte{1, 0, 4, 0, 'r', 'e', 'a', 'd'}})
	listResult := <-listed
	if listResult.err != nil || len(listResult.tools) != 1 || listResult.tools[0] != "read" {
		t.Fatalf("ListTools = %#v, %v", listResult.tools, listResult.err)
	}

	invoked := make(chan struct {
		result ToolResult
		err    error
	}, 1)
	go func() {
		result, err := client.InvokeTool(context.Background(), "read", [][]byte{[]byte("path")})
		invoked <- struct {
			result ToolResult
			err    error
		}{result, err}
	}()
	request = readPeerFrame(t, peer)
	if request.RequestID != 1 || request.MessageType != InvokeToolRequest {
		t.Fatalf("invoke request = %#v", request)
	}
	payload := make([]byte, 4)
	binary.LittleEndian.PutUint32(payload, 7)
	payload = append(payload, []byte("output")...)
	writePeerFrame(t, peer, Frame{MessageType: InvokeToolResponse, RequestID: 1, Payload: payload})
	invokeResult := <-invoked
	if invokeResult.err != nil || invokeResult.result.Status != 7 || string(invokeResult.result.Output) != "output" {
		t.Fatalf("InvokeTool = %#v, %v", invokeResult.result, invokeResult.err)
	}
	_ = dispatcher.Close()
	wait()
}

func TestToolClientEnforcesV1GuestInvocationCapacity(t *testing.T) {
	serial, peer, wait := connectedSerial(t)
	defer peer.Close()
	dispatcher := NewSerialProtocolDispatcher(serial, DispatcherOptions{})
	if err := dispatcher.StartReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dispatcher.Close() })
	client := NewToolClient(dispatcher)
	client.timeout = time.Second

	path := []byte("seed/target")
	offset := []byte("0")
	baseSize, err := InvokeRequestPayloadSize("write", [][]byte{path, offset, nil})
	if err != nil {
		t.Fatal(err)
	}
	validData := []byte(strings.Repeat("v", int(uint64(v1GuestInvocationPayloadCapacity)-baseSize)))
	validResult := make(chan struct {
		result ToolResult
		err    error
	}, 1)
	go func() {
		result, err := client.InvokeTool(context.Background(), "write", [][]byte{path, offset, validData})
		validResult <- struct {
			result ToolResult
			err    error
		}{result, err}
	}()
	request := readPeerFrame(t, peer)
	if request.MessageType != InvokeToolRequest || len(request.Payload) != v1GuestInvocationPayloadCapacity {
		t.Fatalf("largest valid request type/payload = 0x%x/%d, want 0x%x/%d", request.MessageType, len(request.Payload), InvokeToolRequest, v1GuestInvocationPayloadCapacity)
	}
	// The peer represents the guest boundary: only receipt of a request frame
	// can mutate its target.
	target := append([]byte(nil), validData...)
	writePeerFrame(t, peer, Frame{MessageType: InvokeToolResponse, RequestID: request.RequestID, Payload: make([]byte, 4)})
	valid := <-validResult
	if valid.err != nil || valid.result.Status != 0 {
		t.Fatalf("largest valid invocation = %#v, %v", valid.result, valid.err)
	}

	target = []byte("preserve this exact target")
	wantTarget := append([]byte(nil), target...)
	oversizedData := append(append([]byte(nil), validData...), 'x')
	result, err := client.InvokeTool(context.Background(), "write", [][]byte{path, offset, oversizedData})
	if err != nil {
		t.Fatalf("oversized invocation returned a transport error: %v", err)
	}
	if result.Status == 0 {
		t.Fatalf("oversized invocation reported tool success: %#v", result)
	}
	for _, want := range []string{
		"rejected before serial dispatch",
		fmt.Sprintf("encoded payload is %d bytes", v1GuestInvocationPayloadCapacity+1),
		fmt.Sprintf("supported maximum is %d bytes", v1GuestInvocationPayloadCapacity),
		"accepted_bytes:0",
	} {
		if !strings.Contains(string(result.Output), want) {
			t.Fatalf("oversized invocation result %q is missing %q", result.Output, want)
		}
	}
	if err := peer.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	var emitted [1]byte
	count, readErr := peer.Read(emitted[:])
	if count != 0 {
		target = append(target[:0], oversizedData...)
	}
	if !bytes.Equal(target, wantTarget) {
		t.Fatalf("oversized write changed target: got %q, want %q", target, wantTarget)
	}
	if timeout, ok := readErr.(net.Error); count != 0 || !ok || !timeout.Timeout() {
		t.Fatalf("oversized invocation emitted guest bytes: count=%d error=%v", count, readErr)
	}
	_ = dispatcher.Close()
	wait()
}

func TestToolClientRejectsMismatchedResponse(t *testing.T) {
	serial, peer, wait := connectedSerial(t)
	defer peer.Close()
	dispatcher := NewSerialProtocolDispatcher(serial, DispatcherOptions{})
	if err := dispatcher.StartReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dispatcher.Close() })
	client := NewToolClient(dispatcher)
	client.timeout = time.Second
	result := make(chan error, 1)
	go func() { _, err := client.ListTools(context.Background()); result <- err }()
	request := readPeerFrame(t, peer)
	writePeerFrame(t, peer, Frame{MessageType: InvokeToolResponse, RequestID: request.RequestID, Payload: make([]byte, 4)})
	err := <-result
	if err == nil || !strings.Contains(err.Error(), "message type") {
		t.Fatalf("ListTools error = %v", err)
	}
	_ = dispatcher.Close()
	wait()
}

func TestToolClientRejectsMismatchedRequestID(t *testing.T) {
	serial, peer, wait := connectedSerial(t)
	defer peer.Close()
	dispatcher := NewSerialProtocolDispatcher(serial, DispatcherOptions{})
	if err := dispatcher.StartReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dispatcher.Close() })
	client := NewToolClient(dispatcher)
	client.timeout = time.Second
	result := make(chan error, 1)
	go func() { _, err := client.ListTools(context.Background()); result <- err }()
	request := readPeerFrame(t, peer)
	writePeerFrame(t, peer, Frame{MessageType: ListToolsResponse, RequestID: request.RequestID + 1, Payload: []byte{0, 0}})
	err := <-result
	if err == nil || !strings.Contains(err.Error(), "request ID") {
		t.Fatalf("ListTools error = %v", err)
	}
	_ = dispatcher.Close()
	wait()
}
