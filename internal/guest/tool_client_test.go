package guest

import (
	"context"
	"encoding/binary"
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
