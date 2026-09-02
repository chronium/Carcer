package guest

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"time"
)

const (
	ReadyMarker               = "CODEXOS-SEED-READY\n"
	dispatcherReadBytes       = 4096
	dispatcherPollInterval    = 10 * time.Millisecond
	dispatcherCloseTimeout    = 5 * time.Second
	dispatcherWriteChunkBytes = 16 * 1024
	dispatcherWriteStall      = 5 * time.Second
	dispatcherWriteProgress   = 64 * 1024
	dispatcherMaxQueuedWrites = 8
	dispatcherMaxQueuedBytes  = 32*1024*1024 + 8*frameHeaderSize
	maxHostDiagnosticBytes    = 4 * 1024
)

type DuplexSerial interface {
	Pump(context.Context, int, []byte, time.Duration) (SerialPumpResult, error)
	Close() error
}

type HostServiceHandler func(context.Context, HostRequest) (Frame, error)
type SerialEventRecorder func(string, map[string]any)

// dispatcherCallbackContextKey marks the context passed to a host-service
// callback.  CloseFromCallback uses this marker to prove that a callback has
// explicitly opted into the self-close path; Close itself always waits for
// the reader to stop.
type dispatcherCallbackContextKey struct{}

type dispatcherCallbackToken struct {
	dispatcher *SerialProtocolDispatcher
	active     bool
}

type DispatcherError struct {
	Reason string
	Err    error
}

func (e *DispatcherError) Error() string {
	if e.Err == nil {
		return e.Reason
	}
	return e.Reason + ": " + e.Err.Error()
}

func (e *DispatcherError) Unwrap() error { return e.Err }

type DispatcherOptions struct {
	StartupHostServices    HostServiceHandler
	BackgroundHostServices HostServiceHandler
	ExchangeHostServices   HostServiceHandler
	EventRecorder          SerialEventRecorder
}

type queuedSerialWrite struct {
	data          []byte
	kind          string
	requestID     uint32
	scope         string
	offset        int
	nextProgress  int
	stallDeadline time.Time
	complete      bool
	terminal      string
}

type serialEvent struct {
	name string
	data map[string]any
}

type SerialProtocolDispatcher struct {
	serial  DuplexSerial
	options DispatcherOptions

	mutex      sync.Mutex
	exchangeMu sync.Mutex
	notify     chan struct{}
	done       chan struct{}
	cancel     context.CancelFunc
	started    bool
	ready      bool
	closing    bool
	failure    error

	pendingResponse bool
	response        *Frame
	hostActive      int
	hostWindow      uint64
	startupBuffer   []byte
	frameBuffer     []byte
	diagnostic      []byte
	writes          []*queuedSerialWrite
	queuedBytes     int
	events          []serialEvent
	callbackActive  int
	flushingEvents  bool

	pollInterval time.Duration
	closeTimeout time.Duration
	writeStall   time.Duration
	writeChunk   int
}

func NewSerialProtocolDispatcher(serial DuplexSerial, options DispatcherOptions) *SerialProtocolDispatcher {
	return &SerialProtocolDispatcher{
		serial: serial, options: options, notify: make(chan struct{}), done: make(chan struct{}),
		pollInterval: dispatcherPollInterval, closeTimeout: dispatcherCloseTimeout,
		writeStall: dispatcherWriteStall, writeChunk: dispatcherWriteChunkBytes,
	}
}

func (d *SerialProtocolDispatcher) Start(ctx context.Context) error      { return d.start(ctx, false) }
func (d *SerialProtocolDispatcher) StartReady(ctx context.Context) error { return d.start(ctx, true) }

func (d *SerialProtocolDispatcher) start(ctx context.Context, ready bool) error {
	if ctx == nil {
		return &DispatcherError{Reason: "serial protocol context is nil"}
	}
	d.mutex.Lock()
	defer d.mutex.Unlock()
	if d.started {
		return &DispatcherError{Reason: "serial protocol dispatcher has already started"}
	}
	if d.closing {
		return &DispatcherError{Reason: "serial protocol dispatcher is closed"}
	}
	readerContext, cancel := context.WithCancel(ctx)
	d.cancel = cancel
	d.started = true
	d.ready = ready
	go d.readLoop(readerContext)
	return nil
}

