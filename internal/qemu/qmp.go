package qemu

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	qmpTimeout        = 5 * time.Second
	maxQMPMessageSize = 1024 * 1024
)

type QMPError struct {
	Reason string
	Err    error
}

func (e *QMPError) Error() string {
	if e.Err == nil {
		return e.Reason
	}
	return e.Reason + ": " + e.Err.Error()
}

func (e *QMPError) Unwrap() error { return e.Err }

// QMPClient synchronously controls one QEMU process. One caller owns the
// client; it has no background read loop.
type QMPClient struct {
	socketPath string
	timeout    time.Duration
	connection net.Conn
	reader     *bufio.Reader
	writer     *bufio.Writer
	nextID     uint64
}

func NewQMPClient(socketPath string) *QMPClient {
	return &QMPClient{socketPath: socketPath, timeout: qmpTimeout, nextID: 1}
}

func (c *QMPClient) Connect(ctx context.Context) error {
	if c.connection != nil {
		return &QMPError{Reason: "QMP client is already connected"}
	}
	deadline := time.Now().Add(c.timeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		dialer := net.Dialer{Deadline: deadline}
		connection, err := dialer.DialContext(ctx, "unix", c.socketPath)
		if err == nil {
			c.connection = connection
			c.reader = bufio.NewReader(connection)
			c.writer = bufio.NewWriter(connection)
			stopCancellation := interruptConnectionOnCancel(ctx, connection)
			if err := c.readGreeting(ctx); err != nil {
				stopCancellation()
				c.Close()
				return err
			}
			if _, err := c.execute(ctx, "qmp_capabilities"); err != nil {
				stopCancellation()
				c.Close()
				return err
			}
			stopCancellation()
			return nil
		}
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		if time.Until(deadline) <= 0 {
			return &QMPError{Reason: fmt.Sprintf("timed out connecting to QMP socket %s", c.socketPath), Err: err}
		}
		if !retryableQMPConnectError(err) {
			return err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			if contextErr := ctx.Err(); contextErr != nil {
				return contextErr
			}
			return &QMPError{Reason: fmt.Sprintf("timed out connecting to QMP socket %s", c.socketPath), Err: err}
		}
		wait := min(10*time.Millisecond, remaining)
		timer := time.NewTimer(wait)
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

func (c *QMPClient) QueryStatus(ctx context.Context) (string, error) {
	result, err := c.execute(ctx, "query-status")
	if err != nil {
		return "", err
	}
	object, ok := result.(map[string]any)
	if !ok {
		return "", &QMPError{Reason: "query-status returned an invalid response"}
	}
	status, ok := object["status"].(string)
	if !ok {
		return "", &QMPError{Reason: "query-status returned an invalid response"}
	}
	return status, nil
}

func (c *QMPClient) Stop(ctx context.Context) error {
	_, err := c.execute(ctx, "stop")
	return err
}

func (c *QMPClient) Continue(ctx context.Context) error {
	_, err := c.execute(ctx, "cont")
	return err
}

func (c *QMPClient) Quit(ctx context.Context) error {
	_, err := c.execute(ctx, "quit")
	return err
}

func (c *QMPClient) Close() error {
	connection := c.connection
	c.connection = nil
	c.reader = nil
	c.writer = nil
	if connection == nil {
		return nil
	}
	return connection.Close()
}

func (c *QMPClient) readGreeting(ctx context.Context) error {
	greeting, err := c.readMessage(ctx)
	if err != nil {
		return err
	}
	qmp, ok := greeting["QMP"].(map[string]any)
	if !ok {
		return &QMPError{Reason: "invalid QMP greeting"}
	}
	if _, ok := qmp["version"].(map[string]any); !ok {
		return &QMPError{Reason: "invalid QMP greeting"}
	}
	if _, ok := qmp["capabilities"].([]any); !ok {
		return &QMPError{Reason: "invalid QMP greeting"}
	}
	return nil
}

func (c *QMPClient) execute(ctx context.Context, command string) (any, error) {
	if c.connection == nil || c.writer == nil {
		return nil, &QMPError{Reason: "QMP client is not connected"}
	}
	stopCancellation := interruptConnectionOnCancel(ctx, c.connection)
	defer stopCancellation()
	requestID := c.nextID
	if requestID == 0 {
		return nil, &QMPError{Reason: "QMP request ID space is exhausted"}
	}
	c.nextID++
	request := struct {
		Execute string `json:"execute"`
		ID      uint64 `json:"id"`
	}{Execute: command, ID: requestID}
	encoded, err := json.Marshal(request)
	if err != nil {
		return nil, &QMPError{Reason: command + " request could not be encoded", Err: err}
	}
	encoded = append(encoded, '\r', '\n')
	if err := c.setWriteDeadline(ctx); err != nil {
		return nil, err
	}
	if _, err := c.writer.Write(encoded); err != nil {
		return c.closeWithError(qmpOperationError(ctx, command+" request failed", err))
	}
	if err := c.writer.Flush(); err != nil {
		return c.closeWithError(qmpOperationError(ctx, command+" request failed", err))
	}

	for {
		response, err := c.readMessage(ctx)
		if err != nil {
			return c.closeWithError(err)
		}
		if _, event := response["event"]; event {
			continue
		}
		if !qmpResponseIDMatches(response["id"], requestID) {
			return c.closeWithError(&QMPError{Reason: fmt.Sprintf("%s received response for unexpected id %v", command, response["id"])})
		}
		if responseError, exists := response["error"]; exists && responseError != nil {
			object, ok := responseError.(map[string]any)
			if !ok {
				return c.closeWithError(&QMPError{Reason: command + " returned an invalid error response"})
			}
			errorClass, exists := object["class"]
			if !exists {
				errorClass = "unknown error"
			}
			description, exists := object["desc"]
			if !exists {
				description = "no description"
			}
			return nil, &QMPError{Reason: fmt.Sprintf("%s failed: %v: %v", command, errorClass, description)}
		}
		result, exists := response["return"]
		if !exists {
			return c.closeWithError(&QMPError{Reason: command + " returned no result"})
		}
		return result, nil
	}
}

func (c *QMPClient) closeWithError(err error) (any, error) {
	_ = c.Close()
	return nil, err
}

func (c *QMPClient) readMessage(ctx context.Context) (map[string]any, error) {
	if c.connection == nil || c.reader == nil {
		return nil, &QMPError{Reason: "QMP client is not connected"}
	}
	line, err := c.readLine(ctx)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(line) {
		return nil, &QMPError{Reason: "QMP returned invalid JSON"}
	}
	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.UseNumber()
	var message any
	if err := decoder.Decode(&message); err != nil {
		return nil, &QMPError{Reason: "QMP returned invalid JSON", Err: err}
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, &QMPError{Reason: "QMP returned invalid JSON", Err: err}
	}
	object, ok := message.(map[string]any)
	if !ok {
		return nil, &QMPError{Reason: "QMP returned a non-object message"}
	}
	return object, nil
}

func (c *QMPClient) readLine(ctx context.Context) ([]byte, error) {
	line := make([]byte, 0, 4096)
	for {
		if err := c.setReadDeadline(ctx); err != nil {
			return nil, err
		}
		fragment, err := c.reader.ReadSlice('\n')
		if len(fragment) > maxQMPMessageSize-len(line) {
			return nil, &QMPError{Reason: "QMP message exceeds 1 MiB"}
		}
		line = append(line, fragment...)
		switch {
		case err == nil:
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF) && len(line) > 0:
			return line, nil
		case errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded):
			return nil, qmpOperationError(ctx, "QMP read timed out", err)
		case len(line) == 0:
			return nil, &QMPError{Reason: "QMP connection closed", Err: err}
		default:
			return nil, &QMPError{Reason: "QMP connection closed while reading a message", Err: err}
		}
	}
}

