package experiment

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"codexos/internal/guest"
	"codexos/internal/provenance"
	"codexos/internal/qemu"
)

// CompletedArchive is the trusted input required to publish one completed
// generation archive.  Artifact bytes are copied into the archive; nil byte
// slices are therefore valid zero-length files, except SourceSnapshot which
// must contain a valid source-snapshot encoding.
type CompletedArchive struct {
	Generation       uint64
	ParentGeneration *uint64
	Transition       string
	Hardware         qemu.HardwareManifest
	BootISO          []byte
	Handoff          string
	SourceSnapshot   []byte
	KernelELF        []byte
	SuccessorISO     []byte
	QEMUStdout       []byte
	QEMUStderr       []byte
}

// AbortedArchive is the trusted input required to publish one aborted
// generation archive.  LatestSuccess is optional; when present both files
// are persisted and validated using the Python forensic-evidence shape.
type AbortedArchive struct {
	Generation       uint64
	ParentGeneration *uint64
	Transition       string
	Hardware         qemu.HardwareManifest
	BootISO          []byte
	QEMUStdout       []byte
	QEMUStderr       []byte
	LatestSuccess    *AbortedSuccessEvidence
}

// AbortedSuccessEvidence is the optional latest-success pair retained in an
// aborted archive.  Manifest must be the exact JSON identity for Snapshot.
type AbortedSuccessEvidence struct {
	Manifest []byte
	Snapshot []byte
}

type completedArchiveFiles struct {
	Generation       uint64
	ParentGeneration *uint64
	Transition       string
	Hardware         qemu.HardwareManifest
	BootISO          string
	Handoff          string
	SourceSnapshot   []byte
	KernelELF        string
	SuccessorISO     string
	KernelIdentity   provenance.FileIdentity
	ISOIdentity      provenance.FileIdentity
	QEMUStdout       string
	QEMUStderr       string
}

type abortedArchiveFiles struct {
	Generation       uint64
	ParentGeneration *uint64
	Transition       string
	Hardware         qemu.HardwareManifest
	BootISO          string
	QEMUStdout       string
	QEMUStderr       string
	LatestSuccess    *AbortedSuccessEvidence
}

// WriteCompletedArchive validates and durably publishes one immutable
// completed archive.  It is intentionally independent of a live run so that
// tests and future runtime owners can stage the archive after their own
// candidate validation.
func WriteCompletedArchive(runDirectory string, input CompletedArchive) (ArchivedGeneration, error) {
	if err := validateArchiveMetadataInput(input.Generation, input.ParentGeneration, input.Transition); err != nil {
		return ArchivedGeneration{}, err
	}
	if err := qemu.ValidateHardwareManifest(input.Hardware); err != nil {
		return ArchivedGeneration{}, &GenerationRuntimeError{Reason: "generation hardware manifest is malformed", Err: err}
	}
	if !utf8.ValidString(input.Handoff) {
		return ArchivedGeneration{}, &GenerationRuntimeError{Reason: "generation handoff is not valid UTF-8"}
	}
	if _, err := guest.DecodeSourceSnapshot(input.SourceSnapshot); err != nil {
		return ArchivedGeneration{}, err
	}
	return publishArchive(runDirectory, input.Generation, func(staging string) error {
		if err := makeArchiveLayout(staging, true); err != nil {
			return err
		}
		if err := writeArchiveMetadata(staging, input.Generation, "completed", input.ParentGeneration, input.Transition); err != nil {
			return err
		}
		hardware, err := qemu.EncodeHardwareManifest(input.Hardware)
		if err != nil {
			return &GenerationRuntimeError{Reason: "generation hardware manifest is malformed", Err: err}
		}
		if err := writeArchiveFile(staging, hardwareManifestName, hardware); err != nil {
			return err
		}
		if err := writeArchiveFile(staging, handoffName, []byte(input.Handoff)); err != nil {
			return err
		}
		if err := writeArchiveFile(staging, sourceSnapshotName, input.SourceSnapshot); err != nil {
			return err
		}
		if err := materializeSnapshot(input.SourceSnapshot, filepath.Join(staging, sourceName)); err != nil {
			return err
		}
		if err := writeArchiveFile(staging, filepath.Join(successorName, "kernel.elf"), input.KernelELF); err != nil {
			return err
		}
		if err := writeArchiveFile(staging, filepath.Join(successorName, "codexos.iso"), input.SuccessorISO); err != nil {
			return err
		}
		if err := writeArchiveFile(staging, filepath.Join(archiveBootName, "codexos.iso"), input.BootISO); err != nil {
			return err
		}
		if err := writeArchiveFile(staging, stdoutName, input.QEMUStdout); err != nil {
			return err
		}
		return writeArchiveFile(staging, stderrName, input.QEMUStderr)
	})
}

