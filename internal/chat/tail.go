package chat

import (
	"bytes"
	"io"
	"os"
)

const (
	initialTail = 8 << 20 // a first read starts at most this far from the end
	maxLine     = 4 << 20 // longer lines (inline base64, huge outputs) are skipped
	maxChunk    = 8 << 20 // bytes read per poll
	keep        = 400     // messages kept per transcript
)

// Tailer follows a growing JSONL file: it reads only the appended bytes, keeps a partial
// last line for the next poll and starts over when the file is truncated or replaced.
type Tailer struct {
	B *Builder

	// Prefix is history that comes before the file, oldest first; it is parsed on every
	// (re)start of the read, before the file's own lines.
	Prefix []Prefix

	path     string
	parser   Parser
	info     os.FileInfo
	off      int64
	pending  []byte
	skipping bool  // dropping the rest of an oversized (or cut) line
	window   int64 // a first read starts at most this far from the end (default initialTail)
}

// Prefix is the first End bytes of another transcript, which ends on a line boundary.
type Prefix struct {
	Path string
	End  int64
}

func NewTailer(path string, p Parser, b *Builder) *Tailer {
	if b == nil {
		b = NewBuilder(keep)
	}
	return &Tailer{B: b, path: path, parser: p}
}

// Poll reads what was appended since the last call. reset is true when the builder was
// rebuilt from the file (first read, truncation, replacement): send a snapshot, not a delta.
func (t *Tailer) Poll() (reset bool, err error) {
	f, err := os.Open(t.path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return false, err
	}
	size := fi.Size()
	switch {
	case t.info == nil || !os.SameFile(t.info, fi) || size < t.off:
		reset = true
		t.B.Reset()
		t.pending, t.skipping, t.off = nil, false, 0
		win := t.window
		if win <= 0 {
			win = initialTail
		}
		if size > win {
			t.off, t.skipping = size-win, true
		}
		t.replay(win)
	case size == t.off:
		t.info = fi
		return false, nil
	}
	t.info = fi

	n := size - t.off
	if n > maxChunk {
		n = maxChunk
	}
	buf := make([]byte, n)
	read, err := f.ReadAt(buf, t.off)
	if err != nil && err != io.EOF {
		return reset, err
	}
	t.off += int64(read)
	t.consume(buf[:read])
	return reset, nil
}

// replay parses the prefixes, each from at most win bytes before its end. A prefix that
// cannot be read is skipped: the chat then starts at the file itself.
func (t *Tailer) replay(win int64) {
	for _, p := range t.Prefix {
		f, err := os.Open(p.Path)
		if err != nil {
			continue
		}
		start := max(p.End-win, 0)
		buf := make([]byte, p.End-start)
		n, _ := f.ReadAt(buf, start)
		f.Close()
		buf = buf[:n]
		if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
			buf = buf[:i+1]
		} else {
			continue
		}
		if start > 0 { // the first line is cut
			if i := bytes.IndexByte(buf, '\n'); i >= 0 {
				buf = buf[i+1:]
			}
		}
		for _, line := range bytes.Split(buf, []byte{'\n'}) {
			if len(line) > 0 && len(line) <= maxLine {
				t.parser.Line(line, t.B)
			}
		}
	}
}

func (t *Tailer) Builder() *Builder { return t.B }

func (t *Tailer) consume(data []byte) {
	t.pending = append(t.pending, data...)
	for {
		i := bytes.IndexByte(t.pending, '\n')
		if i < 0 {
			break
		}
		line := t.pending[:i]
		t.pending = t.pending[i+1:]
		if t.skipping {
			t.skipping = false
			continue
		}
		if len(line) > 0 && len(line) <= maxLine {
			t.parser.Line(line, t.B)
		}
	}
	if len(t.pending) > maxLine {
		t.pending, t.skipping = nil, true
	}
}
