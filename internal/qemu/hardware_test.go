package qemu

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestHardwareProfileBuildsFrozenQEMUArguments(t *testing.T) {
	arguments, err := ExperimentHardwareProfile.QEMUCommandArguments(
		"/trusted/boot-次<&>.iso",
		"/trusted/qmp.sock",
		"/trusted/serial.sock",
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"-machine", "q35,accel=kvm,pcspk-audiodev=codexos-noaudio",
		"-cpu", "host",
		"-smp", "4",
		"-m", "8192M",
		"-nodefaults",
		"-display", "none",
		"-monitor", "none",
		"-no-reboot",
		"-nic", "none",
		"-audiodev", "none,id=codexos-noaudio",
		"-blockdev", `{"driver":"file","filename":"/trusted/boot-\u6b21<&>.iso","node-name":"codexos-boot-file","read-only":true}`,
		"-blockdev", `{"driver":"raw","file":"codexos-boot-file","node-name":"codexos-boot-cd","read-only":true}`,
		"-device", "ide-cd,drive=codexos-boot-cd,bootindex=1",
		"-device", "VGA",
		"-chardev", "socket,id=codexos-com1,path=/trusted/serial.sock,server=on,wait=off",
		"-device", "isa-serial,chardev=codexos-com1,index=0",
		"-qmp", "unix:/trusted/qmp.sock,server=on,wait=off",
	}
	if !reflect.DeepEqual(arguments, want) {
		t.Fatalf("arguments differ:\n got: %#v\nwant: %#v", arguments, want)
	}
}