func (d *SerialProtocolDispatcher) WaitUntilReady(ctx context.Context, timeout time.Duration) error {
	if ctx == nil {
		return &DispatcherError{Reason: "guest readiness context is nil"}
	}
	if timeout <= 0 {
		return &DispatcherError{Reason: "guest readiness timeout must be positive"}
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		d.mutex.Lock()
		if !d.started {
			d.mutex.Unlock()
			return &DispatcherError{Reason: "serial protocol dispatcher has not started"}
		}
		if d.ready {
			d.mutex.Unlock()
			return nil
		}
		if d.failure != nil {
			err := d.failure
			d.mutex.Unlock()
			return err
		}
		notify := d.notify
		d.mutex.Unlock()
		select {
		case <-notify:
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return &DispatcherError{Reason: "timed out waiting for CODEXOS-SEED-READY"}
		}
	}
}

func (d *SerialProtocolDispatcher) Exchange(ctx context.Context, request Frame, timeout time.Duration) (Frame, error) {
	if ctx == nil {
		return Frame{}, &DispatcherError{Reason: "serial exchange context is nil"}
	}
	if timeout <= 0 {
		return Frame{}, &DispatcherError{Reason: "response timeout must be positive"}
	}
	d.exchangeMu.Lock()
	defer d.exchangeMu.Unlock()
	d.mutex.Lock()
	if !d.ready {
		d.mutex.Unlock()
		return Frame{}, &DispatcherError{Reason: "guest serial protocol is not ready"}
	}
	if d.failure != nil {
		err := d.failure
		d.mutex.Unlock()
		return Frame{}, err
	}
	if d.closing {
		d.mutex.Unlock()
		return Frame{}, &SerialError{Reason: "serial protocol dispatcher is closed"}
	}
	d.pendingResponse = true
	d.response = nil
	hostWindow := d.hostWindow
	d.mutex.Unlock()
	defer func() {
		d.mutex.Lock()
		d.pendingResponse = false
		d.response = nil
		d.signalLocked()
		d.mutex.Unlock()
	}()
	encoded, err := EncodeFrame(request)
	if err != nil {
		return Frame{}, err
	}
	queued, err := d.queueWrite(encoded, "tool_request", request.RequestID, "")
	if err != nil {
		return Frame{}, err
	}
	if err := d.waitForWrite(ctx, queued); err != nil {
		if ctx.Err() != nil {
			d.abortExchange(ctx.Err())
		}
		return Frame{}, err
	}
	deadline := time.Now().Add(timeout)
	for {
		d.mutex.Lock()
		if d.failure != nil {
			err := d.failure
			d.mutex.Unlock()
			return Frame{}, err
		}
		if d.response != nil {
			response := *d.response
			d.mutex.Unlock()
			return response, nil
		}
		active := d.hostActive != 0
		if d.hostWindow != hostWindow {
			hostWindow = d.hostWindow
			deadline = time.Now().Add(timeout)
		}
		notify := d.notify
		d.mutex.Unlock()
		if active {
			select {
			case <-notify:
			case <-ctx.Done():
				d.abortExchange(ctx.Err())
				return Frame{}, ctx.Err()
			}
			continue
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			err := &DispatcherError{Reason: "timed out waiting for tool response"}
			d.abortExchange(err)
			return Frame{}, err
		}
		timer := time.NewTimer(remaining)
		select {
		case <-notify:
			timer.Stop()
		case <-ctx.Done():
			timer.Stop()
			d.abortExchange(ctx.Err())
			return Frame{}, ctx.Err()
		case <-timer.C:
			err := &DispatcherError{Reason: "timed out waiting for tool response"}
			d.abortExchange(err)
			return Frame{}, err
		}
	}
}

