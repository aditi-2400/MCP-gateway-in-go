package jsonrpc

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// DefaultMaxMessageSize is a suggested limit for one message, in bytes.
// Callers pass the limit to NewReader so it can come from config.
const DefaultMaxMessageSize = 4 << 20 // 4 MiB

// ErrMessageTooLarge is returned by Reader.Read when a line is longer than
// the limit. The line is discarded and the next Read continues after it.
var ErrMessageTooLarge = errors.New("jsonrpc: message exceeds maximum size")

// Reader reads newline-delimited JSON-RPC messages, as used by the MCP stdio
// transport. It is not safe for concurrent use: one goroutine should own it.
type Reader struct {
	br      *bufio.Reader
	maxSize int
	line    []byte // reused between reads
}

// NewReader returns a Reader that rejects messages longer than maxSize bytes.
func NewReader(r io.Reader, maxSize int) *Reader {
	return &Reader{br: bufio.NewReader(r), maxSize: maxSize}
}

// Read returns the next message. Errors fall into two groups:
//   - A bad line (*Error from Parse, or ErrMessageTooLarge). The stream is
//     still usable and the caller can keep reading.
//   - Anything else, including io.EOF. The stream is finished.
func (r *Reader) Read() (*Message, error) {
	for {
		line, err := r.readLine()
		if err != nil {
			return nil, err
		}
		if len(line) == 0 {
			continue
		}
		// Parse copies what it keeps, so reusing r.line afterwards is safe.
		return Parse(line)
	}
}

// readLine returns the next line without its line ending. The returned slice
// is only valid until the next call. Memory use stays bounded by maxSize even
// when the incoming line is much longer.
func (r *Reader) readLine() ([]byte, error) {
	r.line = r.line[:0]
	tooLarge := false
	for {
		// ReadSlice returns at most one bufio buffer's worth of data, so a
		// long line arrives in several chunks ending with ErrBufferFull.
		chunk, err := r.br.ReadSlice('\n')
		if !tooLarge {
			// +2 leaves room for a trailing "\r\n", which is trimmed below.
			if len(r.line)+len(chunk) > r.maxSize+2 {
				tooLarge = true
				r.line = r.line[:0]
			} else {
				r.line = append(r.line, chunk...)
			}
		}

		if err == nil {
			break
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if errors.Is(err, io.EOF) {
			if len(r.line) == 0 && !tooLarge {
				return nil, io.EOF
			}
			break // last line had no trailing newline
		}
		return nil, err
	}

	line := bytes.TrimSuffix(r.line, []byte("\n"))
	line = bytes.TrimSuffix(line, []byte("\r"))
	if tooLarge || len(line) > r.maxSize {
		return nil, ErrMessageTooLarge
	}
	return line, nil
}

// Writer writes JSON-RPC messages one per line. It is safe for concurrent
// use, so several goroutines can send responses on the same stream.
type Writer struct {
	mu sync.Mutex
	w  io.Writer
}

// NewWriter returns a Writer that writes to w.
func NewWriter(w io.Writer) *Writer {
	return &Writer{w: w}
}

// Write encodes msg and writes it followed by a newline in a single call.
func (w *Writer) Write(msg *Message) error {
	// json.Marshal compacts raw params and results, so pretty-printed input
	// cannot put a newline inside the message.
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("jsonrpc: encode message: %w", err)
	}
	if bytes.IndexByte(data, '\n') >= 0 {
		return errors.New("jsonrpc: encoded message contains a newline")
	}
	data = append(data, '\n')

	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.w.Write(data); err != nil {
		return fmt.Errorf("jsonrpc: write message: %w", err)
	}
	return nil
}
