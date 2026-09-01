package guest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
)

const serialConnectTimeout = 5 * time.Second

type SerialError struct {
	Reason string
	Err    error
}

func (e *SerialError) Error() string {
	if e.Err == nil {
		return e.Reason
	}
	return e.Reason + ": " + e.Err.Error()
}

func (e *SerialError) Unwrap() error { return e.Err }

// SerialConnection owns one raw QEMU serial Unix connection. A dispatcher may
// become its sole reader; the connection itself starts no read loop.
type SerialConnection struct {
	socketPath     string
	connectTimeout time.Duration
	connection     net.Conn
}

func NewSerialConnection(socketPath string) *SerialConnection {
	return &SerialConnection{socketPath: socketPath, connectTimeout: serialConnectTimeout}
}

func (c *SerialConnection) Connect(ctx context.Context) error {
	if ctx == nil {
		return &SerialError{Reason: "serial connect context is nil"}
	}
	if c.connection != nil {
		return &SerialError{Reason: "serial connection is already connected"}
	}
	deadline := time.Now().Add(c.connectTimeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		connection, err := (&net.Dialer{Deadline: deadline}).DialContext(ctx, "unix", c.socketPath)
		if err == nil {
			c.connection = connection
			return nil
		}
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return &SerialError{Reason: fmt.Sprintf("timed out connecting to serial socket %s", c.socketPath), Err: err}
		}
		if !retryableSerialConnectError(err) {
			return err
		}
		timer := time.NewTimer(min(10*time.Millisecond, remaining))
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (c *SerialConnection) Write(ctx context.Context, data []byte) error {
	connection, err := c.requireConnection()
	if err != nil {
		return err
	}
	if ctx == nil {
		return &SerialError{Reason: "serial write context is nil"}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	stopCancellation := interruptSerialOnCancel(ctx, connection)
	defer stopCancellation()
	if deadline, ok := ctx.Deadline(); ok {
		if err := connection.SetWriteDeadline(deadline); err != nil {
			return c.closeWithSerialError(&SerialError{Reason: "could not set serial write deadline", Err: err})
		}
	} else if err := connection.SetWriteDeadline(time.Time{}); err != nil {
		return c.closeWithSerialError(&SerialError{Reason: "could not clear serial write deadline", Err: err})
	}
	written := 0
	for written < len(data) {
		count, writeErr := connection.Write(data[written:])
		written += count
		if writeErr != nil {
			if contextErr := ctx.Err(); contextErr != nil {
				_ = c.Close()
				return contextErr
			}
			return c.closeWithSerialError(&SerialError{Reason: "serial connection closed", Err: writeErr})
		}
		if count == 0 {
			return c.closeWithSerialError(&SerialError{Reason: "serial connection closed"})
		}
	}
	return nil
}

func (c *SerialConnection) Read(ctx context.Context, maxBytes int, timeout time.Duration) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, &SerialError{Reason: "maxBytes must be positive"}
	}
	connection, err := c.requireConnection()
	if err != nil {
		return nil, err
	}
	if timeout < 0 {
		return nil, &SerialError{Reason: "serial read timeout must not be negative"}
	}
	if ctx == nil {
		return nil, &SerialError{Reason: "serial read context is nil"}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(timeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	if err := connection.SetReadDeadline(deadline); err != nil {
		return nil, c.closeWithSerialError(&SerialError{Reason: "could not set serial read deadline", Err: err})
	}
	stopCancellation := interruptSerialOnCancel(ctx, connection)
	buffer := make([]byte, maxBytes)
	count, readErr := connection.Read(buffer)
	stopCancellation()
	if readErr != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			_ = c.Close()
			return nil, contextErr
		}
		if errors.Is(readErr, os.ErrDeadlineExceeded) {
			return nil, readErr
		}
		return nil, c.closeWithSerialError(&SerialError{Reason: "serial connection closed", Err: readErr})
	}
	if count == 0 {
		return nil, c.closeWithSerialError(&SerialError{Reason: "serial connection closed"})
	}
	return buffer[:count], nil
}

func (c *SerialConnection) Close() error {
	connection := c.connection
	c.connection = nil
	if connection == nil {
		return nil
	}
	return connection.Close()
}

func (c *SerialConnection) requireConnection() (net.Conn, error) {
	if c.connection == nil {
		return nil, &SerialError{Reason: "serial connection is not connected"}
	}
	return c.connection, nil
}

func (c *SerialConnection) closeWithSerialError(err error) error {
	_ = c.Close()
	return err
}

func retryableSerialConnectError(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED)
}

func interruptSerialOnCancel(ctx context.Context, connection net.Conn) func() {
	finished := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = connection.SetDeadline(time.Now())
		close(finished)
	})
	return func() {
		if !stop() {
			<-finished
		}
	}
}
