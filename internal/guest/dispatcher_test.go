package guest

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDispatcherFragmentedReadyAndExchange(t *testing.T) {
	serial, peer, wait := connectedSerial(t)
	defer peer.Close()
	dispatcher := NewSerialProtocolDispatcher(serial, DispatcherOptions{})
	if err := dispatcher.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dispatcher.Close() })
	if _, err := peer.Write([]byte("boot\x1b[2J" + ReadyMarker[:7])); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.Write([]byte(ReadyMarker[7:])); err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.WaitUntilReady(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	if got := string(dispatcher.StartupDiagnostic()); got != "boot\x1b[2J" {
		t.Fatalf("startup diagnostic = %q", got)
	}
	if err := dispatcher.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "already started") {
		t.Fatalf("duplicate Start error = %v", err)
	}

	result := make(chan struct {
		frame Frame
		err   error
	}, 1)
	go func() {
		frame, err := dispatcher.Exchange(context.Background(), Frame{MessageType: ListToolsRequest, RequestID: 9}, time.Second)
		result <- struct {
			frame Frame
			err   error
		}{frame, err}
	}()
	request := readPeerFrame(t, peer)
	if request.MessageType != ListToolsRequest || request.RequestID != 9 {
		t.Fatalf("request = %#v", request)
	}
	writePeerFrame(t, peer, Frame{MessageType: ListToolsResponse, RequestID: 9, Payload: []byte{0, 0}})
	select {
	case exchange := <-result:
		if exchange.err != nil || exchange.frame.MessageType != ListToolsResponse || exchange.frame.RequestID != 9 {
			t.Fatalf("Exchange = %#v, %v", exchange.frame, exchange.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Exchange did not finish")
	}
	if err := dispatcher.Close(); err != nil {
		t.Fatal(err)
	}
	wait()
}

func TestDispatcherPreReadyHostService(t *testing.T) {
	serial, peer, wait := connectedSerial(t)
	defer peer.Close()
	called := make(chan HostRequest, 1)
	dispatcher := NewSerialProtocolDispatcher(serial, DispatcherOptions{
		StartupHostServices: func(_ context.Context, request HostRequest) (Frame, error) {
			called <- request
			return CreateHostServiceResponse(request.RequestID, 0, []byte("asset"))
		},
	})
	if err := dispatcher.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dispatcher.Close() })
	writePeerFrame(t, peer, hostRequestFrame(17, "provided_asset", []byte("name")))
	response := readPeerFrame(t, peer)
	if response.MessageType != HostServiceResponse || response.RequestID != 17 || string(response.Payload[4:]) != "asset" {
		t.Fatalf("host response = %#v", response)
	}
	select {
	case request := <-called:
		if request.ServiceName != "provided_asset" || string(request.Arguments[0]) != "name" {
			t.Fatalf("host request = %#v", request)
		}
	default:
		t.Fatal("startup handler was not called")
	}
	if _, err := peer.Write([]byte(ReadyMarker)); err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.WaitUntilReady(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	_ = dispatcher.Close()
	wait()
}

func TestDispatcherSuspendsExchangeTimeoutForNestedHostService(t *testing.T) {
	serial, peer, wait := connectedSerial(t)
	defer peer.Close()
	dispatcher := NewSerialProtocolDispatcher(serial, DispatcherOptions{
		ExchangeHostServices: func(_ context.Context, request HostRequest) (Frame, error) {
			time.Sleep(60 * time.Millisecond)
			return CreateHostServiceResponse(request.RequestID, 0, []byte("built"))
		},
	})
	if err := dispatcher.StartReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dispatcher.Close() })
	result := make(chan error, 1)
	go func() {
		_, err := dispatcher.Exchange(context.Background(), Frame{MessageType: InvokeToolRequest, RequestID: 3}, 20*time.Millisecond)
		result <- err
	}()
	_ = readPeerFrame(t, peer)
	writePeerFrame(t, peer, hostRequestFrame(44, "build", nil))
	hostResponse := readPeerFrame(t, peer)
	if hostResponse.MessageType != HostServiceResponse || hostResponse.RequestID != 44 {
		t.Fatalf("nested host response = %#v", hostResponse)
	}
	writePeerFrame(t, peer, Frame{MessageType: InvokeToolResponse, RequestID: 3, Payload: make([]byte, 4)})
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("Exchange after nested host service: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("nested Exchange did not finish")
	}
	_ = dispatcher.Close()
	wait()
}

