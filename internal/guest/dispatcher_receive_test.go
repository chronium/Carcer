package guest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestDispatcherSlowLargeIncomingFrame(t *testing.T) {
	for _, nested := range []bool{false, true} {
		name := "tool_response"
		if nested {
			name = "nested_host_request"
		}
		t.Run(name, func(t *testing.T) {
			serial, peer, wait := connectedSerial(t)
			defer peer.Close()
			payload := bytes.Repeat([]byte{0, 'C', 'X', 'O', 'S', 255, 7}, 715493/7+1)[:715493]
			d := NewSerialProtocolDispatcher(serial, DispatcherOptions{
				ExchangeHostServices: func(_ context.Context, r HostRequest) (Frame, error) {
					if len(r.Arguments) != 1 || !bytes.Equal(r.Arguments[0], payload) {
						t.Error("host request payload corrupted")
					}
					time.Sleep(200 * time.Millisecond) // Service execution has its own deadline.
					return CreateHostServiceResponse(r.RequestID, 0, nil)
				},
			})
			if err := d.StartReady(context.Background()); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = d.Close() })
			result := make(chan error, 1)
			go func() {
				frame, err := d.Exchange(context.Background(), Frame{MessageType: InvokeToolRequest, RequestID: 9}, 150*time.Millisecond)
				if err == nil && !bytes.Equal(frame.Payload, payload) {
					err = errors.New("tool response payload corrupted")
				}
				result <- err
			}()
			_ = readPeerFrame(t, peer)
			response := Frame{MessageType: InvokeToolResponse, RequestID: 9, Payload: payload}
			incoming := response
			if nested {
				incoming = hostRequestFrame(9, "build", payload)
			} // Independent ID namespace.
			wire, err := EncodeFrame(incoming)
			if err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			// Fragment the header too; the total transfer exceeds the idle timeout.
			for offset := 0; offset < len(wire); {
				end := min(offset+32768, len(wire))
				if offset < frameHeaderSize {
					end = offset + 1
				}
				if _, err := peer.Write(wire[offset:end]); err != nil {
					t.Fatal(err)
				}
				offset = end
				time.Sleep(10 * time.Millisecond)
			}
			if nested {
				host := readPeerFrame(t, peer)
				if host.MessageType != HostServiceResponse || host.RequestID != 9 {
					t.Fatalf("host response: %#v", host)
				}
				writePeerFrame(t, peer, response)
			}
			select {
			case err := <-result:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("healthy transfer did not finish")
			}
			if time.Since(start) <= 150*time.Millisecond {
				t.Fatal("test did not exceed old response deadline")
			}
			go func() {
				_, err := d.Exchange(context.Background(), Frame{MessageType: ListToolsRequest, RequestID: 10}, time.Second)
				result <- err
			}()
			_ = readPeerFrame(t, peer)
			writePeerFrame(t, peer, Frame{MessageType: ListToolsResponse, RequestID: 10, Payload: []byte{0, 0}})
			if err := <-result; err != nil {
				t.Fatalf("next exchange: %v", err)
			}
			_ = d.Close()
			wait()
		})
	}
}

