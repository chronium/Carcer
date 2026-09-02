package qemu

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const testQMPGreeting = `{"QMP":{"version":{},"capabilities":[]}}` + "\r\n"

func TestQMPClientLifecycle(t *testing.T) {
	path, wait := startQMPPeer(t, func(connection net.Conn) error {
		if _, err := connection.Write([]byte(testQMPGreeting[:13])); err != nil {
			return err
		}
		if _, err := connection.Write([]byte(testQMPGreeting[13:])); err != nil {
			return err
		}

		reader := bufio.NewReader(connection)
		requests := []string{
			`{"execute":"qmp_capabilities","id":1}` + "\r\n",
			`{"execute":"query-status","id":2}` + "\r\n",
			`{"execute":"stop","id":3}` + "\r\n",
			`{"execute":"cont","id":4}` + "\r\n",
			`{"execute":"quit","id":5}` + "\r\n",
		}
		responses := []string{
			`{"event":"RESET"}` + "\r\n" + `{"return":{},"id":1}` + "\r\n",
			`{"event":"STOP"}` + "\r\n" + `{"return":{"status":"running"},"id":2}` + "\r\n",
			`{"return":{},"id":3}` + "\r\n",
			`{"return":{},"id":4}` + "\r\n",
			`{"return":{},"id":5}` + "\r\n",
		}
		for index, expected := range requests {
			actual, err := reader.ReadString('\n')
			if err != nil {
				return err
			}
			if actual != expected {
				return fmt.Errorf("request %d = %q, want %q", index+1, actual, expected)
			}
			if _, err := connection.Write([]byte(responses[index])); err != nil {
				return err
			}
		}
		return nil
	})

	client := NewQMPClient(path)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := client.Connect(ctx); err == nil || !strings.Contains(err.Error(), "already connected") {
		t.Fatalf("duplicate Connect error = %v, want already connected", err)
	}
	status, err := client.QueryStatus(ctx)
	if err != nil {
		t.Fatalf("QueryStatus: %v", err)
	}
	if status != "running" {
		t.Fatalf("QueryStatus = %q, want running", status)
	}
	if err := client.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := client.Continue(ctx); err != nil {
		t.Fatalf("Continue: %v", err)
	}
	if err := client.Quit(ctx); err != nil {
		t.Fatalf("Quit: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	wait()
}

func TestQMPClientRetriesUntilSocketAppears(t *testing.T) {
	path := filepath.Join(t.TempDir(), "qmp.sock")
	done := make(chan error, 1)
	go func() {
		time.Sleep(25 * time.Millisecond)
		listener, err := net.Listen("unix", path)
		if err != nil {
			done <- err
			return
		}
		defer listener.Close()
		if err := listener.(*net.UnixListener).SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
			done <- err
			return
		}
		connection, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer connection.Close()
		if err := connection.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
			done <- err
			return
		}
		reader := bufio.NewReader(connection)
		if _, err := connection.Write([]byte(testQMPGreeting)); err != nil {
			done <- err
			return
		}
		request, err := reader.ReadString('\n')
		if err != nil {
			done <- err
			return
		}
		if request != `{"execute":"qmp_capabilities","id":1}`+"\r\n" {
			done <- fmt.Errorf("capabilities request = %q", request)
			return
		}
		_, err = connection.Write([]byte(`{"return":{},"id":1}` + "\r\n"))
		done <- err
	}()

	client := NewQMPClient(path)
	client.timeout = 500 * time.Millisecond
	if err := client.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("QMP peer did not stop")
	}
}

func TestQMPClientConnectHonorsContext(t *testing.T) {
	client := NewQMPClient(filepath.Join(t.TempDir(), "missing.sock"))
	client.timeout = time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	err := client.Connect(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Connect error = %v, want context deadline exceeded", err)
	}
}

func TestQMPClientReportsItsConnectTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.sock")
	client := NewQMPClient(path)
	client.timeout = 20 * time.Millisecond

	err := client.Connect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "timed out connecting to QMP socket "+path) {
		t.Fatalf("Connect error = %v, want QMP connect timeout", err)
	}
}

func TestQMPClientCancellationInterruptsActiveRead(t *testing.T) {
	requestReceived := make(chan struct{})
	releasePeer := make(chan struct{})
	path, wait := startQMPPeer(t, func(connection net.Conn) error {
		reader := bufio.NewReader(connection)
		if _, err := connection.Write([]byte(testQMPGreeting)); err != nil {
			return err
		}
		if _, err := reader.ReadString('\n'); err != nil {
			return err
		}
		if _, err := connection.Write([]byte(`{"return":{},"id":1}` + "\r\n")); err != nil {
			return err
		}
		if _, err := reader.ReadString('\n'); err != nil {
			return err
		}
		close(requestReceived)
		<-releasePeer
		return nil
	})
	client := NewQMPClient(path)
	client.timeout = 2 * time.Second
	if err := client.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := client.QueryStatus(ctx)
		result <- err
	}()
	select {
	case <-requestReceived:
	case <-time.After(time.Second):
		t.Fatal("peer did not receive query-status")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("QueryStatus error = %v, want context canceled", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("context cancellation did not interrupt the active read")
	}
	if client.connection != nil {
		t.Fatal("canceled request left a potentially desynchronized connection open")
	}
	close(releasePeer)
	if err := client.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	wait()
}

