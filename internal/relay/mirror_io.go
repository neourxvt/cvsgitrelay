package relay

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
)

// Compare in fixed-size chunks: an RCS archive may contain an entire file's
// history, so reading even one complete archive would defeat bounded memory.
func mirrorFilesEqual(ctx context.Context, first, second string) (bool, error) {
	left, err := os.Open(first)
	if err != nil {
		return false, err
	}
	defer left.Close()
	right, err := os.Open(second)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer right.Close()
	leftInfo, err := left.Stat()
	if err != nil {
		return false, err
	}
	rightInfo, err := right.Stat()
	if err != nil {
		return false, err
	}
	if leftInfo.Size() != rightInfo.Size() {
		return false, nil
	}
	var a, b [32 << 10]byte
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		n, leftErr := io.ReadFull(left, a[:])
		m, rightErr := io.ReadFull(right, b[:])
		if leftErr != nil && leftErr != io.EOF && leftErr != io.ErrUnexpectedEOF {
			return false, leftErr
		}
		if rightErr != nil && rightErr != io.EOF && rightErr != io.ErrUnexpectedEOF {
			return false, rightErr
		}
		if n != m || !bytes.Equal(a[:n], b[:m]) {
			return false, nil
		}
		if leftErr != nil || rightErr != nil {
			return leftErr == rightErr, nil
		}
	}
}

// Stream into a sibling temporary file, then sync and atomically replace the
// target. An interrupted write never exposes a partially written archive.
func mirrorWriteStream(ctx context.Context, path string, write func(io.Writer) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	err = write(temporary)
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		err = temporary.Sync()
	}
	closeErr := temporary.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err := os.Rename(temporary.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// Publication copies archive bytes through a fixed-size buffer. Check
// cancellation between chunks as well as before the atomic replacement.
func mirrorCopyFile(ctx context.Context, source, target string) error {
	return mirrorWriteStream(ctx, target, func(out io.Writer) error {
		input, err := os.Open(source)
		if err != nil {
			return err
		}
		defer input.Close()
		var buffer [32 << 10]byte
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			n, readErr := input.Read(buffer[:])
			if n > 0 {
				if _, err := out.Write(buffer[:n]); err != nil {
					return err
				}
			}
			if readErr == io.EOF {
				return nil
			}
			if readErr != nil {
				return readErr
			}
		}
	})
}
