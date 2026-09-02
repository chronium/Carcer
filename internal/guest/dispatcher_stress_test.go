package guest

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestDispatcherLargeHostResponseOneByteProgressAndFraming(t *testing.T) {
	transport := newDispatcherStressSerial(false)
	responseOutput := dispatcherStressPayload()
	hostRequestID := uint32(71)
	events := make(chan dispatcherStressEvent, 256)
	handlerCalled := make(chan HostRequest, 2)
	dispatcher := NewSerialProtocolDispatcher(transport, DispatcherOptions{
		ExchangeHostServices: func(_ context.Context, request HostRequest) (Frame, error) {
			handlerCalled <- request
			if request.ServiceName == "build" {
				return CreateHostServiceResponse(request.RequestID, 0, []byte("built"))
			}
			return CreateHostServiceResponse(request.RequestID, 0, responseOutput)
		},
		EventRecorder: dispatcherStressEventRecorder(events),
	})
	t.Cleanup(func() { _ = dispatcher.Close() })
	if err := dispatcher.StartReady(context.Background()); err != nil {
		t.Fatal(err)
	}

	largeToolPayload, err := EncodeInvokeRequest("read_provided_asset", nil)
	if err != nil {
		t.Fatal(err)
	}
	largeToolRequest := Frame{MessageType: InvokeToolRequest, RequestID: 59, Payload: largeToolPayload}
	largeToolRequestWire, err := EncodeFrame(largeToolRequest)
	if err != nil {
		t.Fatal(err)
	}
	largeExchangeResult := make(chan struct {
		frame Frame
		err   error
	}, 1)
	go func() {
		frame, exchangeErr := dispatcher.Exchange(context.Background(), largeToolRequest, 5*time.Second)
		largeExchangeResult <- struct {
			frame Frame
			err   error
		}{frame, exchangeErr}
	}()
	writeOffset := 0
	writtenLargeToolRequest, _ := transport.waitWritten(t, &writeOffset, len(largeToolRequestWire))
	assertDispatcherStressWireFrame(t, writtenLargeToolRequest, largeToolRequest)
	_ = dispatcherStressEventsUntilWrite(t, events, largeToolRequest.RequestID, "tool_request", "write_completed")

	hostRequest := hostRequestFrame(hostRequestID, "read_provided_asset")
	hostRequestWire, err := EncodeFrame(hostRequest)
	if err != nil {
		t.Fatal(err)
	}
	partialWritesBeforeHostResponse := transport.partialWriteCount()
	transport.enqueueIncoming(hostRequestWire)
	select {
	case request := <-handlerCalled:
		if request.RequestID != hostRequestID || request.ServiceName != "read_provided_asset" || len(request.Arguments) != 0 {
			t.Fatalf("host request = %#v", request)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("large host-service handler was not called")
	}

	hostResponse, err := CreateHostServiceResponse(hostRequestID, 0, responseOutput)
	if err != nil {
		t.Fatal(err)
	}
	hostResponseWire, err := EncodeFrame(hostResponse)
	if err != nil {
		t.Fatal(err)
	}
	written, partialWritesAtHostResponseCompletion := transport.waitWritten(t, &writeOffset, len(hostResponseWire))
	assertDispatcherStressWireFrame(t, written, hostResponse)
	if got := partialWritesAtHostResponseCompletion - partialWritesBeforeHostResponse; got != len(hostResponseWire) {
		t.Fatalf("partial writes for host response = %d, want %d one-byte sends", got, len(hostResponseWire))
	}

	hostEvents := dispatcherStressEventsUntilWrite(t, events, hostRequestID, "host_response", "write_completed")
	assertDispatcherStressProgress(t, hostEvents, hostRequestID, len(hostResponseWire))

	largeToolResponsePayload := make([]byte, 4+len(responseOutput))
	copy(largeToolResponsePayload[4:], responseOutput)
	largeToolResponse := Frame{MessageType: InvokeToolResponse, RequestID: largeToolRequest.RequestID, Payload: largeToolResponsePayload}
	largeToolResponseWire, err := EncodeFrame(largeToolResponse)
	if err != nil {
		t.Fatal(err)
	}
	transport.enqueueIncoming(largeToolResponseWire)
	select {
	case result := <-largeExchangeResult:
		if result.err != nil {
			t.Fatalf("large nested exchange error = %v", result.err)
		}
		assertFrameEqual(t, result.frame, largeToolResponse)
	case <-time.After(2 * time.Second):
		t.Fatal("large nested exchange did not finish")
	}

	toolRequest := Frame{MessageType: ListToolsRequest, RequestID: 93}
	toolRequestWire, err := EncodeFrame(toolRequest)
	if err != nil {
		t.Fatal(err)
	}
	exchangeResult := make(chan struct {
		frame Frame
		err   error
	}, 1)
	go func() {
		frame, exchangeErr := dispatcher.Exchange(context.Background(), toolRequest, 2*time.Second)
		exchangeResult <- struct {
			frame Frame
			err   error
		}{frame, exchangeErr}
	}()
	writtenToolRequest, _ := transport.waitWritten(t, &writeOffset, len(toolRequestWire))
	assertDispatcherStressWireFrame(t, writtenToolRequest, toolRequest)

	toolResponse := Frame{MessageType: ListToolsResponse, RequestID: toolRequest.RequestID, Payload: []byte{0, 0}}
	toolResponseWire, err := EncodeFrame(toolResponse)
	if err != nil {
		t.Fatal(err)
	}
	transport.enqueueIncoming(toolResponseWire)
	select {
	case result := <-exchangeResult:
		if result.err != nil {
			t.Fatalf("ordinary exchange error = %v", result.err)
		}
		assertFrameEqual(t, result.frame, toolResponse)
	case <-time.After(2 * time.Second):
		t.Fatal("ordinary exchange did not finish")
	}

	buildPayload, err := EncodeInvokeRequest("build", nil)
	if err != nil {
		t.Fatal(err)
	}
	buildRequest := Frame{MessageType: InvokeToolRequest, RequestID: 95, Payload: buildPayload}
	buildRequestWire, err := EncodeFrame(buildRequest)
	if err != nil {
		t.Fatal(err)
	}
	buildExchangeResult := make(chan struct {
		frame Frame
		err   error
	}, 1)
	go func() {
		frame, exchangeErr := dispatcher.Exchange(context.Background(), buildRequest, 2*time.Second)
		buildExchangeResult <- struct {
			frame Frame
			err   error
		}{frame, exchangeErr}
	}()
	writtenBuildRequest, _ := transport.waitWritten(t, &writeOffset, len(buildRequestWire))
	assertDispatcherStressWireFrame(t, writtenBuildRequest, buildRequest)
	buildHostRequest := hostRequestFrame(72, "build")
	buildHostRequestWire, err := EncodeFrame(buildHostRequest)
	if err != nil {
		t.Fatal(err)
	}
	transport.enqueueIncoming(buildHostRequestWire)
	select {
	case request := <-handlerCalled:
		if request.RequestID != 72 || request.ServiceName != "build" {
			t.Fatalf("build host request = %#v", request)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("build host-service handler was not called")
	}
	buildHostResponse, err := CreateHostServiceResponse(72, 0, []byte("built"))
	if err != nil {
		t.Fatal(err)
	}
	buildHostResponseWire, err := EncodeFrame(buildHostResponse)
	if err != nil {
		t.Fatal(err)
	}
	writtenBuildHostResponse, _ := transport.waitWritten(t, &writeOffset, len(buildHostResponseWire))
	assertDispatcherStressWireFrame(t, writtenBuildHostResponse, buildHostResponse)

	buildToolResponse := Frame{MessageType: InvokeToolResponse, RequestID: buildRequest.RequestID, Payload: append([]byte{0, 0, 0, 0}, []byte("built")...)}
	buildToolResponseWire, err := EncodeFrame(buildToolResponse)
	if err != nil {
		t.Fatal(err)
	}
	transport.enqueueIncoming(buildToolResponseWire)
	select {
	case result := <-buildExchangeResult:
		if result.err != nil {
			t.Fatalf("build exchange error = %v", result.err)
		}
		assertFrameEqual(t, result.frame, buildToolResponse)
	case <-time.After(2 * time.Second):
		t.Fatal("build exchange did not finish")
	}

	if err := dispatcher.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDispatcherCloseDuringBlockedLargeHostResponse(t *testing.T) {
	transport := newDispatcherStressSerial(true)
	dispatcher := NewSerialProtocolDispatcher(transport, DispatcherOptions{
		BackgroundHostServices: func(_ context.Context, request HostRequest) (Frame, error) {
			return CreateHostServiceResponse(request.RequestID, 0, dispatcherStressPayload())
		},
	})
	dispatcher.closeTimeout = 250 * time.Millisecond
	t.Cleanup(func() { _ = dispatcher.Close() })
	if err := dispatcher.StartReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	requestID := uint32(101)
	requestWire, err := EncodeFrame(hostRequestFrame(requestID, "blocked_close"))
	if err != nil {
		t.Fatal(err)
	}
	transport.enqueueIncoming(requestWire)
	select {
	case <-transport.blockStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("large host response did not reach the blocked write")
	}

	started := time.Now()
	closeResult := make(chan error, 1)
	go func() { closeResult <- dispatcher.Close() }()
	select {
	case err := <-closeResult:
		if err != nil {
			t.Fatalf("Close during blocked host response: %v", err)
		}
		if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
			t.Fatalf("Close during blocked host response took %s", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close during blocked host response did not finish")
	}
	select {
	case <-transport.closed:
	default:
		t.Fatal("Close did not close the serial transport")
	}
}

func TestDispatcherAbortDuringBlockedLargeHostResponse(t *testing.T) {
	transport := newDispatcherStressSerial(false)
	events := make(chan dispatcherStressEvent, 256)
	dispatcher := NewSerialProtocolDispatcher(transport, DispatcherOptions{
		ExchangeHostServices: func(_ context.Context, request HostRequest) (Frame, error) {
			return CreateHostServiceResponse(request.RequestID, 0, dispatcherStressPayload())
		},
		EventRecorder: dispatcherStressEventRecorder(events),
	})
	dispatcher.closeTimeout = 250 * time.Millisecond
	t.Cleanup(func() { _ = dispatcher.Close() })
	if err := dispatcher.StartReady(context.Background()); err != nil {
		t.Fatal(err)
	}

	request := Frame{MessageType: ListToolsRequest, RequestID: 113, Payload: []byte("abort after host response starts")}
	requestWire, err := EncodeFrame(request)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exchangeResult := make(chan error, 1)
	go func() {
		_, exchangeErr := dispatcher.Exchange(ctx, request, 5*time.Second)
		exchangeResult <- exchangeErr
	}()
	writeOffset := 0
	writtenRequest, _ := transport.waitWritten(t, &writeOffset, len(requestWire))
	assertDispatcherStressWireFrame(t, writtenRequest, request)
	_ = dispatcherStressEventsUntilWrite(t, events, request.RequestID, "tool_request", "write_completed")
	transport.setBlockWrites(true)
	hostRequestID := uint32(127)
	hostRequestWire, err := EncodeFrame(hostRequestFrame(hostRequestID, "blocked_abort"))
	if err != nil {
		t.Fatal(err)
	}
	transport.enqueueIncoming(hostRequestWire)
	select {
	case <-transport.blockStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("large host response did not reach the blocked write")
	}

	cancel()
	select {
	case err := <-exchangeResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("aborted Exchange error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("aborted Exchange did not finish")
	}
	cancelled := waitForDispatcherStressEvent(t, events, func(event dispatcherStressEvent) bool {
		return event.name == "serial_protocol_write" &&
			event.data["request_id"] == hostRequestID &&
			event.data["write_kind"] == "host_response" &&
			event.data["phase"] == "write_cancelled"
	})
	if got := cancelled.data["bytes_sent"]; got != 0 {
		t.Fatalf("cancelled host response bytes_sent = %v, want 0", got)
	}

	started := time.Now()
	if err := dispatcher.Close(); err != nil {
		t.Fatalf("Close after aborted host response: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("Close after aborted host response took %s", elapsed)
	}
}

func dispatcherStressPayload() []byte {
	payload := make([]byte, 1<<20)
	for index := range payload {
		payload[index] = byte(index)
	}
	return payload
}

type dispatcherStressEvent struct {
	name string
	data map[string]any
}

func dispatcherStressEventRecorder(events chan<- dispatcherStressEvent) SerialEventRecorder {
	return func(name string, data map[string]any) {
		copyData := make(map[string]any, len(data))
		for key, value := range data {
			copyData[key] = value
		}
		events <- dispatcherStressEvent{name: name, data: copyData}
	}
}

func dispatcherStressEventsUntilWrite(t *testing.T, events <-chan dispatcherStressEvent, requestID uint32, writeKind, phase string) []dispatcherStressEvent {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	collected := make([]dispatcherStressEvent, 0, 24)
	for {
		select {
		case event := <-events:
			collected = append(collected, event)
			if event.name == "serial_protocol_write" && event.data["request_id"] == requestID && event.data["write_kind"] == writeKind && event.data["phase"] == phase {
				return collected
			}
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s/%s write event for request %d", writeKind, phase, requestID)
		}
	}
}

func waitForDispatcherStressEvent(t *testing.T, events <-chan dispatcherStressEvent, match func(dispatcherStressEvent) bool) dispatcherStressEvent {
	t.Helper()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case event := <-events:
			if match(event) {
				return event
			}
		case <-deadline.C:
			t.Fatal("timed out waiting for dispatcher event")
		}
	}
}

func assertDispatcherStressProgress(t *testing.T, events []dispatcherStressEvent, requestID uint32, totalBytes int) {
	t.Helper()
	progress := make([]int, 0, totalBytes/dispatcherWriteProgress)
	completed := false
	started := false
	for _, event := range events {
		if event.name != "serial_protocol_write" || event.data["request_id"] != requestID || event.data["write_kind"] != "host_response" {
			continue
		}
		if event.data["total_bytes"] != totalBytes {
			t.Fatalf("write event total_bytes = %v, want %d", event.data["total_bytes"], totalBytes)
		}
		if event.data["scope"] != "exchange" {
			t.Fatalf("host response write scope = %v, want exchange", event.data["scope"])
		}
		switch event.data["phase"] {
		case "write_started":
			started = true
			if event.data["bytes_sent"] != 0 {
				t.Fatalf("write_started bytes_sent = %v, want 0", event.data["bytes_sent"])
			}
		case "write_progress":
			bytesSent, ok := event.data["bytes_sent"].(int)
			if !ok {
				t.Fatalf("write_progress bytes_sent has type %T", event.data["bytes_sent"])
			}
			progress = append(progress, bytesSent)
		case "write_completed":
			completed = true
			if event.data["bytes_sent"] != totalBytes {
				t.Fatalf("write_completed bytes_sent = %v, want %d", event.data["bytes_sent"], totalBytes)
			}
		}
	}
	if !started || !completed {
		t.Fatalf("write events missing start/completion: started=%v completed=%v", started, completed)
	}
	expected := make([]int, 0, len(progress))
	for bytesSent := dispatcherWriteProgress; bytesSent <= totalBytes; bytesSent += dispatcherWriteProgress {
		expected = append(expected, bytesSent)
	}
	if !bytes.Equal(intsAsBytes(progress), intsAsBytes(expected)) {
		t.Fatalf("write progress = %v, want %v", progress, expected)
	}
}

func intsAsBytes(values []int) []byte {
	result := make([]byte, 0, len(values)*8)
	for _, value := range values {
		result = append(result, byte(value), byte(value>>8), byte(value>>16), byte(value>>24))
	}
	return result
}

func assertDispatcherStressWireFrame(t *testing.T, wire []byte, want Frame) {
	t.Helper()
	buffer := append([]byte(nil), wire...)
	got, complete, err := ExtractFrame(&buffer)
	if err != nil || !complete || len(buffer) != 0 {
		t.Fatalf("ExtractFrame() = %#v, complete=%v, err=%v, trailing=%d", got, complete, err, len(buffer))
	}
	assertFrameEqual(t, got, want)
}

type dispatcherStressSerial struct {
	mutex          sync.Mutex
	incoming       []byte
	written        []byte
	incomingNotify chan struct{}
	writtenNotify  chan struct{}
	closed         chan struct{}
	blockStarted   chan struct{}
	blockOnce      sync.Once
	closeOnce      sync.Once
	closedFlag     bool
	blockWrites    bool
	partialWrites  int
}

const dispatcherStressWriteTimeout = 10 * time.Second

func newDispatcherStressSerial(blockWrites bool) *dispatcherStressSerial {
	return &dispatcherStressSerial{
		incomingNotify: make(chan struct{}),
		writtenNotify:  make(chan struct{}),
		closed:         make(chan struct{}),
		blockStarted:   make(chan struct{}),
		blockWrites:    blockWrites,
	}
}

func (s *dispatcherStressSerial) enqueueIncoming(data []byte) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if s.closedFlag {
		return
	}
	s.incoming = append(s.incoming, data...)
	close(s.incomingNotify)
	s.incomingNotify = make(chan struct{})
}

func (s *dispatcherStressSerial) setBlockWrites(block bool) {
	s.mutex.Lock()
	s.blockWrites = block
	s.mutex.Unlock()
}

func (s *dispatcherStressSerial) Pump(ctx context.Context, maxReadBytes int, outgoing []byte, timeout time.Duration) (SerialPumpResult, error) {
	if ctx == nil {
		return SerialPumpResult{}, errors.New("stress serial context is nil")
	}
	for {
		if err := ctx.Err(); err != nil {
			return SerialPumpResult{}, err
		}
		s.mutex.Lock()
		if s.closedFlag {
			s.mutex.Unlock()
			return SerialPumpResult{}, errors.New("stress serial is closed")
		}
		var incoming []byte
		if len(s.incoming) != 0 {
			count := min(maxReadBytes, len(s.incoming))
			incoming = append([]byte(nil), s.incoming[:count]...)
			s.incoming = s.incoming[count:]
		}
		if len(outgoing) != 0 && s.blockWrites && incoming == nil {
			s.blockOnce.Do(func() { close(s.blockStarted) })
			closed := s.closed
			s.mutex.Unlock()
			select {
			case <-ctx.Done():
				return SerialPumpResult{}, ctx.Err()
			case <-closed:
				return SerialPumpResult{}, errors.New("stress serial is closed")
			}
		}
		result := SerialPumpResult{Incoming: incoming}
		if len(outgoing) != 0 && !s.blockWrites {
			s.written = append(s.written, outgoing[0])
			s.partialWrites++
			result.Sent = 1
			close(s.writtenNotify)
			s.writtenNotify = make(chan struct{})
		}
		if result.Incoming != nil || result.Sent != 0 {
			s.mutex.Unlock()
			return result, nil
		}
		notify := s.incomingNotify
		closed := s.closed
		s.mutex.Unlock()

		timer := time.NewTimer(timeout)
		select {
		case <-ctx.Done():
			timer.Stop()
			return SerialPumpResult{}, ctx.Err()
		case <-closed:
			timer.Stop()
			return SerialPumpResult{}, errors.New("stress serial is closed")
		case <-notify:
			timer.Stop()
		case <-timer.C:
			return SerialPumpResult{}, nil
		}
	}
}

func (s *dispatcherStressSerial) Close() error {
	s.closeOnce.Do(func() {
		s.mutex.Lock()
		s.closedFlag = true
		close(s.closed)
		close(s.incomingNotify)
		close(s.writtenNotify)
		s.mutex.Unlock()
	})
	return nil
}

func (s *dispatcherStressSerial) waitWritten(t *testing.T, offset *int, count int) ([]byte, int) {
	t.Helper()
	deadline := time.NewTimer(dispatcherStressWriteTimeout)
	defer deadline.Stop()
	for {
		s.mutex.Lock()
		if len(s.written)-*offset >= count {
			result := append([]byte(nil), s.written[*offset:*offset+count]...)
			partialWrites := s.partialWrites
			*offset += count
			s.mutex.Unlock()
			return result, partialWrites
		}
		notify := s.writtenNotify
		closed := s.closed
		available := len(s.written) - *offset
		s.mutex.Unlock()
		select {
		case <-notify:
		case <-closed:
			t.Fatalf("stress serial closed after %d of %d expected bytes", available, count)
		case <-deadline.C:
			t.Fatalf("timed out waiting for %d serial bytes", count)
		}
	}
}

func (s *dispatcherStressSerial) partialWriteCount() int {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	return s.partialWrites
}
