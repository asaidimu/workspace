package provider

import (
	"io"
	"strings"
	"testing"
)

func drainSSE(t *testing.T, in string) []SSEEvent {
	t.Helper()
	d := NewSSEDecoder(strings.NewReader(in))
	var events []SSEEvent
	for {
		ev, err := d.Next()
		if err == io.EOF {
			return events
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		events = append(events, ev)
	}
}

func TestSSEBasicFrames(t *testing.T) {
	events := drainSSE(t,
		"event: delta\ndata: {\"text\":\"hi\"}\n\n"+
			"data: plain\n\n"+
			"event: done\ndata: {}\n\n")
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3: %+v", len(events), events)
	}
	if events[0].Event != "delta" || string(events[0].Data) != `{"text":"hi"}` {
		t.Errorf("event 0 = %+v", events[0])
	}
	if events[1].Event != "message" {
		t.Errorf("default event type = %q, want message", events[1].Event)
	}
	if events[2].Event != "done" || string(events[2].Data) != "{}" {
		t.Errorf("event 2 = %+v", events[2])
	}
}

func TestSSEMultiLineData(t *testing.T) {
	events := drainSSE(t, "data: line one\ndata: line two\ndata: line three\n\n")
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	want := "line one\nline two\nline three"
	if string(events[0].Data) != want {
		t.Errorf("data = %q, want %q", events[0].Data, want)
	}
}

func TestSSECRLFAndLoneCR(t *testing.T) {
	events := drainSSE(t, "event: a\r\ndata: one\r\n\r\ndata: two\rdata: three\n\n")
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(events), events)
	}
	if string(events[0].Data) != "one" || events[0].Event != "a" {
		t.Errorf("event 0 = %+v", events[0])
	}
	if string(events[1].Data) != "two\nthree" {
		t.Errorf("event 1 data = %q", events[1].Data)
	}
}

func TestSSECommentsAndColonRules(t *testing.T) {
	events := drainSSE(t,
		": keep-alive\n"+
			": another comment\n"+
			"data: {\"url\":\"https://x:7000\"}\n\n"+
			"data:  two spaces\n\n"+
			"data\n\n") // field without a colon carries an empty value
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3: %+v", len(events), events)
	}
	if string(events[0].Data) != `{"url":"https://x:7000"}` {
		t.Errorf("colon split wrong: %q", events[0].Data)
	}
	if string(events[1].Data) != " two spaces" {
		t.Errorf("space strip wrong: %q", events[1].Data)
	}
	// A lone "data" line is an empty data value: the buffer holds just the
	// newline, so the frame dispatches with empty data.
	if string(events[2].Data) != "" {
		t.Errorf("bare data line = %q, want empty", events[2].Data)
	}
}

func TestSSEEmptyDataFrameDispatches(t *testing.T) {
	events := drainSSE(t, "data:\n\n")
	if len(events) != 1 || string(events[0].Data) != "" || events[0].Event != "message" {
		t.Fatalf("events = %+v, want one empty message frame", events)
	}
}

func TestSSEEventWithoutDataIsNotDispatched(t *testing.T) {
	events := drainSSE(t, "event: ping\n\n")
	if len(events) != 0 {
		t.Fatalf("event-only frame dispatched: %+v", events)
	}
	// The next data frame must not inherit the discarded event type.
	events = drainSSE(t, "event: ping\n\ndata: x\n\n")
	if len(events) != 1 || events[0].Event != "message" || string(events[0].Data) != "x" {
		t.Fatalf("events = %+v", events)
	}
}

func TestSSEIDAndRetryPersistAcrossFrames(t *testing.T) {
	events := drainSSE(t, "id: 7\nretry: 250\ndata: a\n\ndata: b\n\nid: 8\ndata: c\n\n")
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3", len(events))
	}
	if events[0].ID != "7" || events[0].Retry != 250 {
		t.Errorf("event 0 id/retry = %q/%d", events[0].ID, events[0].Retry)
	}
	if events[1].ID != "7" || events[1].Retry != 250 {
		t.Errorf("event 1 must inherit id/retry, got %q/%d", events[1].ID, events[1].Retry)
	}
	if events[2].ID != "8" {
		t.Errorf("event 2 id = %q, want 8", events[2].ID)
	}
}

func TestSSEInvalidRetryAndNULIDIgnored(t *testing.T) {
	events := drainSSE(t, "retry: soon\nid: bad\x00id\ndata: x\n\n")
	if len(events) != 1 {
		t.Fatalf("got %d events", len(events))
	}
	if events[0].Retry != 0 || events[0].ID != "" {
		t.Errorf("retry/id = %d/%q, want 0/empty", events[0].Retry, events[0].ID)
	}
}

func TestSSEEofDiscardsPartialFrame(t *testing.T) {
	events := drainSSE(t, "event: done\ndata: half")
	if len(events) != 0 {
		t.Fatalf("partial frame at EOF was dispatched: %+v", events)
	}
}

func TestSSELargeFrame(t *testing.T) {
	payload := strings.Repeat("x", 300*1024)
	in := "data: " + payload + "\n\n"
	events := drainSSE(t, in)
	if len(events) != 1 || len(events[0].Data) != len(payload) {
		t.Fatalf("got %d events, len=%d", len(events), len(events[0].Data))
	}
}

func TestSSEManyFramesInOneStream(t *testing.T) {
	var sb strings.Builder
	const n = 500
	for i := 0; i < n; i++ {
		sb.WriteString("data: chunk\n\n")
	}
	events := drainSSE(t, sb.String())
	if len(events) != n {
		t.Fatalf("got %d events, want %d", len(events), n)
	}
}

// FuzzSSEDecode pins the decoder's invariants: no panics, no lost frames,
// and every emitted byte field is free of carriage returns (all three line
// terminators are normalized away). Plain `go test` runs the seed corpus;
// `go test -fuzz` extends it.
func FuzzSSEDecode(f *testing.F) {
	seeds := []string{
		"",
		"data: x\n\n",
		"event: delta\ndata: {\"a\":1}\n\n",
		"data: one\ndata: two\n\n",
		": comment\ndata: x\n\n",
		"event: ping\n\n",
		"retry: 100\nid: 42\ndata: x\n\n",
		"data: with\r\nand \\r\n\r\n",
		"data: a\rdata: b\r\r",
		"data",
		"data:",
		":\n\n",
		"data: {\"url\":\"http://x:1/y\"}\n\n",
		"event:\ndata: e\n\n",
		"data: \x00\n\n",
		"id: a\x00b\ndata: x\n\n",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		d := NewSSEDecoder(strings.NewReader(in))
		for {
			ev, err := d.Next()
			if err == io.EOF {
				return
			}
			if err != nil {
				t.Fatalf("Next: %v", err)
			}
			if strings.ContainsRune(string(ev.Data), '\r') {
				t.Fatalf("data contains CR: %q", ev.Data)
			}
			if strings.ContainsRune(ev.Event, '\r') || strings.ContainsRune(ev.Event, '\n') {
				t.Fatalf("event type contains a line break: %q", ev.Event)
			}
			if strings.ContainsRune(ev.ID, '\r') || strings.ContainsRune(ev.ID, '\n') {
				t.Fatalf("id contains a line break: %q", ev.ID)
			}
		}
	})
}