func (d *SerialProtocolDispatcher) StartupDiagnostic() []byte {
	d.mutex.Lock()
	defer d.mutex.Unlock()
	combined := append(append([]byte(nil), d.diagnostic...), d.startupBuffer...)
	return combined[:min(len(combined), maxStartupDiagnosticBytes)]
}

func (d *SerialProtocolDispatcher) Close() error {
	return d.close(false)
}

// CloseFromCallback closes the dispatcher without waiting for the reader
// goroutine.  It is only valid with the context supplied to the current
// HostServiceHandler invocation.  A callback runs on the reader goroutine, so
// waiting for d.done from that callback would deadlock; external callers must
// use Close instead so that their shutdown joins the reader.
func (d *SerialProtocolDispatcher) CloseFromCallback(ctx context.Context) error {
	if ctx == nil {
		return &DispatcherError{Reason: "serial protocol callback context is nil"}
	}
	token, ok := ctx.Value(dispatcherCallbackContextKey{}).(*dispatcherCallbackToken)
	if !ok || token.dispatcher != d {
		return &DispatcherError{Reason: "serial protocol callback context is invalid"}
	}
	d.mutex.Lock()
	active := token.active
	d.mutex.Unlock()
	if !active {
		return &DispatcherError{Reason: "serial protocol callback context is no longer active"}
	}
	return d.close(true)
}

func (d *SerialProtocolDispatcher) close(selfCallback bool) error {
	d.mutex.Lock()
	if !d.closing {
		d.closing = true
		if len(d.writes) != 0 {
			d.recordWriteTerminalLocked(d.writes[0], "write_cancelled", "")
		}
		if d.failure == nil {
			d.failure = &SerialError{Reason: "serial protocol dispatcher is closed"}
		}
		if d.cancel != nil {
			d.cancel()
		}
		d.signalLocked()
	}
	started := d.started
	d.mutex.Unlock()
	_ = d.serial.Close()
	if selfCallback {
		// A host callback runs on the reader goroutine.  In addition to the
		// reader join below, flushing an event recorder here could re-enter
		// Close from that same callback and deadlock.
		return nil
	}
	if !started {
		d.flushEvents()
		return nil
	}
	timer := time.NewTimer(d.closeTimeout)
	defer timer.Stop()
	select {
	case <-d.done:
		d.flushEvents()
		return nil
	case <-timer.C:
		return &SerialError{Reason: "serial protocol reader did not stop"}
	}
}

func (d *SerialProtocolDispatcher) readLoop(ctx context.Context) {
	defer func() {
		close(d.done)
		d.flushEvents()
	}()
	for {
		d.mutex.Lock()
		if d.closing {
			d.mutex.Unlock()
			return
		}
		var queued *queuedSerialWrite
		var outgoing []byte
		if len(d.writes) != 0 {
			queued = d.writes[0]
			if queued.stallDeadline.IsZero() {
				queued.stallDeadline = time.Now().Add(d.writeStall)
				d.recordWriteEventLocked(queued, "write_started", "")
			}
			end := min(len(queued.data), queued.offset+d.writeChunk)
			outgoing = queued.data[queued.offset:end]
		}
		d.mutex.Unlock()
		d.flushEvents()
		result, err := d.serial.Pump(ctx, dispatcherReadBytes, outgoing, d.pollInterval)
		if err != nil {
			d.fail(err)
			return
		}
		if result.Incoming != nil {
			if err := d.consumeIncoming(ctx, result.Incoming); err != nil {
				d.fail(err)
				return
			}
		}
		if result.Sent < 0 || result.Sent > len(outgoing) {
			d.fail(&DispatcherError{Reason: "serial transport reported an invalid write count"})
			return
		}
		if queued != nil {
			if err := d.advanceWrite(queued, result.Sent); err != nil {
				d.fail(err)
				return
			}
		}
	}
}