func (c *QMPClient) setReadDeadline(ctx context.Context) error {
	deadline, err := c.operationDeadline(ctx)
	if err != nil {
		return err
	}
	if err := c.connection.SetReadDeadline(deadline); err != nil {
		return &QMPError{Reason: "could not set QMP read deadline", Err: err}
	}
	return nil
}

func (c *QMPClient) setWriteDeadline(ctx context.Context) error {
	deadline, err := c.operationDeadline(ctx)
	if err != nil {
		return err
	}
	if err := c.connection.SetWriteDeadline(deadline); err != nil {
		return &QMPError{Reason: "could not set QMP write deadline", Err: err}
	}
	return nil
}

func (c *QMPClient) operationDeadline(ctx context.Context) (time.Time, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	deadline := time.Now().Add(c.timeout)
	if contextDeadline, ok := ctx.Deadline(); ok && contextDeadline.Before(deadline) {
		deadline = contextDeadline
	}
	return deadline, nil
}

func retryableQMPConnectError(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED)
}

func qmpResponseIDMatches(value any, expected uint64) bool {
	number, ok := value.(json.Number)
	if !ok {
		return false
	}
	identifier, err := strconv.ParseUint(string(number), 10, 64)
	return err == nil && identifier == expected
}

func qmpOperationError(ctx context.Context, reason string, err error) error {
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	return &QMPError{Reason: reason, Err: err}
}

func interruptConnectionOnCancel(ctx context.Context, connection net.Conn) func() {
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
