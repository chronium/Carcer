package main

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"codexos/internal/guest"
)

func main() { os.Exit(run()) }

func run() int {
	for _, argument := range os.Args[1:] {
		if argument == "--version" {
			fmt.Println("QEMU emulator version disposable-runner")
			return 0
		}
	}
	qmpPath, serialPath := socketPaths(os.Args[1:])
	if qmpPath == "" || serialPath == "" {
		return 2
	}
	qmpListener, err := net.Listen("unix", qmpPath)
	if err != nil {
		return 3
	}
	defer qmpListener.Close()
	serialListener, err := net.Listen("unix", serialPath)
	if err != nil {
		return 4
	}
	defer serialListener.Close()

	done := make(chan struct{})
	results := make(chan error, 2)
	go func() { results <- serveQMP(qmpListener, done) }()
	go func() { results <- serveSerial(serialListener, done) }()
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	for range 2 {
		select {
		case err := <-results:
			if err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, io.EOF) {
				return 5
			}
		case <-timer.C:
			return 6
		}
	}
	return 0
}

func socketPaths(arguments []string) (string, string) {
	var qmpPath string
	var serialPath string
	for index, argument := range arguments {
		if argument == "-qmp" && index+1 < len(arguments) {
			qmpPath = strings.TrimPrefix(strings.Split(arguments[index+1], ",")[0], "unix:")
		}
		if argument == "-chardev" && index+1 < len(arguments) {
			for _, field := range strings.Split(arguments[index+1], ",") {
				if strings.HasPrefix(field, "path=") {
					serialPath = strings.TrimPrefix(field, "path=")
				}
			}
		}
	}
	return qmpPath, serialPath
}

func serveQMP(listener net.Listener, done chan struct{}) error {
	connection, err := listener.Accept()
	if err != nil {
		return err
	}
	defer connection.Close()
	if _, err := io.WriteString(connection, `{"QMP":{"version":{},"capabilities":[]}}`+"\r\n"); err != nil {
		return err
	}
	reader := bufio.NewReader(connection)
	status := "running"
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return err
		}
		var request map[string]any
		decoder := json.NewDecoder(strings.NewReader(string(line)))
		decoder.UseNumber()
		if err := decoder.Decode(&request); err != nil {
			return err
		}
		command, _ := request["execute"].(string)
		var result any = map[string]any{}
		switch command {
		case "stop":
			status = "paused"
		case "cont":
			status = "running"
		case "query-status":
			result = map[string]any{"status": status}
		case "quit":
			if err := json.NewEncoder(connection).Encode(map[string]any{"return": result, "id": request["id"]}); err != nil {
				return err
			}
			close(done)
			_ = listener.Close()
			return nil
		}
		if err := json.NewEncoder(connection).Encode(map[string]any{"return": result, "id": request["id"]}); err != nil {
			return err
		}
	}
}

func serveSerial(listener net.Listener, done <-chan struct{}) error {
	connection, err := listener.Accept()
	if err != nil {
		return err
	}
	defer connection.Close()
	go func() {
		<-done
		_ = connection.Close()
		_ = listener.Close()
	}()
	if _, err := io.WriteString(connection, guest.ReadyMarker); err != nil {
		return err
	}
	for {
		frame, err := guest.ReadFrame(connection)
		if err != nil {
			return err
		}
		var response guest.Frame
		switch frame.MessageType {
		case guest.ListToolsRequest:
			response = guest.Frame{MessageType: guest.ListToolsResponse, RequestID: frame.RequestID, Payload: toolList("read", "write")}
		case guest.InvokeToolRequest:
			name, err := invokeName(frame.Payload)
			if err != nil {
				return err
			}
			payload := make([]byte, 4+len("tool:")+len(name))
			copy(payload[4:], "tool:"+name)
			response = guest.Frame{MessageType: guest.InvokeToolResponse, RequestID: frame.RequestID, Payload: payload}
		default:
			return fmt.Errorf("unexpected serial message type %#x", frame.MessageType)
		}
		encoded, err := guest.EncodeFrame(response)
		if err != nil {
			return err
		}
		if _, err := connection.Write(encoded); err != nil {
			return err
		}
	}
}

func toolList(names ...string) []byte {
	size := 2
	for _, name := range names {
		size += 2 + len(name)
	}
	payload := make([]byte, size)
	binary.LittleEndian.PutUint16(payload[:2], uint16(len(names)))
	offset := 2
	for _, name := range names {
		binary.LittleEndian.PutUint16(payload[offset:offset+2], uint16(len(name)))
		offset += 2
		copy(payload[offset:], name)
		offset += len(name)
	}
	return payload
}

func invokeName(payload []byte) (string, error) {
	if len(payload) < 2 {
		return "", errors.New("short invoke request")
	}
	length := int(binary.LittleEndian.Uint16(payload[:2]))
	if length == 0 || length > len(payload)-2 {
		return "", errors.New("invalid invoke tool name")
	}
	return string(payload[2 : 2+length]), nil
}