func TestDispatcherIncompleteReceiveRetiresConnection(t *testing.T) {
	for _, mode := range []string{"silent", "partial_header", "truncated_payload", "cancel", "transfer_limit"} {
		t.Run(mode, func(t *testing.T) {
			serial, peer, wait := connectedSerial(t)
			defer peer.Close()
			progress := make(chan struct{}, 1)
			d := NewSerialProtocolDispatcher(serial, DispatcherOptions{EventRecorder: func(name string, _ map[string]any) {
				if name == "serial_protocol_receive_progress" {
					select {
					case progress <- struct{}{}:
					default:
					}
				}
			}})
			d.receiveLimit = 250 * time.Millisecond
			if err := d.StartReady(context.Background()); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = d.Close() })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				_, err := d.Exchange(ctx, Frame{MessageType: InvokeToolRequest, RequestID: 4}, 100*time.Millisecond)
				result <- err
			}()
			_ = readPeerFrame(t, peer)
			wire, err := EncodeFrame(Frame{MessageType: InvokeToolResponse, RequestID: 4, Payload: make([]byte, 715497)})
			if err != nil {
				t.Fatal(err)
			}
			count := frameHeaderSize + 100
			if mode == "partial_header" {
				count = 7
			}
			if mode != "silent" {
				if _, err := peer.Write(wire[:count]); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "cancel" {
				select {
				case <-progress:
				case <-time.After(time.Second):
					t.Fatal("partial response was not received before cancellation")
				}
				cancel()
			}
			if mode == "transfer_limit" {
				// Keep receiving well inside the idle limit; the absolute limit
				// must still close the stream even though bytes continue to arrive.
				done := make(chan struct{})
				go func() {
					defer close(done)
					for i := 0; i < 100; i++ {
						time.Sleep(20 * time.Millisecond)
						if _, err := peer.Write([]byte{0}); err != nil {
							return
						}
					}
				}()
				t.Cleanup(func() { _ = peer.Close(); <-done })
			}
			var failure error
			select {
			case failure = <-result:
			case <-time.After(time.Second):
				t.Fatal("incomplete transfer did not terminate")
			}
			if mode == "cancel" {
				if !errors.Is(failure, context.Canceled) {
					t.Fatalf("cancellation: %v", failure)
				}
			} else {
				want := "timed out waiting for tool response"
				if mode == "transfer_limit" {
					want = "transfer deadline"
				}
				if failure == nil || !strings.Contains(failure.Error(), want) {
					t.Fatalf("failure: %v, want %s", failure, want)
				}
			}
			// Neither a late response nor a later request may reuse this stream.
			_, second := d.Exchange(context.Background(), Frame{MessageType: ListToolsRequest, RequestID: 5}, time.Second)
			if second != failure {
				t.Fatalf("terminal error changed: %v -> %v", failure, second)
			}
			_ = peer.SetReadDeadline(time.Now().Add(time.Second))
			var b [1]byte
			if n, err := peer.Read(b[:]); n != 0 || !errors.Is(err, io.EOF) {
				t.Fatalf("retired stream: n=%d err=%v", n, err)
			}
			_ = d.Close()
			wait()
		})
	}
}

func TestDispatcherRejectsStaleHeaderBeforePayload(t *testing.T) {
	for _, wrongType := range []bool{false, true} {
		name := "request_id"
		if wrongType {
			name = "message_type"
		}
		t.Run(name, func(t *testing.T) {
			serial, peer, wait := connectedSerial(t)
			defer peer.Close()
			d := NewSerialProtocolDispatcher(serial, DispatcherOptions{})
			if err := d.StartReady(context.Background()); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = d.Close() })
			result := make(chan error, 1)
			go func() {
				_, err := d.Exchange(context.Background(), Frame{MessageType: InvokeToolRequest, RequestID: 7}, 5*time.Second)
				result <- err
			}()
			_ = readPeerFrame(t, peer)
			stale := Frame{MessageType: InvokeToolResponse, RequestID: 6, Payload: make([]byte, 715497)}
			want := "request ID"
			if wrongType {
				stale.RequestID = 7
				stale.MessageType = ListToolsResponse
				want = "message type"
			}
			wire, err := EncodeFrame(stale)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := peer.Write(wire[:frameHeaderSize]); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-result:
				if err == nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("stale response: %v", err)
				}
				_, later := d.Exchange(context.Background(), Frame{MessageType: ListToolsRequest, RequestID: 8}, time.Second)
				if later != err {
					t.Fatalf("stale response did not retire dispatcher: %v", later)
				}
			case <-time.After(time.Second):
				t.Fatal("waited for stale payload")
			}
			_ = d.Close()
			wait()
		})
	}
}