func TestQMPClientRejectsInvalidGreetingsAndCloses(t *testing.T) {
	tests := []struct {
		name     string
		greeting string
		want     string
	}{
		{name: "invalid JSON", greeting: "{\r\n", want: "QMP returned invalid JSON"},
		{name: "non-object", greeting: "[]\r\n", want: "QMP returned a non-object message"},
		{name: "missing version", greeting: `{"QMP":{"capabilities":[]}}` + "\r\n", want: "invalid QMP greeting"},
		{name: "invalid UTF-8", greeting: string([]byte{'{', 0xff, '}', '\n'}), want: "QMP returned invalid JSON"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path, wait := startQMPPeer(t, func(connection net.Conn) error {
				_, err := connection.Write([]byte(test.greeting))
				return err
			})
			client := NewQMPClient(path)
			err := client.Connect(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Connect error = %v, want containing %q", err, test.want)
			}
			if client.connection != nil {
				t.Fatal("failed Connect left the client connected")
			}
			if err := client.Close(); err != nil {
				t.Fatalf("Close after failed Connect: %v", err)
			}
			wait()
		})
	}
}

func TestQMPClientRejectsOversizedMessage(t *testing.T) {
	path, wait := startQMPPeer(t, func(connection net.Conn) error {
		message := strings.Repeat(" ", maxQMPMessageSize) + "\n"
		_, err := connection.Write([]byte(message))
		return err
	})
	client := NewQMPClient(path)
	err := client.Connect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "QMP message exceeds 1 MiB") {
		t.Fatalf("Connect error = %v, want message size error", err)
	}
	wait()
}

func TestQMPClientResponseValidation(t *testing.T) {
	tests := []struct {
		name     string
		response string
		want     string
	}{
		{name: "unexpected id", response: `{"return":{},"id":99}`, want: "unexpected id 99"},
		{name: "fractional id", response: `{"return":{},"id":2.0}`, want: "unexpected id 2.0"},
		{name: "invalid error", response: `{"error":"bad","id":2}`, want: "invalid error response"},
		{name: "command error", response: `{"error":{"class":"GenericError","desc":"not allowed"},"id":2}`, want: "query-status failed: GenericError: not allowed"},
		{name: "default command error", response: `{"error":{},"id":2}`, want: "query-status failed: unknown error: no description"},
		{name: "missing result", response: `{"id":2}`, want: "query-status returned no result"},
		{name: "invalid status type", response: `{"return":{"status":7},"id":2}`, want: "query-status returned an invalid response"},
		{name: "trailing JSON", response: `{"return":{},"id":2} {}`, want: "QMP returned invalid JSON"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path, wait := startQMPPeer(t, func(connection net.Conn) error {
				reader := bufio.NewReader(connection)
				if _, err := connection.Write([]byte(testQMPGreeting)); err != nil {
					return err
				}
				if _, err := reader.ReadString('\n'); err != nil {
					return err
				}
				if _, err := connection.Write([]byte(`{"return":{},"id":1}` + "\r\n")); err != nil {
					return err
				}
				if _, err := reader.ReadString('\n'); err != nil {
					return err
				}
				_, err := connection.Write([]byte(test.response + "\r\n"))
				return err
			})
			client := NewQMPClient(path)
			if err := client.Connect(context.Background()); err != nil {
				t.Fatalf("Connect: %v", err)
			}
			_, err := client.QueryStatus(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("QueryStatus error = %v, want containing %q", err, test.want)
			}
			protocolFailure := test.name != "command error" && test.name != "default command error" && test.name != "invalid status type"
			if protocolFailure && client.connection != nil {
				t.Fatal("protocol failure left the connection open")
			}
			if !protocolFailure && client.connection == nil {
				t.Fatal("complete response unexpectedly closed the connection")
			}
			if err := client.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			wait()
		})
	}
}

func TestQMPClientRequiresConnection(t *testing.T) {
	client := NewQMPClient("unused")
	if _, err := client.QueryStatus(context.Background()); err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("QueryStatus error = %v, want not connected", err)
	}
	if err := client.Stop(context.Background()); err == nil || !strings.Contains(err.Error(), "not connected") {
		t.Fatalf("Stop error = %v, want not connected", err)
	}
}

func startQMPPeer(t *testing.T, serve func(net.Conn) error) (string, func()) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "qmp.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer connection.Close()
		if err := connection.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
			done <- err
			return
		}
		done <- serve(connection)
	}()

	var once sync.Once
	wait := func() {
		t.Helper()
		once.Do(func() {
			if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				t.Errorf("close QMP listener: %v", err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("QMP peer: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Error("QMP peer did not stop")
			}
		})
	}
	t.Cleanup(wait)
	return path, wait
}