// writeCompletedArchiveFiles is the live-runtime archive path. Large boot,
// successor, and log files are streamed into the staging tree instead of
// being retained together in memory.
func writeCompletedArchiveFiles(runDirectory string, input completedArchiveFiles) (ArchivedGeneration, error) {
	if err := validateArchiveMetadataInput(input.Generation, input.ParentGeneration, input.Transition); err != nil {
		return ArchivedGeneration{}, err
	}
	if err := qemu.ValidateHardwareManifest(input.Hardware); err != nil {
		return ArchivedGeneration{}, &GenerationRuntimeError{Reason: "generation hardware manifest is malformed", Err: err}
	}
	if !utf8.ValidString(input.Handoff) {
		return ArchivedGeneration{}, &GenerationRuntimeError{Reason: "generation handoff is not valid UTF-8"}
	}
	if _, err := guest.DecodeSourceSnapshot(input.SourceSnapshot); err != nil {
		return ArchivedGeneration{}, err
	}
	return publishArchive(runDirectory, input.Generation, func(staging string) error {
		if err := makeArchiveLayout(staging, true); err != nil {
			return err
		}
		if err := writeArchiveMetadata(staging, input.Generation, "completed", input.ParentGeneration, input.Transition); err != nil {
			return err
		}
		hardware, err := qemu.EncodeHardwareManifest(input.Hardware)
		if err != nil {
			return &GenerationRuntimeError{Reason: "generation hardware manifest is malformed", Err: err}
		}
		for relative, contents := range map[string][]byte{
			hardwareManifestName: hardware,
			handoffName:          []byte(input.Handoff),
			sourceSnapshotName:   input.SourceSnapshot,
		} {
			if err := writeArchiveFile(staging, relative, contents); err != nil {
				return err
			}
		}
		if err := materializeSnapshot(input.SourceSnapshot, filepath.Join(staging, sourceName)); err != nil {
			return err
		}
		for destination, source := range map[string]string{
			filepath.Join(archiveBootName, "codexos.iso"): input.BootISO,
			stdoutName: input.QEMUStdout,
			stderrName: input.QEMUStderr,
		} {
			if err := copyArchiveFile(staging, destination, source); err != nil {
				return err
			}
		}
		if err := copyArchiveFileVerified(staging, filepath.Join(successorName, "kernel.elf"), input.KernelELF, &input.KernelIdentity); err != nil {
			return err
		}
		if err := copyArchiveFileVerified(staging, filepath.Join(successorName, "codexos.iso"), input.SuccessorISO, &input.ISOIdentity); err != nil {
			return err
		}
		return nil
	})
}

