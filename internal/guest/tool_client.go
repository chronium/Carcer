package guest

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const (
	toolResponseTimeout = 5 * time.Second
	// This matches REQUEST_BUFFER_SIZE in the version 1 guest protocol loop.
	v1GuestInvocationPayloadCapacity = 16*1024 + 256 + 27
)

type ToolClient struct {
	dispatcher *SerialProtocolDispatcher
	mutex      sync.Mutex
	nextID     uint32
	timeout    time.Duration
}

func NewToolClient(dispatcher *SerialProtocolDispatcher) *ToolClient {
	return &ToolClient{dispatcher: dispatcher, nextID: 1, timeout: toolResponseTimeout}
}

func (c *ToolClient) ListTools(ctx context.Context) ([]string, error) {
	payload, err := c.exchange(ctx, ListToolsRequest, ListToolsResponse, nil)
	if err != nil {
		return nil, err
	}
	return ParseToolList(payload)
}

func (c *ToolClient) InvokeTool(ctx context.Context, name string, arguments [][]byte) (ToolResult, error) {
	encodedBytes, err := InvokeRequestPayloadSize(name, arguments)
	if err != nil {
		return ToolResult{}, err
	}
	if encodedBytes > v1GuestInvocationPayloadCapacity {
		output := fmt.Sprintf(
			"guest invocation rejected before serial dispatch: encoded payload is %d bytes; supported maximum is %d bytes; accepted_bytes:0",
			encodedBytes,
			v1GuestInvocationPayloadCapacity,
		)
		return ToolResult{Status: 1, Output: []byte(output)}, nil
	}
	payload, err := EncodeInvokeRequest(name, arguments)
	if err != nil {
		return ToolResult{}, err
	}
	response, err := c.exchange(ctx, InvokeToolRequest, InvokeToolResponse, payload)
	if err != nil {
		return ToolResult{}, err
	}
	return DecodeToolResult(response)
}

func (c *ToolClient) exchange(ctx context.Context, requestType, responseType uint16, payload []byte) ([]byte, error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	requestID := c.nextID
	if requestID == ^uint32(0) {
		c.nextID = 1
	} else {
		c.nextID++
	}
	response, err := c.dispatcher.Exchange(ctx, Frame{MessageType: requestType, RequestID: requestID, Payload: payload}, c.timeout)
	if err != nil {
		return nil, err
	}
	if response.RequestID != requestID {
		return nil, &ToolProtocolError{Reason: fmt.Sprintf("response request ID %d does not match %d", response.RequestID, requestID)}
	}
	if response.MessageType != responseType {
		return nil, &ToolProtocolError{Reason: fmt.Sprintf("response message type 0x%04x does not match 0x%04x", response.MessageType, responseType)}
	}
	return response.Payload, nil
}