func TestHardwareProfileValidation(t *testing.T) {
	valid := TestHardwareProfile
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid profile: %v", err)
	}
	if err := valid.RequireAvailable(); err != nil {
		t.Fatalf("test profile should not require KVM: %v", err)
	}
	tests := []struct {
		name string
		edit func(*HardwareProfile)
		want string
	}{
		{name: "profile empty", edit: func(p *HardwareProfile) { p.Profile = "" }, want: "hardware profile"},
		{name: "profile uppercase", edit: func(p *HardwareProfile) { p.Profile = "Test" }, want: "hardware profile"},
		{name: "profile too long", edit: func(p *HardwareProfile) { p.Profile = "a" + strings.Repeat("-", 64) }, want: "hardware profile"},
		{name: "machine", edit: func(p *HardwareProfile) { p.Machine = "pc" }, want: "hardware machine"},
		{name: "accelerator", edit: func(p *HardwareProfile) { p.Accelerator = "tcg" }, want: "hardware accelerator"},
		{name: "CPU empty", edit: func(p *HardwareProfile) { p.CPUModel = "" }, want: "CPU model"},
		{name: "CPU slash", edit: func(p *HardwareProfile) { p.CPUModel = "x/64" }, want: "CPU model"},
		{name: "vCPU low", edit: func(p *HardwareProfile) { p.VCPUs = 0 }, want: "vCPU count"},
		{name: "vCPU high", edit: func(p *HardwareProfile) { p.VCPUs = 257 }, want: "vCPU count"},
		{name: "memory low", edit: func(p *HardwareProfile) { p.MemoryMiB = 0 }, want: "memory size"},
		{name: "memory high", edit: func(p *HardwareProfile) { p.MemoryMiB = 1_048_577 }, want: "memory size"},
		{name: "graphics", edit: func(p *HardwareProfile) { p.Graphics = "none" }, want: "peripheral hardware"},
		{name: "network", edit: func(p *HardwareProfile) { p.Network = "e1000" }, want: "peripheral hardware"},
		{name: "writable", edit: func(p *HardwareProfile) { p.WritableBlockDevices = []string{"disk"} }, want: "writable block devices"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			profile := valid
			test.edit(&profile)
			err := profile.Validate()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestHardwareManifestExactEncodingAndParsing(t *testing.T) {
	manifest, err := TestHardwareProfile.Manifest("QEMU emulator version λ<&>")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeHardwareManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	want := "{\n" +
		"  \"accelerator\": \"kvm:tcg\",\n" +
		"  \"cpu_model\": \"qemu64\",\n" +
		"  \"graphics\": \"std-vga\",\n" +
		"  \"machine\": \"q35\",\n" +
		"  \"memory_mib\": 128,\n" +
		"  \"network\": \"none\",\n" +
		"  \"profile\": \"test-v1\",\n" +
		"  \"qemu_arguments\": [\n" +
		strings.Join(indentJSONStrings(t, manifest.QEMUArguments), ",\n") + "\n" +
		"  ],\n" +
		"  \"qemu_version\": \"QEMU emulator version \\u03bb<&>\",\n" +
		"  \"schema_version\": 1,\n" +
		"  \"vcpus\": 1,\n" +
		"  \"writable_block_devices\": []\n" +
		"}\n"
	if string(encoded) != want {
		t.Fatalf("manifest bytes differ:\nGo: %s\nWant: %s", encoded, want)
	}
	parsed, err := ParseHardwareManifest(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed, manifest) {
		t.Fatalf("parsed manifest = %#v, want %#v", parsed, manifest)
	}
}

func TestHardwareManifestLiveDisplayRoundTrip(t *testing.T) {
	manifest, err := TestHardwareProfile.Manifest("QEMU emulator version test")
	if err != nil {
		t.Fatal(err)
	}
	EnableDisplay(manifest.QEMUArguments)
	encoded, err := EncodeHardwareManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseHardwareManifest(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(parsed, manifest) {
		t.Fatalf("live display manifest changed on round trip: %#v", parsed)
	}
	for _, change := range []string{"display backend", "extra device"} {
		t.Run(change, func(t *testing.T) {
			modified := manifest
			modified.QEMUArguments = append([]string(nil), manifest.QEMUArguments...)
			if change == "display backend" {
				for i, argument := range modified.QEMUArguments {
					if argument == "-display" {
						modified.QEMUArguments[i+1] = "sdl"
						break
					}
				}
			} else {
				modified.QEMUArguments = append(modified.QEMUArguments, "-device", "virtio-net-pci")
			}
			encoded, err := json.Marshal(modified)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ParseHardwareManifest(encoded); err == nil {
				t.Fatal("live display manifest accepted unsupported arguments")
			}
		})
	}
}

func TestHardwareManifestRejectsMalformedState(t *testing.T) {
	manifest, err := TestHardwareProfile.Manifest("QEMU emulator version test")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeHardwareManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var valid map[string]any
	if err := json.Unmarshal(encoded, &valid); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		edit func(map[string]any)
		want string
	}{
		{name: "extra key", edit: func(v map[string]any) { v["extra"] = true }, want: "malformed"},
		{name: "missing key", edit: func(v map[string]any) { delete(v, "network") }, want: "malformed"},
		{name: "boolean schema", edit: func(v map[string]any) { v["schema_version"] = true }, want: "malformed"},
		{name: "fractional vCPU", edit: func(v map[string]any) { v["vcpus"] = 1.5 }, want: "malformed"},
		{name: "null writable", edit: func(v map[string]any) { v["writable_block_devices"] = nil }, want: "malformed"},
		{name: "writable device", edit: func(v map[string]any) { v["writable_block_devices"] = []any{"disk"} }, want: "writable block devices"},
		{name: "wrong arguments", edit: func(v map[string]any) { v["qemu_arguments"] = []any{"-machine", "none"} }, want: "malformed"},
		{name: "control version", edit: func(v map[string]any) { v["qemu_version"] = "bad\tversion" }, want: "malformed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			copyValue := make(map[string]any, len(valid))
			for key, value := range valid {
				copyValue[key] = value
			}
			test.edit(copyValue)
			candidate, err := json.Marshal(copyValue)
			if err != nil {
				t.Fatal(err)
			}
			_, err = ParseHardwareManifest(candidate)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Parse error = %v, want containing %q", err, test.want)
			}
		})
	}
	if _, err := ParseHardwareManifest(append(encoded, []byte("{}")...)); err == nil {
		t.Fatal("trailing JSON was accepted")
	}
	if _, err := ParseHardwareManifest(make([]byte, hardwareManifestMax+1)); err == nil {
		t.Fatal("oversized manifest was accepted")
	}
}

