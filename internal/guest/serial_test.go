package guest

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSerialConnectionRawBytesTimeoutAndClosure(t *testing.T) {
	path, peer, wait := startSerialPeer(t)
	serial := NewSerialConnection(path)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := serial.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := serial.Connect(ctx); err == nil || !strings.Contains(err.Error(), "already connected") {
		t.Fatalf("duplicate Connect error = %v", err)
	}

	outgoing := []byte("\x00host to guest\xff")
	if err := serial.Write(ctx, outgoing); err != nil {
		t.Fatalf("Write: %v", err)
	}
	readOutgoing := make([]byte, len(outgoing))
	if _, err := io.ReadFull(peer, readOutgoing); err != nil {
		t.Fatal(err)
	}
	if string(readOutgoing) != string(outgoing) {
		t.Fatalf("peer read %q, want %q", readOutgoing, outgoing)
	}

	if _, err := serial.Read(ctx, 1, 10*time.Millisecond); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Read timeout error = %v, want deadline exceeded", err)
	}
	incoming := []byte("\xfeguest to host\x00")
	if _, err := peer.Write(incoming); err != nil {
		t.Fatal(err)
	}
	var received []byte
	for len(received) < len(incoming) {
		part, err := serial.Read(ctx, 2, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		received = append(received, part...)
	}
	if string(received) != string(incoming) {
		t.Fatalf("Read = %q, want %q", received, incoming)
	}

	if err := peer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := serial.Read(ctx, 1, time.Second); err == nil || !strings.Contains(err.Error(), "serial connection closed") {
		t.Fatalf("Read after peer close error = %v", err)
	}
	if _, err := serial.Read(ctx, 1, time.Second); err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("Read after closure error = %v", err)
	}
	if err := serial.Close(); err != nil {
		t.Fatal(err)
	}
	wait()
}

func TestSerialConnectionRetriesUntilSocketAppears(t *testing.T) {
	path := filepath.Join(t.TempDir(), "serial.sock")
	done := make(chan error, 1)
	go func() {
		time.Sleep(25 * time.Millisecond)
		listener, err := net.Listen("unix", path)
		if err != nil {
			done <- err
			return
		}
		defer listener.Close()
		_ = listener.(*net.UnixListener).SetDeadline(time.Now().Add(2 * time.Second))
		connection, err := listener.Accept()
		if err == nil {
			err = connection.Close()
		}
		done <- err
	}()
	serial := NewSerialConnection(path)
	serial.connectTimeout = 500 * time.Millisecond
	if err := serial.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := serial.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serial peer did not stop")
	}
}

func TestSerialConnectionConcurrentConnectInstallsOneConnection(t *testing.T) {
	path, peer, _ := startSerialPeer(t)
	defer peer.Close()
	serial := NewSerialConnection(path)
	results := make(chan error, 2)
	start := make(chan struct{})
	for range 2 {
		go func() {
			<-start
			results <- serial.Connect(context.Background())
		}()
	}
	close(start)
	var succeeded, rejected int
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			succeeded++
		case strings.Contains(err.Error(), "already connected"):
			rejected++
		default:
			t.Fatalf("Connect error = %v", err)
		}
	}
	if succeeded != 1 || rejected != 1 {
		t.Fatalf("Connect results: %d succeeded, %d rejected", succeeded, rejected)
	}
	if err := serial.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSerialConnectionCancellationInterruptsReadAndCloses(t *testing.T) {
	path, peer, wait := startSerialPeer(t)
	defer peer.Close()
	serial := NewSerialConnection(path)
	if err := serial.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := serial.Read(ctx, 1, 2*time.Second)
		result <- err
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Read error = %v, want context canceled", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("cancellation did not interrupt Read")
	}
	if serial.isConnected() {
		t.Fatal("canceled Read left the connection open")
	}
	wait()
}

func TestSerialConnectionValidationAndConnectTimeout(t *testing.T) {
	serial := NewSerialConnection(filepath.Join(t.TempDir(), "missing.sock"))
	if err := serial.Connect(nil); err == nil || !strings.Contains(err.Error(), "context is nil") {
		t.Fatalf("Connect error = %v", err)
	}
	serial.connectTimeout = 20 * time.Millisecond
	if err := serial.Connect(context.Background()); err == nil || !strings.Contains(err.Error(), "timed out connecting") {
		t.Fatalf("Connect timeout error = %v", err)
	}
	if _, err := serial.Read(context.Background(), 0, time.Second); err == nil || !strings.Contains(err.Error(), "maxBytes") {
		t.Fatalf("Read size error = %v", err)
	}
	if err := serial.Write(context.Background(), []byte("x")); err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("Write error = %v", err)
	}
}

