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
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"codexos/internal/guest"
)

const (
	fakeQEMUModeEnvironment           = "CODEXOS_DISPOSABLE_QEMU_LIFECYCLE"
	processRecordDirectoryEnvironment = "CODEXOS_DISPOSABLE_PROCESS_RECORDS"
	largeShutdownMode                 = "large-shutdown"
	largeShutdownSocketBuffer         = 1024
)

const maxHostResponseOutput = 64 * 1024

var canonicalSeedSourcePaths = []string{
	"seed/files.h",
	"seed/kernel.c",
	"seed/limine.conf",
	"seed/linker.ld",
}

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
	if err := recordProcess("qemu"); err != nil {
		return 7
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
	mode := os.Getenv(fakeQEMUModeEnvironment)
	lifecycle := mode == "1" || mode == "lifecycle"
	largeShutdown := mode == largeShutdownMode
	candidate := hasArgument(os.Args[1:], "-S")
	go func() {
		results <- serveSerial(serialListener, done, lifecycle, lifecycle && !candidate, largeShutdown && !candidate)
	}()
	timeout := 15 * time.Second
	if (lifecycle || largeShutdown) && !candidate {
		timeout = time.Minute
	}
	timer := time.NewTimer(timeout)
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

func recordProcess(kind string) error {
	directory := os.Getenv(processRecordDirectoryEnvironment)
	if directory == "" {
		return nil
	}
	path := filepath.Join(directory, fmt.Sprintf("%s-%d.pid", kind, os.Getpid()))
	return os.WriteFile(path, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600)
}

func recordLargeShutdownRequest() error {
	directory := os.Getenv(processRecordDirectoryEnvironment)
	if directory == "" {
		return nil
	}
	path := filepath.Join(directory, fmt.Sprintf("qemu-large-response-requested-%d.marker", os.Getpid()))
	return os.WriteFile(path, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600)
}

func hasArgument(arguments []string, wanted string) bool {
	for _, argument := range arguments {
		if argument == wanted {
			return true
		}
	}
	return false
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

func serveSerial(listener net.Listener, done <-chan struct{}, lifecycle, activeLifecycle, largeShutdown bool) error {
	connection, err := listener.Accept()
	if err != nil {
		return err
	}
	defer connection.Close()
	if largeShutdown {
		unixConnection, ok := connection.(*net.UnixConn)
		if !ok {
			return errors.New("large-shutdown serial connection is not a Unix socket")
		}
		if err := unixConnection.SetReadBuffer(largeShutdownSocketBuffer); err != nil {
			return fmt.Errorf("set large-shutdown serial read buffer: %w", err)
		}
		if err := unixConnection.SetWriteBuffer(largeShutdownSocketBuffer); err != nil {
			return fmt.Errorf("set large-shutdown serial write buffer: %w", err)
		}
	}
	go func() {
		<-done
		_ = connection.Close()
		_ = listener.Close()
	}()
	if id := os.Getenv("CODEXOS_DISPOSABLE_BOOTSTRAP_ARTIFACT"); id != "" {
		request, err := encodeHostServiceRequest(91, "read_bootstrap_artifact", [][]byte{[]byte(id), []byte("0"), []byte("12")})
		if err != nil {
			return err
		}
		if _, err = connection.Write(request); err != nil {
			return err
		}
		response, err := guest.ReadFrame(connection)
		if err != nil {
			return err
		}
		want := uint32(0)
		if os.Getenv("CODEXOS_DISPOSABLE_BOOTSTRAP_DISABLED") == "1" {
			want = 2
		}
		if response.RequestID != 91 || len(response.Payload) < 4 || binary.LittleEndian.Uint32(response.Payload) != want {
			return errors.New("incorrect pre-ready artifact authorization")
		}
		if want == 0 && string(response.Payload[4:]) != "boot runtime" {
			return errors.New("incorrect inherited boot bytes")
		}
		if err = os.WriteFile(os.Getenv("CODEXOS_DISPOSABLE_BOOTSTRAP_READ_MARKER"), []byte("read before ready"), 0600); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(connection, guest.ReadyMarker); err != nil {
		return err
	}
	if largeShutdown {
		request, err := encodeHostServiceRequest(1, "read_provided_asset", [][]byte{[]byte("bulk"), []byte("0"), []byte("1048576")})
		if err != nil {
			return err
		}
		if _, err := connection.Write(request); err != nil {
			return err
		}
		if err := recordLargeShutdownRequest(); err != nil {
			return err
		}
		<-done
		return nil
	}
	var snapshot []byte
	if activeLifecycle {
		snapshot, err = canonicalSeedSnapshot()
		if err != nil {
			return err
		}
	}
	nextHostRequestID := uint32(1)
	for {
		frame, err := guest.ReadFrame(connection)
		if err != nil {
			select {
			case <-done:
				return nil
			default:
			}
			return err
		}
		var response guest.Frame
		switch frame.MessageType {
		case guest.ListToolsRequest:
			if lifecycle {
				response = guest.Frame{
					MessageType: guest.ListToolsResponse,
					RequestID:   frame.RequestID,
					Payload:     toolList("list", "read", "write", "truncate", "remove", "build", "finish_generation", "request_feature"),
				}
			} else {
				response = guest.Frame{MessageType: guest.ListToolsResponse, RequestID: frame.RequestID, Payload: toolList("read", "write")}
			}
		case guest.InvokeToolRequest:
			if activeLifecycle {
				name, arguments, err := decodeInvoke(frame.Payload)
				if err != nil {
					return err
				}
				response, err = lifecycleInvoke(connection, frame.RequestID, name, arguments, snapshot, &nextHostRequestID)
				if err != nil {
					return err
				}
			} else {
				name, err := invokeName(frame.Payload)
				if err != nil {
					return err
				}
				payload := make([]byte, 4+len("tool:")+len(name))
				copy(payload[4:], "tool:"+name)
				response = guest.Frame{MessageType: guest.InvokeToolResponse, RequestID: frame.RequestID, Payload: payload}
			}
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

func lifecycleInvoke(connection net.Conn, toolRequestID uint32, name string, arguments [][]byte, snapshot []byte, nextHostRequestID *uint32) (guest.Frame, error) {
	if name != "build" && name != "finish_generation" {
		payload := make([]byte, 4+len("tool:")+len(name))
		copy(payload[4:], "tool:"+name)
		return guest.Frame{MessageType: guest.InvokeToolResponse, RequestID: toolRequestID, Payload: payload}, nil
	}

	if nextHostRequestID == nil || *nextHostRequestID == 0 {
		return guest.Frame{}, errors.New("host-service request ID allocator is unavailable")
	}
	hostRequestID := *nextHostRequestID
	if hostRequestID == ^uint32(0) {
		*nextHostRequestID = 1
	} else {
		(*nextHostRequestID)++
	}
	service := name
	hostArguments := [][]byte{snapshot}
	if name == "finish_generation" {
		if len(arguments) != 1 {
			return guest.Frame{}, errors.New("finish_generation requires one handoff argument")
		}
		hostArguments = [][]byte{arguments[0], snapshot}
	}
	hostFrame, err := encodeHostServiceRequest(hostRequestID, service, hostArguments)
	if err != nil {
		return guest.Frame{}, err
	}
	if _, err := connection.Write(hostFrame); err != nil {
		return guest.Frame{}, err
	}
	hostResponse, err := guest.ReadFrame(connection)
	if err != nil {
		return guest.Frame{}, err
	}
	if hostResponse.MessageType != guest.HostServiceResponse || hostResponse.RequestID != hostRequestID || len(hostResponse.Payload) < 4 {
		return guest.Frame{}, fmt.Errorf("invalid host-service response for %s", name)
	}
	status := binary.LittleEndian.Uint32(hostResponse.Payload[:4])
	if status > 2 || len(hostResponse.Payload)-4 > maxHostResponseOutput {
		return guest.Frame{
			MessageType: guest.InvokeToolResponse,
			RequestID:   toolRequestID,
			Payload:     []byte{1, 0, 0, 0},
		}, nil
	}
	return guest.Frame{MessageType: guest.InvokeToolResponse, RequestID: toolRequestID, Payload: hostResponse.Payload}, nil
}

func encodeHostServiceRequest(requestID uint32, service string, arguments [][]byte) ([]byte, error) {
	if requestID == 0 || service == "" || !utf8.ValidString(service) || len(service) > 255 || len(arguments) > 64 {
		return nil, errors.New("invalid host-service request")
	}
	size := 2 + len(service) + 2
	for _, argument := range arguments {
		size += 4 + len(argument)
	}
	payload := make([]byte, size)
	binary.LittleEndian.PutUint16(payload[:2], uint16(len(service)))
	copy(payload[2:], service)
	offset := 2 + len(service)
	binary.LittleEndian.PutUint16(payload[offset:offset+2], uint16(len(arguments)))
	offset += 2
	for _, argument := range arguments {
		binary.LittleEndian.PutUint32(payload[offset:offset+4], uint32(len(argument)))
		offset += 4
		copy(payload[offset:], argument)
		offset += len(argument)
	}
	return guest.EncodeFrame(guest.Frame{MessageType: guest.HostServiceRequest, RequestID: requestID, Payload: payload})
}

func decodeInvoke(payload []byte) (string, [][]byte, error) {
	if len(payload) < 2 {
		return "", nil, errors.New("short invoke request")
	}
	offset := 0
	nameLength := int(binary.LittleEndian.Uint16(payload[offset : offset+2]))
	offset += 2
	if nameLength == 0 || nameLength > 255 || nameLength > len(payload)-offset {
		return "", nil, errors.New("invalid invoke tool name")
	}
	nameBytes := payload[offset : offset+nameLength]
	if !utf8.Valid(nameBytes) {
		return "", nil, errors.New("invoke tool name is not valid UTF-8")
	}
	name := string(nameBytes)
	offset += nameLength
	if len(payload)-offset < 2 {
		return "", nil, errors.New("invoke request is missing its argument count")
	}
	argumentCount := int(binary.LittleEndian.Uint16(payload[offset : offset+2]))
	offset += 2
	if argumentCount > 64 {
		return "", nil, errors.New("invoke request contains too many arguments")
	}
	arguments := make([][]byte, 0, argumentCount)
	for range argumentCount {
		if len(payload)-offset < 4 {
			return "", nil, errors.New("invoke request has a truncated argument length")
		}
		length := uint64(binary.LittleEndian.Uint32(payload[offset : offset+4]))
		offset += 4
		if length > uint64(len(payload)-offset) {
			return "", nil, errors.New("invoke request has a truncated argument")
		}
		argument := append([]byte(nil), payload[offset:offset+int(length)]...)
		offset += int(length)
		arguments = append(arguments, argument)
	}
	if offset != len(payload) {
		return "", nil, errors.New("invoke request contains trailing data")
	}
	return name, arguments, nil
}

func canonicalSeedSnapshot() ([]byte, error) {
	root, err := findSeedRoot()
	if err != nil {
		return nil, err
	}
	files := make([]guest.SnapshotFile, 0, len(canonicalSeedSourcePaths))
	for _, path := range canonicalSeedSourcePaths {
		content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			return nil, fmt.Errorf("read canonical seed source %s: %w", path, err)
		}
		files = append(files, guest.SnapshotFile{Path: path, Content: content})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return guest.EncodeSourceSnapshot(files)
}

func findSeedRoot() (string, error) {
	directory, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("find canonical seed source: %w", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "seed", "kernel.c")); err == nil {
			return directory, nil
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			break
		}
		directory = parent
	}
	return "", errors.New("find canonical seed source: repository root is unavailable")
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