// WriteAbortedArchive validates and durably publishes one immutable aborted
// archive.  The optional forensic pair is checked before publication.
func WriteAbortedArchive(runDirectory string, input AbortedArchive) (ArchivedGeneration, error) {
	if err := validateArchiveMetadataInput(input.Generation, input.ParentGeneration, input.Transition); err != nil {
		return ArchivedGeneration{}, err
	}
	if err := qemu.ValidateHardwareManifest(input.Hardware); err != nil {
		return ArchivedGeneration{}, &GenerationRuntimeError{Reason: "generation hardware manifest is malformed", Err: err}
	}
	if input.LatestSuccess != nil {
		if err := validateAbortedSuccessEvidenceBytes(input.LatestSuccess.Manifest, input.LatestSuccess.Snapshot, input.Generation); err != nil {
			return ArchivedGeneration{}, err
		}
	}
	return publishArchive(runDirectory, input.Generation, func(staging string) error {
		if err := makeArchiveLayout(staging, false); err != nil {
			return err
		}
		if err := writeArchiveMetadata(staging, input.Generation, "aborted", input.ParentGeneration, input.Transition); err != nil {
			return err
		}
		hardware, err := qemu.EncodeHardwareManifest(input.Hardware)
		if err != nil {
			return &GenerationRuntimeError{Reason: "generation hardware manifest is malformed", Err: err}
		}
		if err := writeArchiveFile(staging, hardwareManifestName, hardware); err != nil {
			return err
		}
		if err := writeArchiveFile(staging, abortedMarkerName, []byte(AbortMarker)); err != nil {
			return err
		}
		if err := writeArchiveFile(staging, filepath.Join(archiveBootName, "codexos.iso"), input.BootISO); err != nil {
			return err
		}
		if err := writeArchiveFile(staging, stdoutName, input.QEMUStdout); err != nil {
			return err
		}
		if err := writeArchiveFile(staging, stderrName, input.QEMUStderr); err != nil {
			return err
		}
		if input.LatestSuccess != nil {
			if err := writeArchiveFile(staging, latestSuccessManifestName, input.LatestSuccess.Manifest); err != nil {
				return err
			}
			if err := writeArchiveFile(staging, latestSuccessSnapshotName, input.LatestSuccess.Snapshot); err != nil {
				return err
			}
		}
		return nil
	})
}

func writeAbortedArchiveFiles(runDirectory string, input abortedArchiveFiles) (ArchivedGeneration, error) {
	if err := validateArchiveMetadataInput(input.Generation, input.ParentGeneration, input.Transition); err != nil {
		return ArchivedGeneration{}, err
	}
	if err := qemu.ValidateHardwareManifest(input.Hardware); err != nil {
		return ArchivedGeneration{}, &GenerationRuntimeError{Reason: "generation hardware manifest is malformed", Err: err}
	}
	if input.LatestSuccess != nil {
		if err := validateAbortedSuccessEvidenceBytes(input.LatestSuccess.Manifest, input.LatestSuccess.Snapshot, input.Generation); err != nil {
			return ArchivedGeneration{}, err
		}
	}
	return publishArchive(runDirectory, input.Generation, func(staging string) error {
		if err := makeArchiveLayout(staging, false); err != nil {
			return err
		}
		if err := writeArchiveMetadata(staging, input.Generation, "aborted", input.ParentGeneration, input.Transition); err != nil {
			return err
		}
		hardware, err := qemu.EncodeHardwareManifest(input.Hardware)
		if err != nil {
			return &GenerationRuntimeError{Reason: "generation hardware manifest is malformed", Err: err}
		}
		if err := writeArchiveFile(staging, hardwareManifestName, hardware); err != nil {
			return err
		}
		if err := writeArchiveFile(staging, abortedMarkerName, []byte(AbortMarker)); err != nil {
			return err
		}
		for destination, source := range map[string]string{
			filepath.Join(archiveBootName, "codexos.iso"): input.BootISO,
			stdoutName: input.QEMUStdout,
			stderrName: input.QEMUStderr,
		} {
			if err := copyArchiveFile(staging, destination, source); err != nil {
				return err
			}
		}
		if input.LatestSuccess != nil {
			if err := writeArchiveFile(staging, latestSuccessManifestName, input.LatestSuccess.Manifest); err != nil {
				return err
			}
			if err := writeArchiveFile(staging, latestSuccessSnapshotName, input.LatestSuccess.Snapshot); err != nil {
				return err
			}
		}
		return nil
	})
}