func (d *SerialProtocolDispatcher) consumeIncoming(ctx context.Context, incoming []byte) error {
	d.mutex.Lock()
	ready := d.ready
	if ready {
		d.frameBuffer = append(d.frameBuffer, incoming...)
	} else {
		d.startupBuffer = append(d.startupBuffer, incoming...)
	}
	d.mutex.Unlock()
	if ready {
		return d.consumeFrames(ctx)
	}
	return d.consumeStartup(ctx)
}

func (d *SerialProtocolDispatcher) consumeStartup(ctx context.Context) error {
	for {
		d.mutex.Lock()
		if len(d.startupBuffer) == 0 {
			d.mutex.Unlock()
			return nil
		}
		if bytes.HasPrefix(d.startupBuffer, []byte(ReadyMarker)) {
			d.startupBuffer = d.startupBuffer[len(ReadyMarker):]
			d.ready = true
			d.frameBuffer = append(d.frameBuffer, d.startupBuffer...)
			d.startupBuffer = nil
			d.signalLocked()
			d.mutex.Unlock()
			return d.consumeFrames(ctx)
		}
		startsMagic := bytes.HasPrefix(d.startupBuffer, frameMagic[:])
		partialReady := bytes.HasPrefix([]byte(ReadyMarker), d.startupBuffer)
		partialMagic := bytes.HasPrefix(frameMagic[:], d.startupBuffer)
		if startsMagic && d.options.StartupHostServices != nil {
			frame, complete, err := ExtractFrame(&d.startupBuffer)
			d.mutex.Unlock()
			if err != nil {
				return err
			}
			if !complete {
				return nil
			}
			if frame.MessageType != HostServiceRequest {
				return &FramingError{Reason: "received a non-host-service frame before CODEXOS-SEED-READY"}
			}
			if err := d.dispatchHostService(ctx, frame, d.options.StartupHostServices, "startup"); err != nil {
				return err
			}
			continue
		}
		if partialReady || (d.options.StartupHostServices != nil && partialMagic) {
			d.mutex.Unlock()
			return nil
		}
		if len(d.diagnostic) < maxStartupDiagnosticBytes {
			d.diagnostic = append(d.diagnostic, d.startupBuffer[0])
		}
		d.startupBuffer = d.startupBuffer[1:]
		d.mutex.Unlock()
	}
}

func (d *SerialProtocolDispatcher) consumeFrames(ctx context.Context) error {
	for {
		d.mutex.Lock()
		frame, complete, err := ExtractFrame(&d.frameBuffer)
		if err != nil {
			d.mutex.Unlock()
			return err
		}
		if !complete {
			d.mutex.Unlock()
			return nil
		}
		if frame.MessageType == HostServiceRequest {
			exchange := d.pendingResponse
			handler, scope := d.options.BackgroundHostServices, "background"
			if exchange {
				handler, scope = d.options.ExchangeHostServices, "exchange"
			}
			d.mutex.Unlock()
			if err := d.dispatchHostService(ctx, frame, handler, scope); err != nil {
				return err
			}
			continue
		}
		if !d.pendingResponse || d.response != nil {
			d.mutex.Unlock()
			return &FramingError{Reason: "received an unexpected guest tool-protocol response"}
		}
		d.pendingResponse = false
		copyFrame := frame
		d.response = &copyFrame
		d.recordLocked("serial_tool_response_received", map[string]any{"request_id": frame.RequestID, "response_bytes": len(frame.Payload)})
		d.signalLocked()
		d.mutex.Unlock()
		d.flushEvents()
	}
}

