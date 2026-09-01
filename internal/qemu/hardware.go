package qemu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	hardwareSchemaVersion = 1
	qemuVersionLimit      = 256
	qemuVersionOutputMax  = 1024 * 1024
	hardwareManifestMax   = 1024 * 1024
	kvmReadWriteAccess    = 6 // POSIX R_OK | W_OK.
)

var (
	hardwareProfilePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	hardwareCPUPattern     = regexp.MustCompile(`^[A-Za-z0-9_.+\-]{1,64}$`)
)

type HardwareError struct {
	Reason string
	Err    error
}

func (e *HardwareError) Error() string {
	if e.Err == nil {
		return e.Reason
	}
	return e.Reason + ": " + e.Err.Error()
}

func (e *HardwareError) Unwrap() error { return e.Err }

type HardwareProfile struct {
	Profile              string
	Machine              string
	Accelerator          string
	CPUModel             string
	VCPUs                int
	MemoryMiB            int
	Graphics             string
	Network              string
	WritableBlockDevices []string
}

type HardwareManifest struct {
	SchemaVersion        int      `json:"schema_version"`
	Profile              string   `json:"profile"`
	Machine              string   `json:"machine"`
	Accelerator          string   `json:"accelerator"`
	CPUModel             string   `json:"cpu_model"`
	VCPUs                int      `json:"vcpus"`
	MemoryMiB            int      `json:"memory_mib"`
	Graphics             string   `json:"graphics"`
	Network              string   `json:"network"`
	WritableBlockDevices []string `json:"writable_block_devices"`
	QEMUVersion          string   `json:"qemu_version"`
	QEMUArguments        []string `json:"qemu_arguments"`
}

var ExperimentHardwareProfile = HardwareProfile{
	Profile:              "experiment-v1",
	Machine:              "q35",
	Accelerator:          "kvm",
	CPUModel:             "host",
	VCPUs:                4,
	MemoryMiB:            8192,
	Graphics:             "std-vga",
	Network:              "none",
	WritableBlockDevices: []string{},
}

var TestHardwareProfile = HardwareProfile{
	Profile:              "test-v1",
	Machine:              "q35",
	Accelerator:          "kvm:tcg",
	CPUModel:             "qemu64",
	VCPUs:                1,
	MemoryMiB:            128,
	Graphics:             "std-vga",
	Network:              "none",
	WritableBlockDevices: []string{},
}

func (p HardwareProfile) Validate() error {
	switch {
	case !hardwareProfilePattern.MatchString(p.Profile):
		return &HardwareError{Reason: "invalid CodexOS hardware profile"}
	case p.Machine != "q35":
		return &HardwareError{Reason: "invalid CodexOS hardware machine"}
	case p.Accelerator != "kvm" && p.Accelerator != "kvm:tcg":
		return &HardwareError{Reason: "invalid CodexOS hardware accelerator"}
	case !hardwareCPUPattern.MatchString(p.CPUModel):
		return &HardwareError{Reason: "invalid CodexOS hardware CPU model"}
	case p.VCPUs < 1 || p.VCPUs > 256:
		return &HardwareError{Reason: "invalid CodexOS hardware vCPU count"}
	case p.MemoryMiB < 1 || p.MemoryMiB > 1_048_576:
		return &HardwareError{Reason: "invalid CodexOS hardware memory size"}
	case p.Graphics != "std-vga" || p.Network != "none":
		return &HardwareError{Reason: "invalid CodexOS peripheral hardware"}
	case len(p.WritableBlockDevices) != 0:
		return &HardwareError{Reason: "writable block devices are not supported"}
	default:
		return nil
	}
}

func (p HardwareProfile) RequireAvailable() error {
	if err := p.Validate(); err != nil {
		return err
	}
	if p.Accelerator != "kvm" {
		return nil
	}
	if err := syscall.Access("/dev/kvm", kvmReadWriteAccess); err != nil {
		return &HardwareError{Reason: fmt.Sprintf("%s requires KVM, but /dev/kvm is unavailable", p.Profile)}
	}
	return nil
}