func TestDispatcherRejectsUnexpectedResponse(t *testing.T) {
	serial, peer, wait := connectedSerial(t)
	defer peer.Close()
	dispatcher := NewSerialProtocolDispatcher(serial, DispatcherOptions{})
	if err := dispatcher.StartReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dispatcher.Close() })
	writePeerFrame(t, peer, Frame{MessageType: ListToolsResponse, RequestID: 1})
	deadline := time.Now().Add(time.Second)
	for {
		dispatcher.mutex.Lock()
		failure := dispatcher.failure
		dispatcher.mutex.Unlock()
		if failure != nil {
			if !strings.Contains(failure.Error(), "unexpected") {
				t.Fatalf("failure = %v", failure)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("unexpected response did not fail dispatcher")
		}
		time.Sleep(time.Millisecond)
	}
	_ = dispatcher.Close()
	wait()
}

func TestDispatcherWriteStallAndBoundedClose(t *testing.T) {
	transport := &stalledDuplexSerial{closed: make(chan struct{})}
	dispatcher := NewSerialProtocolDispatcher(transport, DispatcherOptions{})
	dispatcher.writeStall = 20 * time.Millisecond
	dispatcher.pollInterval = time.Millisecond
	if err := dispatcher.StartReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := dispatcher.Exchange(context.Background(), Frame{MessageType: ListToolsRequest, RequestID: 1}, time.Second)
	if err == nil || !strings.Contains(err.Error(), "timed out writing") {
		t.Fatalf("Exchange error = %v, want write timeout", err)
	}
	if err := dispatcher.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-transport.closed:
	default:
		t.Fatal("Close did not close transport")
	}
}

func TestDispatcherCancellationPoisonsQueuedExchange(t *testing.T) {
	transport := &stalledDuplexSerial{closed: make(chan struct{})}
	dispatcher := NewSerialProtocolDispatcher(transport, DispatcherOptions{})
	dispatcher.writeStall = time.Second
	dispatcher.pollInterval = time.Millisecond
	if err := dispatcher.StartReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := dispatcher.Exchange(ctx, Frame{MessageType: ListToolsRequest, RequestID: 1}, time.Second)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Exchange error = %v, want deadline exceeded", err)
	}
	select {
	case <-transport.closed:
	case <-time.After(time.Second):
		t.Fatal("abandoned exchange did not close transport")
	}
	_, secondErr := dispatcher.Exchange(context.Background(), Frame{MessageType: ListToolsRequest, RequestID: 2}, time.Second)
	if !errors.Is(secondErr, context.DeadlineExceeded) {
		t.Fatalf("second Exchange error = %v, want preserved terminal failure", secondErr)
	}
	if err := dispatcher.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestDispatcherRejectsInvalidTransportWriteCount(t *testing.T) {
	transport := &invalidCountDuplexSerial{closed: make(chan struct{})}
	dispatcher := NewSerialProtocolDispatcher(transport, DispatcherOptions{})
	if err := dispatcher.StartReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, err := dispatcher.Exchange(context.Background(), Frame{MessageType: ListToolsRequest, RequestID: 1}, time.Second)
	if err == nil || !strings.Contains(err.Error(), "invalid write count") {
		t.Fatalf("Exchange error = %v", err)
	}
	_ = dispatcher.Close()
}

func TestDispatcherHandlerPanicBecomesBoundedFailureResponse(t *testing.T) {
	serial, peer, wait := connectedSerial(t)
	defer peer.Close()
	dispatcher := NewSerialProtocolDispatcher(serial, DispatcherOptions{
		BackgroundHostServices: func(context.Context, HostRequest) (Frame, error) { panic("secret panic") },
	})
	if err := dispatcher.StartReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dispatcher.Close() })
	writePeerFrame(t, peer, hostRequestFrame(8, "panic", nil))
	response := readPeerFrame(t, peer)
	if binary.LittleEndian.Uint32(response.Payload[:4]) != 2 || !strings.Contains(string(response.Payload[4:]), "trusted host-service failure") {
		t.Fatalf("panic response = %#v", response)
	}
	_ = dispatcher.Close()
	wait()
}

