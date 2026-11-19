package tools

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"minimalharness/internal/fileguard"
	"os"
	"strings"
	"unicode/utf8"
)

const maxReadSnapshotBytes = 8 << 20
const maxReadScanBytes = 32 << 20
const maxDirectoryEntries = 100000
const maxDirectoryOffset = 10000

type readContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r readContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func (e *Executor) Read(a ReadArgs) (string, error) { return e.ReadContext(context.Background(), a) }

// Reads have bounded scan and snapshot costs, independent of an enormous file
// or directory. Large-file reads never authorize an edit against an unseen hash.
func (e *Executor) ReadContext(ctx context.Context, a ReadArgs) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := e.editMu.LockContext(ctx); err != nil {
		return "", err
	}
	defer e.editMu.Unlock()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	base := e.Root
	if dir, ok := e.outputDirBase(a.Path); ok {
		base = dir
	}
	f, err := fileguard.Open(base, a.Path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return readDirectory(ctx, f, a)
	}
	if base == e.Root {
		e.invalidateSnapshot(a.Path)
		if info.Size() <= maxReadSnapshotBytes {
			h := sha256.New()
			n, err := io.Copy(h, io.LimitReader(readContextReader{ctx, f}, maxReadSnapshotBytes+1))
			if err != nil {
				return "", err
			}
			if n <= maxReadSnapshotBytes {
				var digest [32]byte
				copy(digest[:], h.Sum(nil))
				e.rememberSnapshot(a.Path, digest)
			}
			if _, err := f.Seek(0, 0); err != nil {
				return "", err
			}
		}
	}
	start := max(a.Offset-1, 0)
	remaining := int64(maxReadScanBytes)
	lr := &io.LimitedReader{R: readContextReader{ctx, f}, N: remaining + 1}
	reader := bufio.NewReader(lr)
	var out strings.Builder
	for line := 0; ; line++ {
		var data []byte
		eof, clipped := false, false
		for {
			part, err := reader.ReadSlice('\n')
			remaining -= int64(len(part))
			if remaining < 0 {
				out.WriteString("... [read scan limit reached; narrow the file with a search or shell command]\n")
				return out.String(), nil
			}
			if line >= start {
				take := min(len(part), max(0, MaxReadBytes-out.Len()-len(data)))
				clipped = clipped || take < len(part)
				data = append(data, part[:take]...)
			}
			if clipped {
				break
			}
			if err == bufio.ErrBufferFull {
				continue
			}
			if err != nil && err != io.EOF {
				return "", err
			}
			eof = err == io.EOF
			break
		}
		if line >= start {
			for i := 0; clipped && i < utf8.UTFMax-1 && !utf8.Valid(data) && len(data) > 0; i++ {
				data = data[:len(data)-1]
			}
			if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
				return "", fmt.Errorf("read: binary or non-UTF-8 file; use a binary inspection command instead")
			}
			for _, b := range data {
				if b < 0x20 && b != '\n' && b != '\r' && b != '\t' {
					return "", fmt.Errorf("read: binary control bytes in file; use a binary inspection command instead")
				}
			}
			fmt.Fprintf(&out, "%d\t%s\n", line+1, strings.TrimSuffix(string(data), "\n"))
			if clipped {
				out.WriteString("... [line truncated at read byte limit]\n")
				return out.String(), nil
			}
			if (a.Limit > 0 && line-start+1 >= a.Limit) || eof {
				return out.String(), nil
			}
			if out.Len() >= MaxReadBytes {
				out.WriteString("... [truncated: use offset to continue]\n")
				return out.String(), nil
			}
		}
		if eof {
			return out.String(), nil
		}
	}
}

func readDirectory(ctx context.Context, f *os.File, a ReadArgs) (string, error) {
	offset := max(a.Offset-1, 0)
	if offset > maxDirectoryOffset {
		return "", &InputError{Err: fmt.Errorf("directory offset exceeds %d; narrow the path", maxDirectoryOffset+1)}
	}
	limit := a.Limit
	if limit <= 0 {
		limit = DefaultGlobLimit
	}
	limit = min(limit, MaxGlobLimit)
	window := pathWindow{limit: offset + limit}
	incomplete := false
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		entries, err := f.ReadDir(256)
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() {
				name += "/"
			}
			window.add(name)
		}
		if err != nil && err != io.EOF {
			return "", err
		}
		if err == io.EOF {
			break
		}
		if window.total >= maxDirectoryEntries {
			incomplete = true
			break
		}
	}
	paths := window.sorted()
	paths = paths[min(offset, len(paths)):]
	for i := range paths {
		paths[i] = displayPath(paths[i])
	}
	out := "directory " + displayPath(a.Path) + ":\n" + strings.Join(paths, "\n")
	if incomplete {
		out += "\n... [truncated: directory scan limit reached; narrow the path]"
	} else if window.total > offset+len(paths) {
		out += "\n... [truncated: use offset to continue]"
	}
	return out, nil
}