func (p HardwareProfile) QEMUCommandArguments(bootISO, qmpSocket, serialSocket string) ([]string, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if !utf8.ValidString(bootISO) || !utf8.ValidString(qmpSocket) || !utf8.ValidString(serialSocket) {
		return nil, &HardwareError{Reason: "QEMU path is not valid UTF-8"}
	}
	bootFile, err := compactASCIIJSON(struct {
		Driver   string `json:"driver"`
		Filename string `json:"filename"`
		NodeName string `json:"node-name"`
		ReadOnly bool   `json:"read-only"`
	}{Driver: "file", Filename: bootISO, NodeName: "codexos-boot-file", ReadOnly: true})
	if err != nil {
		return nil, &HardwareError{Reason: "could not encode QEMU boot file", Err: err}
	}
	bootRaw, err := compactASCIIJSON(struct {
		Driver   string `json:"driver"`
		File     string `json:"file"`
		NodeName string `json:"node-name"`
		ReadOnly bool   `json:"read-only"`
	}{Driver: "raw", File: "codexos-boot-file", NodeName: "codexos-boot-cd", ReadOnly: true})
	if err != nil {
		return nil, &HardwareError{Reason: "could not encode QEMU boot device", Err: err}
	}
	return []string{
		"-machine", p.Machine + ",accel=" + p.Accelerator + ",pcspk-audiodev=codexos-noaudio",
		"-cpu", p.CPUModel,
		"-smp", fmt.Sprintf("%d", p.VCPUs),
		"-m", fmt.Sprintf("%dM", p.MemoryMiB),
		"-nodefaults",
		"-display", "none",
		"-monitor", "none",
		"-no-reboot",
		"-nic", "none",
		"-audiodev", "none,id=codexos-noaudio",
		"-blockdev", bootFile,
		"-blockdev", bootRaw,
		"-device", "ide-cd,drive=codexos-boot-cd,bootindex=1",
		"-device", "VGA",
		"-chardev", "socket,id=codexos-com1,path=" + serialSocket + ",server=on,wait=off",
		"-device", "isa-serial,chardev=codexos-com1,index=0",
		"-qmp", "unix:" + qmpSocket + ",server=on,wait=off",
	}, nil
}

func (p HardwareProfile) Manifest(qemuVersion string) (HardwareManifest, error) {
	arguments, err := p.QEMUCommandArguments("<BOOT_ISO>", "<QMP_SOCKET>", "<SERIAL_SOCKET>")
	if err != nil {
		return HardwareManifest{}, err
	}
	manifest := HardwareManifest{
		SchemaVersion:        hardwareSchemaVersion,
		Profile:              p.Profile,
		Machine:              p.Machine,
		Accelerator:          p.Accelerator,
		CPUModel:             p.CPUModel,
		VCPUs:                p.VCPUs,
		MemoryMiB:            p.MemoryMiB,
		Graphics:             p.Graphics,
		Network:              p.Network,
		WritableBlockDevices: []string{},
		QEMUVersion:          qemuVersion,
		QEMUArguments:        arguments,
	}
	if err := ValidateHardwareManifest(manifest); err != nil {
		return HardwareManifest{}, err
	}
	return manifest, nil
}

func ValidateHardwareManifest(manifest HardwareManifest) error {
	if manifest.SchemaVersion != hardwareSchemaVersion || manifest.WritableBlockDevices == nil || manifest.QEMUArguments == nil {
		return malformedHardwareManifest()
	}
	profile := HardwareProfile{
		Profile:              manifest.Profile,
		Machine:              manifest.Machine,
		Accelerator:          manifest.Accelerator,
		CPUModel:             manifest.CPUModel,
		VCPUs:                manifest.VCPUs,
		MemoryMiB:            manifest.MemoryMiB,
		Graphics:             manifest.Graphics,
		Network:              manifest.Network,
		WritableBlockDevices: manifest.WritableBlockDevices,
	}
	if err := profile.Validate(); err != nil {
		return err
	}
	if !validQEMUVersion(manifest.QEMUVersion) {
		return malformedHardwareManifest()
	}
	expected, err := profile.QEMUCommandArguments("<BOOT_ISO>", "<QMP_SOCKET>", "<SERIAL_SOCKET>")
	if err != nil {
		return err
	}
	if !equalStrings(manifest.QEMUArguments, expected) {
		return malformedHardwareManifest()
	}
	return nil
}

