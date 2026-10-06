// Package tailread reads the bounded tail of an append-only JSONL file. It is
// the one place a provider's session file is read for status evidence, so no
// reader can scan a long transcript from the top.
package tailread

import (
	"bytes"
	"io"
	"os"
)

// Lines returns at most maxBytes from the end of path, trimmed to whole lines:
// when the window starts mid-file, the partial first line is dropped. complete
// is false when the file does not end in a newline, so the newest row may still
// be being written and the caller should defer judgement until it flushes.
func Lines(path string, maxBytes int64) (data []byte, complete bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	start := max(info.Size()-maxBytes, 0)
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil, false, err
	}
	data, err = io.ReadAll(io.LimitReader(f, maxBytes))
	if err != nil {
		return nil, false, err
	}
	if start > 0 {
		if n := bytes.IndexByte(data, '\n'); n >= 0 {
			data = data[n+1:]
		} else {
			data = nil
		}
	}
	return data, len(data) == 0 || data[len(data)-1] == '\n', nil
}