func publishArchive(runDirectory string, generation uint64, populate func(string) error) (ArchivedGeneration, error) {
	run, err := resolveRunDirectory(runDirectory, true)
	if err != nil {
		return ArchivedGeneration{}, &GenerationRuntimeError{Reason: "could not resolve run directory", Err: err}
	}
	final := filepath.Join(run, generationName(generation))
	if pathExists(final) {
		return ArchivedGeneration{}, &GenerationRuntimeError{Reason: "generation archive already exists: " + final}
	}
	staging, err := os.MkdirTemp(run, fmt.Sprintf(".generation-%04d-archive-", generation))
	if err != nil {
		return ArchivedGeneration{}, &GenerationRuntimeError{Reason: "could not stage generation archive", Err: err}
	}
	removeStaging := true
	defer func() {
		if removeStaging {
			_ = os.RemoveAll(staging)
		}
	}()
	if err := populate(staging); err != nil {
		return ArchivedGeneration{}, err
	}
	if pathExists(final) {
		return ArchivedGeneration{}, &GenerationRuntimeError{Reason: "generation archive already exists: " + final}
	}
	if err := syncArchiveTree(staging); err != nil {
		return ArchivedGeneration{}, err
	}
	if err := os.Rename(staging, final); err != nil {
		return ArchivedGeneration{}, &GenerationRuntimeError{Reason: "could not publish generation archive", Err: err}
	}
	removeStaging = false
	if err := syncDirectory(run); err != nil {
		return ArchivedGeneration{}, err
	}
	archive, err := readArchivedGeneration(run, generation)
	if err != nil {
		return ArchivedGeneration{}, generationArchiveError(generation, err)
	}
	return archive, nil
}

func validateArchiveMetadataInput(generation uint64, parent *uint64, transition string) error {
	if generation == 0 {
		if parent != nil || transition != "initial" {
			return &GenerationRuntimeError{Reason: "generation archive metadata is malformed"}
		}
		return nil
	}
	if parent == nil || *parent >= generation || (transition != "successor" && transition != "rollback") {
		return &GenerationRuntimeError{Reason: "generation archive metadata is malformed"}
	}
	return nil
}

func makeArchiveLayout(staging string, completed bool) error {
	if err := os.Mkdir(filepath.Join(staging, archiveBootName), 0o755); err != nil {
		return err
	}
	if completed {
		if err := os.Mkdir(filepath.Join(staging, sourceName), 0o755); err != nil {
			return err
		}
		if err := os.Mkdir(filepath.Join(staging, successorName), 0o755); err != nil {
			return err
		}
	}
	return nil
}

func writeArchiveMetadata(staging string, generation uint64, outcome string, parent *uint64, transition string) error {
	// Field order is the same lexical order emitted by Python's sort_keys=True.
	value := struct {
		Generation       uint64  `json:"generation"`
		Outcome          string  `json:"outcome"`
		ParentGeneration *uint64 `json:"parent_generation"`
		Transition       string  `json:"transition"`
	}{generation, outcome, parent, transition}
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return &GenerationRuntimeError{Reason: "could not encode generation archive metadata", Err: err}
	}
	encoded = append(encoded, '\n')
	return writeArchiveFile(staging, archiveMetadataName, encoded)
}

func writeArchiveFile(staging, relative string, contents []byte) error {
	if filepath.IsAbs(relative) || relative == "." || stringsContainsDotDot(relative) {
		return &GenerationRuntimeError{Reason: "unsafe generation archive path: " + relative}
	}
	path := filepath.Join(staging, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return &GenerationRuntimeError{Reason: "could not create generation archive artifact", Err: err}
	}
	output, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return &GenerationRuntimeError{Reason: "could not persist generation archive artifact: " + relative, Err: err}
	}
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(path)
		}
	}()
	if written, writeErr := output.Write(contents); writeErr != nil || written != len(contents) {
		_ = output.Close()
		if writeErr == nil {
			writeErr = io.ErrShortWrite
		}
		return &GenerationRuntimeError{Reason: "could not persist generation archive artifact: " + relative, Err: writeErr}
	}
	if err := output.Sync(); err != nil {
		_ = output.Close()
		return &GenerationRuntimeError{Reason: "could not persist generation archive artifact: " + relative, Err: err}
	}
	if err := output.Close(); err != nil {
		return &GenerationRuntimeError{Reason: "could not persist generation archive artifact: " + relative, Err: err}
	}
	remove = false
	return nil
}

func copyArchiveFile(staging, relative, source string) error {
	return copyArchiveFileVerified(staging, relative, source, nil)
}