func TestDispatcherRoutesBackgroundAndExchangeHostServices(t *testing.T) {
	serial, peer, wait := connectedSerial(t)
	defer peer.Close()
	called := make(chan string, 2)
	dispatcher := NewSerialProtocolDispatcher(serial, DispatcherOptions{
		BackgroundHostServices: func(_ context.Context, request HostRequest) (Frame, error) {
			called <- "background:" + request.ServiceName
			return CreateHostServiceResponse(request.RequestID, 0, []byte("idle"))
		},
		ExchangeHostServices: func(_ context.Context, request HostRequest) (Frame, error) {
			called <- "exchange:" + request.ServiceName
			return CreateHostServiceResponse(request.RequestID, 0, []byte("nested"))
		},
	})
	if err := dispatcher.StartReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dispatcher.Close() })

	writePeerFrame(t, peer, hostRequestFrame(31, "idle_service"))
	backgroundResponse := readPeerFrame(t, peer)
	if backgroundResponse.RequestID != 31 || string(backgroundResponse.Payload[4:]) != "idle" {
		t.Fatalf("background response = %#v", backgroundResponse)
	}

	exchangeResult := make(chan error, 1)
	go func() {
		_, err := dispatcher.Exchange(context.Background(), Frame{MessageType: ListToolsRequest, RequestID: 32}, time.Second)
		exchangeResult <- err
	}()
	_ = readPeerFrame(t, peer)
	writePeerFrame(t, peer, hostRequestFrame(33, "nested_service"))
	exchangeResponse := readPeerFrame(t, peer)
	if exchangeResponse.RequestID != 33 || string(exchangeResponse.Payload[4:]) != "nested" {
		t.Fatalf("exchange host response = %#v", exchangeResponse)
	}
	writePeerFrame(t, peer, Frame{MessageType: ListToolsResponse, RequestID: 32, Payload: []byte{0, 0}})
	if err := <-exchangeResult; err != nil {
		t.Fatal(err)
	}
	if first, second := <-called, <-called; first != "background:idle_service" || second != "exchange:nested_service" {
		t.Fatalf("host-service routes = %q, %q", first, second)
	}
	_ = dispatcher.Close()
	wait()
}

func TestDispatcherRecoversAfterMalformedNonzeroHostRequest(t *testing.T) {
	serial, peer, wait := connectedSerial(t)
	defer peer.Close()
	dispatcher := NewSerialProtocolDispatcher(serial, DispatcherOptions{
		BackgroundHostServices: func(_ context.Context, request HostRequest) (Frame, error) {
			return CreateHostServiceResponse(request.RequestID, 0, []byte("valid"))
		},
	})
	if err := dispatcher.StartReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dispatcher.Close() })

	writePeerFrame(t, peer, Frame{MessageType: HostServiceRequest, RequestID: 41, Payload: []byte{0}})
	malformedResponse := readPeerFrame(t, peer)
	if malformedResponse.RequestID != 41 || binary.LittleEndian.Uint32(malformedResponse.Payload[:4]) != 1 {
		t.Fatalf("malformed host response = %#v", malformedResponse)
	}
	writePeerFrame(t, peer, hostRequestFrame(42, "valid_service"))
	validResponse := readPeerFrame(t, peer)
	if validResponse.RequestID != 42 || binary.LittleEndian.Uint32(validResponse.Payload[:4]) != 0 || string(validResponse.Payload[4:]) != "valid" {
		t.Fatalf("valid host response = %#v", validResponse)
	}
	_ = dispatcher.Close()
	wait()
}

func TestDispatcherEventRecorderCanInspectDispatcher(t *testing.T) {
	serial, peer, wait := connectedSerial(t)
	defer peer.Close()
	var dispatcher *SerialProtocolDispatcher
	recorded := make(chan string, 8)
	dispatcher = NewSerialProtocolDispatcher(serial, DispatcherOptions{
		EventRecorder: func(event string, _ map[string]any) {
			_ = dispatcher.StartupDiagnostic()
			recorded <- event
		},
	})
	if err := dispatcher.StartReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dispatcher.Close() })
	result := make(chan error, 1)
	go func() {
		_, err := dispatcher.Exchange(context.Background(), Frame{MessageType: ListToolsRequest, RequestID: 5}, time.Second)
		result <- err
	}()
	request := readPeerFrame(t, peer)
	writePeerFrame(t, peer, Frame{MessageType: ListToolsResponse, RequestID: request.RequestID, Payload: []byte{0, 0}})
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("reentrant event recorder deadlocked dispatcher")
	}
	if len(recorded) == 0 {
		t.Fatal("event recorder was not called")
	}
	_ = dispatcher.Close()
	wait()
}