func (d *SerialProtocolDispatcher) dispatchHostService(ctx context.Context, frame Frame, handler HostServiceHandler, scope string) (err error) {
	queued := false
	d.mutex.Lock()
	d.hostActive++
	d.recordLocked("serial_host_service_request_received", map[string]any{"request_id": frame.RequestID, "request_bytes": len(frame.Payload), "scope": scope})
	d.signalLocked()
	d.mutex.Unlock()
	d.flushEvents()
	defer func() {
		if !queued {
			d.finishHostService()
		}
	}()
	request, decodeErr := DecodeHostServiceRequest(frame)
	var response Frame
	if decodeErr != nil {
		if frame.RequestID == 0 {
			return decodeErr
		}
		response, err = CreateHostServiceResponse(frame.RequestID, 1, boundedHostDiagnostic(decodeErr.Error()))
	} else if handler == nil {
		response, err = CreateHostServiceResponse(request.RequestID, 1, []byte("host services are not available"))
	} else {
		response, err = d.callHostServiceHandler(ctx, handler, request)
		if err != nil {
			response, err = CreateHostServiceResponse(request.RequestID, 2, boundedHostDiagnostic("trusted host-service failure: "+err.Error()))
		}
	}
	if err != nil {
		return err
	}
	encoded, err := EncodeFrame(response)
	if err != nil {
		return err
	}
	d.record("serial_host_service_response_prepared", map[string]any{"request_id": response.RequestID, "response_bytes": len(encoded), "scope": scope})
	if _, err := d.queueWrite(encoded, "host_response", response.RequestID, scope); err != nil {
		return err
	}
	queued = true
	return nil
}

func (d *SerialProtocolDispatcher) queueWrite(data []byte, kind string, requestID uint32, scope string) (*queuedSerialWrite, error) {
	d.mutex.Lock()
	if d.failure != nil {
		err := d.failure
		d.mutex.Unlock()
		return nil, err
	}
	if d.closing {
		d.mutex.Unlock()
		return nil, &SerialError{Reason: "serial protocol dispatcher is closed"}
	}
	if len(d.writes) >= dispatcherMaxQueuedWrites || len(data) > dispatcherMaxQueuedBytes-d.queuedBytes {
		d.mutex.Unlock()
		return nil, &DispatcherError{Reason: "serial protocol write queue is full"}
	}
	queued := &queuedSerialWrite{data: data, kind: kind, requestID: requestID, scope: scope, nextProgress: dispatcherWriteProgress}
	d.writes = append(d.writes, queued)
	d.queuedBytes += len(data)
	d.signalLocked()
	d.mutex.Unlock()
	d.flushEvents()
	return queued, nil
}