func ParseHardwareManifest(encoded []byte) (HardwareManifest, error) {
	if len(encoded) > hardwareManifestMax || !utf8.Valid(encoded) {
		return HardwareManifest{}, malformedHardwareManifest()
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	var fields map[string]json.RawMessage
	if err := decoder.Decode(&fields); err != nil {
		return HardwareManifest{}, malformedHardwareManifestWith(err)
	}
	if err := requireHardwareJSONEOF(decoder); err != nil {
		return HardwareManifest{}, malformedHardwareManifestWith(err)
	}
	expectedKeys := []string{
		"schema_version", "profile", "machine", "accelerator", "cpu_model", "vcpus",
		"memory_mib", "graphics", "network", "writable_block_devices", "qemu_version", "qemu_arguments",
	}
	if len(fields) != len(expectedKeys) {
		return HardwareManifest{}, malformedHardwareManifest()
	}
	for _, key := range expectedKeys {
		if _, exists := fields[key]; !exists {
			return HardwareManifest{}, malformedHardwareManifest()
		}
	}
	var manifest HardwareManifest
	if err := json.Unmarshal(encoded, &manifest); err != nil {
		return HardwareManifest{}, malformedHardwareManifestWith(err)
	}
	if err := ValidateHardwareManifest(manifest); err != nil {
		return HardwareManifest{}, err
	}
	return manifest, nil
}

func EncodeHardwareManifest(manifest HardwareManifest) ([]byte, error) {
	if err := ValidateHardwareManifest(manifest); err != nil {
		return nil, err
	}
	value := struct {
		Accelerator          string   `json:"accelerator"`
		CPUModel             string   `json:"cpu_model"`
		Graphics             string   `json:"graphics"`
		Machine              string   `json:"machine"`
		MemoryMiB            int      `json:"memory_mib"`
		Network              string   `json:"network"`
		Profile              string   `json:"profile"`
		QEMUArguments        []string `json:"qemu_arguments"`
		QEMUVersion          string   `json:"qemu_version"`
		SchemaVersion        int      `json:"schema_version"`
		VCPUs                int      `json:"vcpus"`
		WritableBlockDevices []string `json:"writable_block_devices"`
	}{
		Accelerator: manifest.Accelerator, CPUModel: manifest.CPUModel, Graphics: manifest.Graphics,
		Machine: manifest.Machine, MemoryMiB: manifest.MemoryMiB, Network: manifest.Network,
		Profile: manifest.Profile, QEMUArguments: manifest.QEMUArguments, QEMUVersion: manifest.QEMUVersion,
		SchemaVersion: manifest.SchemaVersion, VCPUs: manifest.VCPUs, WritableBlockDevices: manifest.WritableBlockDevices,
	}
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return nil, &HardwareError{Reason: "could not encode generation hardware manifest", Err: err}
	}
	return hardwareASCIIJSON(output.Bytes()), nil
}

func DiscoverQEMUVersion(ctx context.Context, executable string) (string, error) {
	operationContext, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	command := exec.CommandContext(operationContext, executable, "--version")
	output := &boundedOutput{limit: qemuVersionOutputMax}
	command.Stdout = output
	command.Stderr = output
	if err := command.Run(); err != nil {
		if contextErr := operationContext.Err(); contextErr != nil {
			return "", &HardwareError{Reason: "could not determine QEMU version", Err: contextErr}
		}
		return "", &HardwareError{Reason: "could not determine QEMU version", Err: err}
	}
	if output.overflow {
		return "", &HardwareError{Reason: "could not determine QEMU version: output exceeds 1 MiB"}
	}
	if !utf8.Valid(output.bytes) {
		return "", &HardwareError{Reason: "QEMU version output is invalid"}
	}
	version, found := firstPythonTextLine(string(output.bytes))
	if !found {
		return "", &HardwareError{Reason: "QEMU version output is empty"}
	}
	if !validQEMUVersion(version) {
		return "", &HardwareError{Reason: "QEMU version output is invalid"}
	}
	return version, nil
}

type boundedOutput struct {
	bytes    []byte
	limit    int
	overflow bool
}

func (w *boundedOutput) Write(value []byte) (int, error) {
	remaining := w.limit - len(w.bytes)
	if remaining > 0 {
		w.bytes = append(w.bytes, value[:min(remaining, len(value))]...)
	}
	if len(value) > remaining {
		w.overflow = true
	}
	return len(value), nil
}

func compactASCIIJSON(value any) (string, error) {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return "", err
	}
	encoded := hardwareASCIIJSON(output.Bytes())
	return strings.TrimSuffix(string(encoded), "\n"), nil
}

func hardwareASCIIJSON(encoded []byte) []byte {
	output := make([]byte, 0, len(encoded))
	for len(encoded) > 0 {
		r, size := utf8.DecodeRune(encoded)
		if r < utf8.RuneSelf {
			output = append(output, byte(r))
		} else if r <= 0xffff {
			output = fmt.Appendf(output, `\u%04x`, r)
		} else {
			first, second := utf16.EncodeRune(r)
			output = fmt.Appendf(output, `\u%04x\u%04x`, first, second)
		}
		encoded = encoded[size:]
	}
	return output
}

func validQEMUVersion(version string) bool {
	if version == "" || !utf8.ValidString(version) || len([]byte(version)) > qemuVersionLimit {
		return false
	}
	for _, value := range version {
		if !unicode.IsPrint(value) {
			return false
		}
	}
	return true
}

func firstPythonTextLine(value string) (string, bool) {
	if value == "" {
		return "", false
	}
	for index, character := range value {
		switch character {
		case '\n', '\v', '\f', '\r', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
			return value[:index], true
		}
	}
	return value, true
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func requireHardwareJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON data")
	}
	return nil
}

func malformedHardwareManifest() error {
	return &HardwareError{Reason: "generation hardware manifest is malformed"}
}

func malformedHardwareManifestWith(err error) error {
	return &HardwareError{Reason: "generation hardware manifest is malformed", Err: err}
}