func TestSerialPumpReadsBeforeWriting(t *testing.T) {
	path, peer, wait := startSerialPeer(t)
	defer peer.Close()
	serial := NewSerialConnection(path)
	if err := serial.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := peer.Write([]byte("guest-first")); err != nil {
		t.Fatal(err)
	}
	result, err := serial.Pump(context.Background(), 64, []byte("host-second"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if string(result.Incoming) != "guest-first" || result.Sent != len("host-second") {
		t.Fatalf("Pump = %#v", result)
	}
	outgoing := make([]byte, result.Sent)
	if _, err := io.ReadFull(peer, outgoing); err != nil {
		t.Fatal(err)
	}
	if string(outgoing) != "host-second" {
		t.Fatalf("peer received %q", outgoing)
	}
	_ = serial.Close()
	wait()
}

func TestSerialPumpIdleTimeoutAndCancellation(t *testing.T) {
	path, peer, wait := startSerialPeer(t)
	defer peer.Close()
	serial := NewSerialConnection(path)
	if err := serial.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	result, err := serial.Pump(context.Background(), 1, nil, 20*time.Millisecond)
	if err != nil || result.Incoming != nil || result.Sent != 0 {
		t.Fatalf("idle Pump = %#v, %v", result, err)
	}
	if elapsed := time.Since(started); elapsed < 15*time.Millisecond || elapsed > 500*time.Millisecond {
		t.Fatalf("idle Pump took %s", elapsed)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = serial.Pump(ctx, 1, nil, time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Pump error = %v", err)
	}
	if serial.isConnected() {
		t.Fatal("canceled Pump left the connection open")
	}
	wait()
}

func TestSerialPumpContextDeadlineWinsOverIdleTimeout(t *testing.T) {
	path, peer, wait := startSerialPeer(t)
	defer peer.Close()
	serial := NewSerialConnection(path)
	if err := serial.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := serial.Pump(ctx, 1, nil, time.Second)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Pump error = %v, want context deadline exceeded", err)
	}
	if serial.isConnected() {
		t.Fatal("deadline-expired Pump left the connection open")
	}
	wait()
}

func TestSerialPumpPerformsPartialNonblockingWrite(t *testing.T) {
	path, peer, wait := startSerialPeer(t)
	defer peer.Close()
	serial := NewSerialConnection(path)
	if err := serial.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	connection, err := serial.requireConnection()
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.(*net.UnixConn).SetWriteBuffer(1024); err != nil {
		t.Fatal(err)
	}
	outgoing := make([]byte, 1024*1024)
	result, err := serial.Pump(context.Background(), 1, outgoing, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if result.Sent <= 0 || result.Sent >= len(outgoing) {
		t.Fatalf("Pump sent %d of %d bytes; want one partial write", result.Sent, len(outgoing))
	}
	_ = serial.Close()
	// Resolve and close the accepted peer before closing the listener. Without
	// this join, cleanup can win the accept race and leave deferredConn holding
	// a nil connection.
	if err := peer.Close(); err != nil {
		t.Fatal(err)
	}
	wait()
}

func TestSerialPumpDetectsPeerClosureAndValidatesArguments(t *testing.T) {
	path, peer, wait := startSerialPeer(t)
	serial := NewSerialConnection(path)
	if err := serial.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := serial.Pump(context.Background(), 0, nil, time.Second); err == nil || !strings.Contains(err.Error(), "maxReadBytes") {
		t.Fatalf("Pump size error = %v", err)
	}
	if _, err := serial.Pump(context.Background(), 1, nil, -time.Second); err == nil || !strings.Contains(err.Error(), "must not be negative") {
		t.Fatalf("Pump timeout error = %v", err)
	}
	if err := peer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := serial.Pump(context.Background(), 1, nil, time.Second); err == nil || !strings.Contains(err.Error(), "serial connection closed") {
		t.Fatalf("Pump closure error = %v", err)
	}
	if serial.isConnected() {
		t.Fatal("peer closure left the connection open")
	}
	wait()
}

func TestEscapeDiagnosticBytes(t *testing.T) {
	input := []byte("safe\n\r\t\x1b[2J\x00\xff")
	if got, want := EscapeDiagnosticBytes(input), `safe\n\r\t\x1b[2J\x00\xff`; got != want {
		t.Fatalf("EscapeDiagnosticBytes = %q, want %q", got, want)
	}
	oversized := append(make([]byte, maxStartupDiagnosticBytes), 'x')
	got := EscapeDiagnosticBytes(oversized)
	if !strings.HasSuffix(got, "...[truncated]") || len(got) != maxStartupDiagnosticBytes*4+len("...[truncated]") {
		t.Fatalf("truncated diagnostic = %q", got[len(got)-40:])
	}
}

func startSerialPeer(t *testing.T) (string, net.Conn, func()) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "serial.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	peerReady := make(chan net.Conn, 1)
	go func() {
		connection, _ := listener.Accept()
		peerReady <- connection
	}()
	// SerialConnection connects after this helper returns. Expose a proxy peer
	// once accepted through a small blocking wrapper.
	proxy := &deferredConn{ready: peerReady}
	wait := func() { _ = listener.Close() }
	t.Cleanup(wait)
	return path, proxy, wait
}

type deferredConn struct {
	ready chan net.Conn
	conn  net.Conn
}

func (c *deferredConn) connection() net.Conn {
	if c.conn == nil {
		c.conn = <-c.ready
	}
	return c.conn
}

func (c *deferredConn) Read(value []byte) (int, error)    { return c.connection().Read(value) }
func (c *deferredConn) Write(value []byte) (int, error)   { return c.connection().Write(value) }
func (c *deferredConn) Close() error                      { return c.connection().Close() }
func (c *deferredConn) LocalAddr() net.Addr               { return c.connection().LocalAddr() }
func (c *deferredConn) RemoteAddr() net.Addr              { return c.connection().RemoteAddr() }
func (c *deferredConn) SetDeadline(value time.Time) error { return c.connection().SetDeadline(value) }
func (c *deferredConn) SetReadDeadline(value time.Time) error {
	return c.connection().SetReadDeadline(value)
}
func (c *deferredConn) SetWriteDeadline(value time.Time) error {
	return c.connection().SetWriteDeadline(value)
}