func TestQEMUVersionValidationUsesUTF8BytesAndPrintability(t *testing.T) {
	if !validQEMUVersion(strings.Repeat("é", 128)) {
		t.Fatal("256-byte printable version was rejected")
	}
	for _, invalid := range []string{
		"", strings.Repeat("é", 129), "line\nfeed", "tab\t", "non-breaking\u00a0space", "separator\u2028",
	} {
		if validQEMUVersion(invalid) {
			t.Fatalf("invalid version %q was accepted", invalid)
		}
	}
}

func TestDiscoverQEMUVersion(t *testing.T) {
	executable := writeExecutable(t, "#!/bin/sh\nprintf 'QEMU emulator version λ<&>\\nignored\\n' >&2\n")
	version, err := DiscoverQEMUVersion(context.Background(), executable)
	if err != nil {
		t.Fatal(err)
	}
	if version != "QEMU emulator version λ<&>" {
		t.Fatalf("version = %q", version)
	}
}

func TestDiscoverQEMUVersionFailuresAndCancellation(t *testing.T) {
	tests := []struct {
		name   string
		script string
		want   string
	}{
		{name: "empty", script: "#!/bin/sh\nexit 0\n", want: "output is empty"},
		{name: "nonzero", script: "#!/bin/sh\nexit 7\n", want: "could not determine"},
		{name: "invalid", script: "#!/bin/sh\nprintf 'bad\\tversion\\n'\n", want: "output is invalid"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := DiscoverQEMUVersion(context.Background(), writeExecutable(t, test.script))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Discover error = %v, want containing %q", err, test.want)
			}
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := DiscoverQEMUVersion(ctx, writeExecutable(t, "#!/bin/sh\nexec sleep 2\n"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canceled discovery error = %v, want deadline exceeded", err)
	}
}

func TestBoundedQEMUVersionOutput(t *testing.T) {
	output := &boundedOutput{limit: 4}
	if written, err := output.Write([]byte("abcdef")); err != nil || written != 6 {
		t.Fatalf("Write = %d, %v", written, err)
	}
	if string(output.bytes) != "abcd" || !output.overflow {
		t.Fatalf("bounded output = %q overflow=%v", output.bytes, output.overflow)
	}
}

func FuzzParseHardwareManifest(f *testing.F) {
	manifest, err := TestHardwareProfile.Manifest("QEMU emulator version fuzz")
	if err != nil {
		f.Fatal(err)
	}
	seed, err := EncodeHardwareManifest(manifest)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(seed)
	f.Add([]byte(`{"schema_version":1}`))
	f.Fuzz(func(t *testing.T, encoded []byte) {
		parsed, err := ParseHardwareManifest(encoded)
		if err != nil {
			return
		}
		if err := ValidateHardwareManifest(parsed); err != nil {
			t.Fatalf("successful parse did not validate: %v", err)
		}
		canonical, err := EncodeHardwareManifest(parsed)
		if err != nil {
			t.Fatalf("successful parse did not encode: %v", err)
		}
		if _, err := ParseHardwareManifest(canonical); err != nil {
			t.Fatalf("canonical encoding did not parse: %v", err)
		}
	})
}

func indentJSONStrings(t *testing.T, values []string) []string {
	t.Helper()
	encoded := make([]string, len(values))
	for index, value := range values {
		item, err := compactASCIIJSON(value)
		if err != nil {
			t.Fatal(err)
		}
		encoded[index] = "    " + item
	}
	return encoded
}

func writeExecutable(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-qemu")
	if err := os.WriteFile(path, []byte(contents), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