func TestDispatcherHandlerCanCloseWithoutSelfWait(t *testing.T) {
	serial, peer, wait := connectedSerial(t)
	defer peer.Close()
	var dispatcher *SerialProtocolDispatcher
	closeResult := make(chan error, 1)
	recorded := make(chan string, 8)
	dispatcher = NewSerialProtocolDispatcher(serial, DispatcherOptions{
		BackgroundHostServices: func(ctx context.Context, _ HostRequest) (Frame, error) {
			dispatcher.mutex.Lock()
			dispatcher.recordLocked("callback_before_close", map[string]any{})
			dispatcher.mutex.Unlock()
			closeResult <- dispatcher.CloseFromCallback(ctx)
			return Frame{}, errors.New("closed")
		},
		EventRecorder: func(event string, _ map[string]any) { recorded <- event },
	})
	dispatcher.closeTimeout = 100 * time.Millisecond
	if err := dispatcher.StartReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	writePeerFrame(t, peer, hostRequestFrame(12, "close", nil))
	select {
	case err := <-closeResult:
		if err != nil {
			t.Fatalf("handler Close: %v", err)
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("handler Close waited for its own reader")
	}
	if err := dispatcher.Close(); err != nil {
		t.Fatal(err)
	}
	foundCallbackEvent := false
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	for !foundCallbackEvent {
		select {
		case event := <-recorded:
			foundCallbackEvent = event == "callback_before_close"
		case <-deadline.C:
			t.Fatal("self-close discarded an event queued by its callback")
		}
	}
	wait()
}

func TestDispatcherExternalCloseWaitsForHostServiceCallback(t *testing.T) {
	serial, peer, wait := connectedSerial(t)
	defer peer.Close()
	handlerStarted := make(chan struct{})
	releaseHandler := make(chan struct{})
	handlerReturned := make(chan struct{})
	dispatcher := NewSerialProtocolDispatcher(serial, DispatcherOptions{
		BackgroundHostServices: func(context.Context, HostRequest) (Frame, error) {
			close(handlerStarted)
			<-releaseHandler
			close(handlerReturned)
			return Frame{}, errors.New("handler released")
		},
	})
	dispatcher.closeTimeout = 500 * time.Millisecond
	if err := dispatcher.StartReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	writePeerFrame(t, peer, hostRequestFrame(13, "blocked", nil))
	select {
	case <-handlerStarted:
	case <-time.After(time.Second):
		t.Fatal("host-service handler was not called")
	}

	closeResult := make(chan error, 1)
	go func() { closeResult <- dispatcher.Close() }()
	select {
	case err := <-closeResult:
		t.Fatalf("external Close returned while callback was active: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseHandler)
	select {
	case err := <-closeResult:
		if err != nil {
			t.Fatalf("external Close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("external Close did not join the reader after callback completion")
	}
	select {
	case <-handlerReturned:
	default:
		t.Fatal("host-service callback had not returned before external Close completed")
	}
	wait()
}

type stalledDuplexSerial struct {
	once   sync.Once
	closed chan struct{}
}

func (s *stalledDuplexSerial) Pump(ctx context.Context, _ int, _ []byte, timeout time.Duration) (SerialPumpResult, error) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return SerialPumpResult{}, ctx.Err()
	case <-timer.C:
		return SerialPumpResult{}, nil
	}
}
func (s *stalledDuplexSerial) Close() error { s.once.Do(func() { close(s.closed) }); return nil }

type invalidCountDuplexSerial struct {
	once   sync.Once
	closed chan struct{}
}

func (s *invalidCountDuplexSerial) Pump(_ context.Context, _ int, outgoing []byte, _ time.Duration) (SerialPumpResult, error) {
	if len(outgoing) == 0 {
		return SerialPumpResult{}, nil
	}
	return SerialPumpResult{Sent: len(outgoing) + 1}, nil
}
func (s *invalidCountDuplexSerial) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

func connectedSerial(t *testing.T) (*SerialConnection, net.Conn, func()) {
	t.Helper()
	path, peer, wait := startSerialPeer(t)
	serial := NewSerialConnection(path)
	if err := serial.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	return serial, peer, wait
}

func readPeerFrame(t *testing.T, peer net.Conn) Frame {
	t.Helper()
	if err := peer.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	frame, err := ReadFrame(peer)
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func writePeerFrame(t *testing.T, peer net.Conn, frame Frame) {
	t.Helper()
	encoded, err := EncodeFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	if err := peer.SetWriteDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.Write(encoded); err != nil {
		t.Fatal(err)
	}
}

func hostRequestFrame(requestID uint32, name string, arguments ...[]byte) Frame {
	payload := make([]byte, 2+len(name)+2)
	binary.LittleEndian.PutUint16(payload[:2], uint16(len(name)))
	copy(payload[2:], name)
	offset := 2 + len(name)
	binary.LittleEndian.PutUint16(payload[offset:offset+2], uint16(len(arguments)))
	for _, argument := range arguments {
		encoded := make([]byte, 4+len(argument))
		binary.LittleEndian.PutUint32(encoded[:4], uint32(len(argument)))
		copy(encoded[4:], argument)
		payload = append(payload, encoded...)
	}
	return Frame{MessageType: HostServiceRequest, RequestID: requestID, Payload: payload}
}