func (d *SerialProtocolDispatcher) waitForWrite(ctx context.Context, queued *queuedSerialWrite) error {
	for {
		d.mutex.Lock()
		if d.failure != nil {
			err := d.failure
			d.mutex.Unlock()
			return err
		}
		if queued.complete {
			d.mutex.Unlock()
			return nil
		}
		notify := d.notify
		d.mutex.Unlock()
		select {
		case <-notify:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (d *SerialProtocolDispatcher) advanceWrite(queued *queuedSerialWrite, sent int) error {
	d.mutex.Lock()
	defer func() { d.mutex.Unlock(); d.flushEvents() }()
	if len(d.writes) == 0 || d.writes[0] != queued {
		return nil
	}
	now := time.Now()
	if sent != 0 {
		queued.offset += sent
		queued.stallDeadline = now.Add(d.writeStall)
		if queued.kind == "host_response" {
			for queued.offset >= queued.nextProgress {
				d.recordWriteEventLocked(queued, "write_progress", "")
				queued.nextProgress += dispatcherWriteProgress
			}
		}
	} else if !queued.stallDeadline.IsZero() && !now.Before(queued.stallDeadline) {
		d.recordWriteTerminalLocked(queued, "write_timed_out", "")
		return &DispatcherError{Reason: "timed out writing serial protocol frame"}
	}
	if queued.offset != len(queued.data) {
		return nil
	}
	d.writes = d.writes[1:]
	d.queuedBytes -= len(queued.data)
	queued.complete = true
	d.recordWriteTerminalLocked(queued, "write_completed", "")
	if queued.kind == "host_response" {
		d.finishHostServiceLocked()
	}
	d.signalLocked()
	return nil
}

func (d *SerialProtocolDispatcher) fail(err error) {
	d.mutex.Lock()
	defer func() { d.mutex.Unlock(); d.flushEvents() }()
	if d.closing {
		return
	}
	if len(d.writes) != 0 {
		d.recordWriteTerminalLocked(d.writes[0], "write_failed", fmt.Sprintf("%T", err))
	}
	if d.hostActive != 0 {
		d.hostWindow += uint64(d.hostActive)
		d.hostActive = 0
	}
	if d.failure == nil {
		d.failure = err
	}
	d.signalLocked()
}

func (d *SerialProtocolDispatcher) abortExchange(err error) {
	d.mutex.Lock()
	if d.failure == nil {
		d.failure = err
	}
	if len(d.writes) != 0 {
		d.recordWriteTerminalLocked(d.writes[0], "write_cancelled", "")
	}
	if d.cancel != nil {
		d.cancel()
	}
	d.signalLocked()
	d.mutex.Unlock()
	_ = d.serial.Close()
	d.flushEvents()
}

func (d *SerialProtocolDispatcher) finishHostService() {
	d.mutex.Lock()
	defer d.mutex.Unlock()
	d.finishHostServiceLocked()
}
func (d *SerialProtocolDispatcher) finishHostServiceLocked() {
	if d.hostActive > 0 {
		d.hostActive--
		d.hostWindow++
		d.signalLocked()
	}
}
func (d *SerialProtocolDispatcher) signalLocked() { close(d.notify); d.notify = make(chan struct{}) }

func (d *SerialProtocolDispatcher) record(event string, data map[string]any) {
	d.mutex.Lock()
	d.recordLocked(event, data)
	d.mutex.Unlock()
	d.flushEvents()
}
func (d *SerialProtocolDispatcher) recordLocked(event string, data map[string]any) {
	if d.options.EventRecorder == nil {
		return
	}
	if len(d.events) < 64 {
		d.events = append(d.events, serialEvent{name: event, data: data})
	}
}
func (d *SerialProtocolDispatcher) recordWriteEventLocked(q *queuedSerialWrite, phase, errorName string) {
	data := map[string]any{"request_id": q.requestID, "phase": phase, "bytes_sent": q.offset, "total_bytes": len(q.data), "write_kind": q.kind}
	if q.scope != "" {
		data["scope"] = q.scope
	}
	if errorName != "" {
		data["error"] = errorName
	}
	d.recordLocked("serial_protocol_write", data)
}
func (d *SerialProtocolDispatcher) recordWriteTerminalLocked(q *queuedSerialWrite, phase, errorName string) {
	if q.terminal == "" {
		q.terminal = phase
		d.recordWriteEventLocked(q, phase, errorName)
	}
}
func boundedHostDiagnostic(message string) []byte {
	data := []byte(message)
	return data[:min(len(data), maxHostDiagnosticBytes)]
}

func (d *SerialProtocolDispatcher) callHostServiceHandler(ctx context.Context, handler HostServiceHandler, request HostRequest) (response Frame, err error) {
	d.mutex.Lock()
	d.callbackActive++
	d.mutex.Unlock()
	token := &dispatcherCallbackToken{dispatcher: d, active: true}
	defer func() {
		d.mutex.Lock()
		token.active = false
		d.callbackActive--
		d.mutex.Unlock()
	}()
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("panic: %v", recovered)
		}
	}()
	callbackContext := context.WithValue(ctx, dispatcherCallbackContextKey{}, token)
	return handler(callbackContext, request)
}

func (d *SerialProtocolDispatcher) flushEvents() {
	if d.options.EventRecorder == nil {
		return
	}
	d.mutex.Lock()
	if d.flushingEvents {
		d.mutex.Unlock()
		return
	}
	d.flushingEvents = true
	d.mutex.Unlock()
	for {
		d.mutex.Lock()
		if len(d.events) == 0 {
			d.flushingEvents = false
			d.mutex.Unlock()
			return
		}
		events := append([]serialEvent(nil), d.events...)
		d.events = d.events[:0]
		d.callbackActive++
		d.mutex.Unlock()
		for _, event := range events {
			func() {
				defer func() { _ = recover() }()
				d.options.EventRecorder(event.name, event.data)
			}()
		}
		d.mutex.Lock()
		d.callbackActive--
		d.mutex.Unlock()
	}
}
