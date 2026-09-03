package guest

import (
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
	if ctx == nil || invoke == nil {
		return nil, errors.New("source snapshot capture is unavailable")
	}
	listed, err := invoke(ctx, "list", nil)
	if err != nil {
		return nil, err
	}
	if listed.Status != 0 {
		return nil, fmt.Errorf("source list failed with status %d", listed.Status)
	}
	if !utf8.Valid(listed.Output) {
		return nil, errors.New("source list is not valid UTF-8")
	}
	text := strings.TrimSuffix(string(listed.Output), "\n")
	paths := []string{}
	if text != "" {
		paths = strings.Split(text, "\n")
	}
	sort.Strings(paths)
	files := make([]SnapshotFile, 0, len(paths))
	total := 0
	for _, path := range paths {
		if path == "" {
			return nil, errors.New("source list contains an empty path")
		}
		requested := maxSnapshotContent - total + 1
		result, readErr := invoke(ctx, "read", [][]byte{
			[]byte(path), []byte("0"), []byte(strconv.Itoa(requested)),
		})
		if readErr != nil {
			return nil, readErr
		}
		if result.Status != 0 {
			return nil, fmt.Errorf("source read for %q failed with status %d", path, result.Status)
		}
		if len(result.Output) > maxSnapshotContent-total {
			return nil, errors.New("source snapshot content exceeds 64 KiB")
		}
		total += len(result.Output)
		files = append(files, SnapshotFile{Path: path, Content: append([]byte(nil), result.Output...)})
	}
	return EncodeSourceSnapshot(files)
}
