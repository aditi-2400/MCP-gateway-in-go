package jsonrpc

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
)

const testMaxSize = 1024

func TestReaderReadsLines(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		wantMethods []string
	}{
		{
			name:        "newline separated",
			input:       "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"a\"}\n{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"b\"}\n",
			wantMethods: []string{"a", "b"},
		},
		{
			name:        "windows line endings",
			input:       "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"a\"}\r\n{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"b\"}\r\n",
			wantMethods: []string{"a", "b"},
		},
		{
			name:        "blank lines are skipped",
			input:       "\n\r\n{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"a\"}\n\n",
			wantMethods: []string{"a"},
		},
		{
			name:        "last line without trailing newline",
			input:       "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"a\"}\n{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"b\"}",
			wantMethods: []string{"a", "b"},
		},
		{
			name:        "empty stream",
			input:       "",
			wantMethods: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := NewReader(strings.NewReader(tt.input), testMaxSize)
			var got []string
			for {
				msg, err := r.Read()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("Read() error = %v", err)
				}
				got = append(got, msg.Method)
			}
			if strings.Join(got, ",") != strings.Join(tt.wantMethods, ",") {
				t.Errorf("methods = %v, want %v", got, tt.wantMethods)
			}
		})
	}
}

func TestReaderBadLineDoesNotStopStream(t *testing.T) {
	input := "not json\n{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"ok\"}\n"
	r := NewReader(strings.NewReader(input), testMaxSize)

	_, err := r.Read()
	var rpcErr *Error
	if !errors.As(err, &rpcErr) || rpcErr.Code != CodeParseError {
		t.Fatalf("first Read() error = %v, want *Error with code %d", err, CodeParseError)
	}

	msg, err := r.Read()
	if err != nil {
		t.Fatalf("second Read() error = %v, want nil", err)
	}
	if msg.Method != "ok" {
		t.Errorf("Method = %q, want %q", msg.Method, "ok")
	}
}

func TestReaderSizeLimit(t *testing.T) {
	// A message whose JSON is exactly n bytes long.
	messageOfSize := func(n int) string {
		base := `{"jsonrpc":"2.0","id":1,"method":"m","params":{"p":""}}`
		return strings.Replace(base, `"p":""`, `"p":"`+strings.Repeat("x", n-len(base))+`"`, 1)
	}
	next := "{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"next\"}\n"

	t.Run("exactly at the limit is accepted", func(t *testing.T) {
		line := messageOfSize(testMaxSize)
		r := NewReader(strings.NewReader(line+"\n"), testMaxSize)
		if _, err := r.Read(); err != nil {
			t.Fatalf("Read() error = %v, want nil", err)
		}
	})

	t.Run("one byte over the limit is rejected and skipped", func(t *testing.T) {
		line := messageOfSize(testMaxSize + 1)
		r := NewReader(strings.NewReader(line+"\n"+next), testMaxSize)
		if _, err := r.Read(); !errors.Is(err, ErrMessageTooLarge) {
			t.Fatalf("Read() error = %v, want ErrMessageTooLarge", err)
		}
		msg, err := r.Read()
		if err != nil {
			t.Fatalf("Read() after oversized line error = %v", err)
		}
		if msg.Method != "next" {
			t.Errorf("Method = %q, want %q", msg.Method, "next")
		}
	})

	t.Run("far over the limit is rejected without buffering it", func(t *testing.T) {
		line := strings.Repeat("x", 100*testMaxSize)
		r := NewReader(strings.NewReader(line+"\n"+next), testMaxSize)
		if _, err := r.Read(); !errors.Is(err, ErrMessageTooLarge) {
			t.Fatalf("Read() error = %v, want ErrMessageTooLarge", err)
		}
		if len(r.line) > testMaxSize {
			t.Errorf("reader buffered %d bytes, want at most %d", len(r.line), testMaxSize)
		}
		if msg, err := r.Read(); err != nil || msg.Method != "next" {
			t.Fatalf("Read() after oversized line = %v, %v", msg, err)
		}
	})

	t.Run("oversized last line without newline", func(t *testing.T) {
		r := NewReader(strings.NewReader(messageOfSize(testMaxSize+1)), testMaxSize)
		if _, err := r.Read(); !errors.Is(err, ErrMessageTooLarge) {
			t.Fatalf("Read() error = %v, want ErrMessageTooLarge", err)
		}
		if _, err := r.Read(); !errors.Is(err, io.EOF) {
			t.Fatalf("second Read() error = %v, want io.EOF", err)
		}
	})
}

