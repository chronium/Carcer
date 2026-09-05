package guest

import (
	"codexos/internal/sourcecapacity"
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// CaptureSourceSnapshot obtains one canonical source snapshot through list and
// read while the caller owns whatever serialization boundary protects the
// mutable source. A successful short read establishes EOF.
func CaptureSourceSnapshot(ctx context.Context, invoke func(context.Context, string, [][]byte) (ToolResult, error)) ([]byte, error) {
	snapshot, err := CaptureCanonicalSourceSnapshot(ctx, invoke)
	if err != nil {
		return nil, err
	}
	return snapshot.Bytes(), nil
}

// CaptureCanonicalSourceSnapshot obtains and identifies the canonical source
// snapshot while the caller owns the mutable-source serialization boundary.
func CaptureCanonicalSourceSnapshot(ctx context.Context, invoke func(context.Context, string, [][]byte) (ToolResult, error)) (SourceSnapshot, error) {
	return CaptureCanonicalSourceSnapshotWithBudget(ctx, invoke, 0)
}

func CaptureCanonicalSourceSnapshotWithBudget(ctx context.Context, invoke func(context.Context, string, [][]byte) (ToolResult, error), budget sourcecapacity.Budget) (SourceSnapshot, error) {
	if err := budget.Validate(); err != nil {
		return SourceSnapshot{}, err
	}
	if ctx == nil || invoke == nil {
		return SourceSnapshot{}, errors.New("source snapshot capture is unavailable")
	}
	listed, err := invoke(ctx, "list", [][]byte{[]byte("seed/")})
	if err != nil {
		return SourceSnapshot{}, err
	}
	if listed.Status != 0 {
		return SourceSnapshot{}, fmt.Errorf("source list failed with status %d", listed.Status)
	}
	if !utf8.Valid(listed.Output) {
		return SourceSnapshot{}, errors.New("source list is not valid UTF-8")
	}
	text := strings.TrimSuffix(string(listed.Output), "\n")
	paths := []string{}
	if text != "" {
		paths = strings.Split(text, "\n")
	}
	if len(paths) > maxSnapshotFiles {
		return SourceSnapshot{}, errors.New("source snapshot contains more than 128 files")
	}
	sort.Strings(paths)
	files := make([]SnapshotFile, 0, len(paths))
	total := 0
	for _, path := range paths {
		if path == "" {
			return SourceSnapshot{}, errors.New("source list contains an empty path")
		}
		if !strings.HasPrefix(path, "seed/") {
			return SourceSnapshot{}, fmt.Errorf("source list contains path outside seed/ prefix: %q", path)
		}
		if err := ValidateSourcePath(path); err != nil {
			return SourceSnapshot{}, err
		}
		requested := budget.Bytes() - total + 1
		result, readErr := invoke(ctx, "read", [][]byte{
			[]byte(path), []byte("0"), []byte(strconv.Itoa(requested)),
		})
		if readErr != nil {
			return SourceSnapshot{}, readErr
		}
		if result.Status != 0 {
			return SourceSnapshot{}, fmt.Errorf("source read for %q failed with status %d", path, result.Status)
		}
		if len(result.Output) > budget.Bytes()-total {
			return SourceSnapshot{}, budget.Exceeded()
		}
		total += len(result.Output)
		files = append(files, SnapshotFile{Path: path, Content: append([]byte(nil), result.Output...)})
	}
	return NewCanonicalSourceSnapshotWithBudget(files, budget)
}