func copyArchiveFileVerified(staging, relative, source string, expected *provenance.FileIdentity) error {
	if filepath.IsAbs(relative) || stringsContainsDotDot(relative) {
		return &GenerationRuntimeError{Reason: "unsafe generation archive path: " + relative}
	}
	input, err := openRegularNoFollow(source)
	if err != nil {
		return &GenerationRuntimeError{Reason: "could not open generation archive source: " + source, Err: err}
	}
	defer input.Close()
	destination := filepath.Join(staging, relative)
	if !pathWithin(staging, destination) {
		return &GenerationRuntimeError{Reason: "unsafe generation archive path: " + relative}
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return &GenerationRuntimeError{Reason: "could not create generation archive artifact", Err: err}
	}
	writer := io.Writer(output)
	var digest hash.Hash
	if expected != nil {
		digest = sha256.New()
		writer = io.MultiWriter(output, digest)
	}
	written, copyErr := io.CopyBuffer(writer, input, make([]byte, 1024*1024))
	if copyErr == nil && expected != nil &&
		(uint64(written) != expected.Size || hex.EncodeToString(digest.Sum(nil)) != expected.SHA256) {
		copyErr = &GenerationRuntimeError{Reason: "validated successor artifact changed before archival: " + source}
	}
	if copyErr == nil {
		copyErr = output.Sync()
	}
	closeErr := output.Close()
	if copyErr != nil {
		return &GenerationRuntimeError{Reason: "could not persist generation archive artifact: " + relative, Err: copyErr}
	}
	if closeErr != nil {
		return &GenerationRuntimeError{Reason: "could not persist generation archive artifact: " + relative, Err: closeErr}
	}
	return nil
}

func materializeSnapshot(snapshot []byte, destination string) error {
	files, err := guest.DecodeSourceSnapshot(snapshot)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return err
	}
	root, err := filepath.Abs(destination)
	if err != nil {
		return err
	}
	for _, file := range files {
		output := filepath.Join(root, filepath.FromSlash(file.Path))
		resolved, err := filepath.Abs(output)
		if err != nil || !pathWithin(root, resolved) {
			return fmt.Errorf("source path escapes archive: %q", file.Path)
		}
		if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
			return err
		}
		relative, err := filepath.Rel(root, output)
		if err != nil {
			return err
		}
		if err := writeArchiveFile(root, filepath.ToSlash(relative), file.Content); err != nil {
			return err
		}
	}
	return nil
}

func pathWithin(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !stringsContainsDotDot(relative)
}

func stringsContainsDotDot(path string) bool {
	for _, component := range strings.Split(filepath.ToSlash(path), "/") {
		if component == ".." {
			return true
		}
	}
	return false
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return &GenerationRuntimeError{Reason: "could not sync generation archive directory", Err: err}
	}
	err = directory.Sync()
	closeErr := directory.Close()
	if err != nil {
		return &GenerationRuntimeError{Reason: "could not sync generation archive directory", Err: err}
	}
	if closeErr != nil {
		return &GenerationRuntimeError{Reason: "could not close generation archive directory", Err: closeErr}
	}
	return nil
}

func syncArchiveTree(root string) error {
	directories := make([]string, 0, 4)
	if err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			directories = append(directories, path)
		}
		return nil
	}); err != nil {
		return &GenerationRuntimeError{Reason: "could not inspect generation archive directories", Err: err}
	}
	sort.Slice(directories, func(i, j int) bool {
		return strings.Count(filepath.ToSlash(directories[i]), "/") > strings.Count(filepath.ToSlash(directories[j]), "/")
	})
	for _, directory := range directories {
		if err := syncDirectory(directory); err != nil {
			return err
		}
	}
	return nil
}

func validateAbortedSuccessEvidenceBytes(manifestBytes, snapshot []byte, generation uint64) error {
	temporary, err := os.MkdirTemp("", ".codexos-aborted-evidence-")
	if err != nil {
		return &GenerationRuntimeError{Reason: "could not validate aborted generation forensic evidence", Err: err}
	}
	defer os.RemoveAll(temporary)
	manifestPath := filepath.Join(temporary, latestSuccessManifestName)
	snapshotPath := filepath.Join(temporary, latestSuccessSnapshotName)
	if err := os.WriteFile(manifestPath, manifestBytes, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(snapshotPath, snapshot, 0o600); err != nil {
		return err
	}
	return validateAbortedSuccessEvidence(manifestPath, snapshotPath, generation)
}
