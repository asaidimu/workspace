package provider

import (
	"bufio"
	"io"
	"strconv"
	"strings"
)

// SSEEvent is one decoded server-sent event frame, before any vendor
// mapping. Data is the concatenated data lines with the frame's trailing
// newline removed, exactly as the WHATWG event-stream format defines it.
type SSEEvent struct {
	Event string // event type, defaulting to "message"
	Data  []byte
	ID    string // last event id, which persists across frames
	Retry int    // milliseconds, 0 when the stream never set one
}

// SSEDecoder incrementally decodes an event stream. Adapters share this one
// decoder (section 10: "one shared SSE decoder"); each maps Data into its
// vendor's typed events. The decoder follows the WHATWG parsing rules:
// comments are ignored, only the first colon splits field from value, one
// leading space of the value is stripped, lines may end in \n, \r\n or \r,
// an empty line dispatches, and an end-of-file mid-frame discards it.
type SSEDecoder struct {
	scanner *bufio.Scanner
	event   string
	data    []byte
	id      string
	retry   int
}

func NewSSEDecoder(r io.Reader) *SSEDecoder {
	sc := bufio.NewScanner(r)
	// Splits \n, \r\n and lone \r, and grows past the 64 KiB default so a
	// large JSON frame is not an error.
	sc.Split(splitSSELines)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	return &SSEDecoder{scanner: sc}
}

// Next returns the next complete event, or io.EOF when the stream ended.
// A malformed stream cannot be distinguished from a clean one at the byte
// level — SSE has no framing errors — so scanner errors are the only
// non-EOF failure.
func (d *SSEDecoder) Next() (SSEEvent, error) {
	for {
		if !d.scanner.Scan() {
			if err := d.scanner.Err(); err != nil {
				return SSEEvent{}, err
			}
			// End of file discards any partial frame (WHATWG: run the EOF
			// steps — the pending buffers are simply dropped).
			return SSEEvent{}, io.EOF
		}
		line := d.scanner.Text()
		if line == "" {
			if len(d.data) == 0 {
				// Nothing to dispatch: reset the event type and continue.
				d.event = ""
				continue
			}
			ev := SSEEvent{
				Event: d.event,
				Data:  d.data,
				ID:    d.id,
				Retry: d.retry,
			}
			if ev.Event == "" {
				ev.Event = "message"
			}
			// The data buffer accumulated one trailing newline per data
			// line; dispatch removes exactly one.
			ev.Data = d.data[:len(d.data)-1]
			d.data = nil
			d.event = ""
			return ev, nil
		}
		if strings.HasPrefix(line, ":") {
			continue // comment
		}
		field, value := line, ""
		if i := strings.IndexByte(line, ':'); i >= 0 {
			field = line[:i]
			value = line[i+1:]
			value = strings.TrimPrefix(value, " ")
		}
		switch field {
		case "event":
			d.event = value
		case "data":
			d.data = append(d.data, value...)
			d.data = append(d.data, '\n')
		case "id":
			if !strings.ContainsRune(value, 0) {
				d.id = value
			}
		case "retry":
			if n, err := strconv.Atoi(value); err == nil {
				d.retry = n
			}
		}
	}
}

// splitSSELines is a bufio.SplitFunc that recognizes \n, \r\n and a lone
// \r as line terminators without swallowing the next line's start.
func splitSSELines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	for i := 0; i < len(data); i++ {
		switch data[i] {
		case '\n':
			return i + 1, trimCR(data[:i]), nil
		case '\r':
			if i+1 < len(data) {
				if data[i+1] == '\n' {
					return i + 2, data[:i], nil
				}
				return i + 1, data[:i], nil
			}
			if atEOF {
				return i + 1, data[:i], nil
			}
			// Could be \r\n: wait for the next byte.
			return 0, nil, nil
		}
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func trimCR(line []byte) []byte {
	if n := len(line); n > 0 && line[n-1] == '\r' {
		return line[:n-1]
	}
	return line
}
