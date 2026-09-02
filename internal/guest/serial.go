package guest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
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
	mutex          sync.Mutex
	lifecycle      sync.Mutex
	socketPath     string
	connectTimeout time.Duration
	connection     net.Conn
}

type SerialPumpResult struct {
	Incoming []byte
	Sent     int
}

func NewSerialConnection(socketPath string) *SerialConnection {
	return &SerialConnection{socketPath: socketPath, connectTimeout: serialConnectTimeout}
}

func (c *SerialConnection) Connect(ctx context.Context) error {
	if ctx == nil {
		return &SerialError{Reason: "serial connect context is nil"}
	}
	c.lifecycle.Lock()
	defer c.lifecycle.Unlock()
	c.mutex.Lock()
	connected := c.connection != nil
	c.mutex.Unlock()
	if connected {
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
			c.mutex.Lock()
			c.connection = connection
			c.mutex.Unlock()
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
			timer.Stop()
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

// Pump performs one bounded duplex I/O opportunity. Incoming data is always
// consumed before an outgoing write, preserving the dispatcher's read-first
// handling of nested host-service requests.
func (c *SerialConnection) Pump(ctx context.Context, maxReadBytes int, outgoing []byte, timeout time.Duration) (SerialPumpResult, error) {
	if maxReadBytes <= 0 {
		return SerialPumpResult{}, &SerialError{Reason: "maxReadBytes must be positive"}
	}
	if timeout < 0 {
		return SerialPumpResult{}, &SerialError{Reason: "serial pump timeout must not be negative"}
	}
	connection, err := c.requireConnection()
	if err != nil {
		return SerialPumpResult{}, err
	}
	if ctx == nil {
		return SerialPumpResult{}, &SerialError{Reason: "serial pump context is nil"}
	}
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return c.closePumpError(&SerialError{Reason: "serial connection is not a Unix socket"})
	}
	raw, err := unixConnection.SyscallConn()
	if err != nil {
		return c.closePumpError(&SerialError{Reason: "could not access serial socket", Err: err})
	}
	deadline := time.Now().Add(timeout)
	contextDeadline, hasContextDeadline := ctx.Deadline()
	contextControlsDeadline := hasContextDeadline && contextDeadline.Before(deadline)
	if contextControlsDeadline {
		deadline = contextDeadline
	}
	for {
		if err := ctx.Err(); err != nil {
			_ = c.Close()
			return SerialPumpResult{}, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			if contextErr := ctx.Err(); contextErr != nil {
				_ = c.Close()
				return SerialPumpResult{}, contextErr
			}
			if contextControlsDeadline {
				_ = c.Close()
				return SerialPumpResult{}, context.DeadlineExceeded
			}
			return SerialPumpResult{}, nil
		}
		poll := min(10*time.Millisecond, remaining)
		var result SerialPumpResult
		var operationErr error
		controlErr := raw.Control(func(fileDescriptor uintptr) {
			fd := int(fileDescriptor)
			if fd < 0 || fd/strconv.IntSize >= len(syscall.FdSet{}.Bits) {
				operationErr = errors.New("serial socket descriptor exceeds select limit")
				return
			}
			var readable, writable syscall.FdSet
			setSerialFD(&readable, fd)
			writeSet := (*syscall.FdSet)(nil)
			if len(outgoing) != 0 {
				setSerialFD(&writable, fd)
				writeSet = &writable
			}
			timeval := syscall.NsecToTimeval(poll.Nanoseconds())
			_, operationErr = syscall.Select(fd+1, &readable, writeSet, nil, &timeval)
			if operationErr != nil {
				return
			}
			if serialFDIsSet(&readable, fd) {
				buffer := make([]byte, maxReadBytes)
				count, _, readErr := syscall.Recvfrom(fd, buffer, syscall.MSG_DONTWAIT)
				if readErr != nil && !errors.Is(readErr, syscall.EAGAIN) {
					operationErr = readErr
					return
				}
				if readErr == nil {
					if count == 0 {
						operationErr = io.EOF
						return
					}
					result.Incoming = buffer[:count]
				}
			}
			if writeSet != nil && serialFDIsSet(&writable, fd) {
				count, writeErr := syscall.SendmsgN(fd, outgoing, nil, nil, syscall.MSG_DONTWAIT|syscall.MSG_NOSIGNAL)
				if writeErr != nil && !errors.Is(writeErr, syscall.EAGAIN) {
					operationErr = writeErr
					return
				}
				result.Sent = count
				if writeErr == nil && count == 0 {
					operationErr = io.ErrUnexpectedEOF
				}
			}
		})
		if controlErr != nil {
			operationErr = controlErr
		}
		if errors.Is(operationErr, syscall.EINTR) {
			continue
		}
		if operationErr != nil {
			return c.closePumpError(&SerialError{Reason: "serial connection closed", Err: operationErr})
		}
		if contextErr := ctx.Err(); contextErr != nil {
			_ = c.Close()
			return SerialPumpResult{}, contextErr
		}
		if result.Incoming != nil || result.Sent != 0 {
			return result, nil
		}
		if time.Now().Compare(deadline) >= 0 {
			if contextControlsDeadline {
				_ = c.Close()
				return SerialPumpResult{}, context.DeadlineExceeded
			}
			return result, nil
		}
	}
}

func (c *SerialConnection) Close() error {
	c.lifecycle.Lock()
	defer c.lifecycle.Unlock()
	c.mutex.Lock()
	connection := c.connection
	c.connection = nil
	c.mutex.Unlock()
	if connection == nil {
		return nil
	}
	return connection.Close()
}

func (c *SerialConnection) requireConnection() (net.Conn, error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	if c.connection == nil {
		return nil, &SerialError{Reason: "serial connection is not connected"}
	}
	return c.connection, nil
}

func (c *SerialConnection) isConnected() bool {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.connection != nil
}

func (c *SerialConnection) closeWithSerialError(err error) error {
	_ = c.Close()
	return err
}

func (c *SerialConnection) closePumpError(err error) (SerialPumpResult, error) {
	_ = c.Close()
	return SerialPumpResult{}, err
}

func setSerialFD(set *syscall.FdSet, descriptor int) {
	set.Bits[descriptor/strconv.IntSize] |= 1 << (uint(descriptor) % strconv.IntSize)
}

func serialFDIsSet(set *syscall.FdSet, descriptor int) bool {
	return set.Bits[descriptor/strconv.IntSize]&(1<<(uint(descriptor)%strconv.IntSize)) != 0
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