func TestReaderLongLineUnderLimit(t *testing.T) {
	// Longer than bufio's default 4096-byte buffer, so the line arrives in
	// several chunks that must be joined back together.
	const maxSize = 64 * 1024
	text := strings.Repeat("y", 20000)
	line := `{"jsonrpc":"2.0","id":1,"method":"m","params":{"t":"` + text + `"}}`
	r := NewReader(strings.NewReader(line+"\n"), maxSize)

	msg, err := r.Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if !strings.Contains(string(msg.Params), text) {
		t.Error("params lost data from a long line")
	}
}

func TestReaderMessageSurvivesNextRead(t *testing.T) {
	// The reader reuses its line buffer, so a returned message must not
	// point into it.
	input := "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"a\",\"params\":{\"k\":\"first\"}}\n" +
		"{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"b\",\"params\":{\"k\":\"second\"}}\n"
	r := NewReader(strings.NewReader(input), testMaxSize)

	first, err := r.Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if _, err := r.Read(); err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if string(first.Params) != `{"k":"first"}` {
		t.Errorf("first message params changed to %s", first.Params)
	}
}

func TestReaderPassesThroughIOErrors(t *testing.T) {
	boom := errors.New("pipe broke")
	r := NewReader(iotest.ErrReader(boom), testMaxSize)
	if _, err := r.Read(); !errors.Is(err, boom) {
		t.Fatalf("Read() error = %v, want %v", err, boom)
	}
}

func TestWriterWritesOneLinePerMessage(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)

	if err := w.Write(NewRequest(NewIntID(1), "tools/list", nil)); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := w.Write(NewNotification("notifications/cancelled", []byte(`{"requestId":1}`))); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	want := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n" +
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}` + "\n"
	if buf.String() != want {
		t.Errorf("output =\n%s\nwant\n%s", buf.String(), want)
	}
}

func TestWriterRemovesNewlinesFromRawParams(t *testing.T) {
	// Raw params from elsewhere may be pretty-printed. The output must still
	// be a single line, or the receiver would see a broken message.
	var buf bytes.Buffer
	w := NewWriter(&buf)
	params := []byte("{\n  \"text\": \"a\\nb\"\n}")

	if err := w.Write(NewRequest(NewIntID(1), "tools/call", params)); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	out := buf.String()
	if strings.Count(out, "\n") != 1 || !strings.HasSuffix(out, "\n") {
		t.Errorf("output is not exactly one line: %q", out)
	}
}

func TestWriterConcurrentWritesStayWhole(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(&buf)
	const goroutines = 50

	var wg sync.WaitGroup
	for i := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			msg := NewResult(NewIntID(int64(i)), []byte(`{"text":"`+strings.Repeat("z", 500)+`"}`))
			if err := w.Write(msg); err != nil {
				t.Errorf("Write() error = %v", err)
			}
		}()
	}
	wg.Wait()

	r := NewReader(&buf, 1<<20)
	seen := make(map[ID]bool)
	for {
		msg, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Read() error = %v (lines were interleaved)", err)
		}
		seen[*msg.ID] = true
	}
	if len(seen) != goroutines {
		t.Errorf("read %d distinct messages, want %d", len(seen), goroutines)
	}
}

func TestWriterPassesThroughIOErrors(t *testing.T) {
	boom := errors.New("pipe closed")
	w := NewWriter(failingWriter{err: boom})
	if err := w.Write(NewNotification("x", nil)); !errors.Is(err, boom) {
		t.Fatalf("Write() error = %v, want %v", err, boom)
	}
}

type failingWriter struct{ err error }

func (f failingWriter) Write([]byte) (int, error) { return 0, f.err }
