// Package sse provides an API for Server-Sent Events as described at https://html.spec.whatwg.org/multipage/server-sent-events.html.
package sse

import (
	"bytes"
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Event represents the data sent to a client.
// All fields are optional, and are not sent if empty.
type Event struct {
	Event string
	Data  []byte
	ID    string
	Retry time.Duration
}

// Write is a convenience method for including data in the Event.
func (e *Event) Write(b []byte) (int, error) {
	e.Data = append(e.Data, b...)
	return len(b), nil
}

// EventStream is the main point of interaction with the API.
// Events may be sent using the Send() method, and any additional
// context added to the request is provided by the Context() method.
type EventStream struct {
	ctx    context.Context
	events chan Event
}

// Context returns the context.Context attached to the *http.Request that started the
// event stream. It can be used to check if e.g. a client canceled/closed the connection:
//
//     select {
//     case <-stream.Context().Done():
//         stream.Close()
//         return
//     }
//
func (s EventStream) Context() context.Context { return s.ctx }

// Send sends an event to the client.
func (s EventStream) Send(e Event) { s.events <- e }

// ResetLastEventID causes an event with an empty id to be sent to the client,
// "...meaning no `Last-Event-ID` header will now be sent in the event of a reconnection being attempted."
func (s EventStream) ResetLastEventID() { s.events <- Event{ID: " "} }

// Close closes the EventStream. Send() must not be called after Close() is called.
func (s EventStream) Close() error {
	close(s.events)
	return nil
}

// NewEventStreamHandler is a function that is called for each new request received by a Handler.
// Note that it MUST NOT block (for long).
// The EventStream parameter is used for sending events to the client.
// If the client included a Last-Event-ID header, its value is provided in the lastEventID parameter.
// If the function returns an error, the Handler responds to the client with a 500.
type NewEventStreamHandler func(stream EventStream, lastEventID string) error

// Handler may be used as a http.Handler for the handling and sending of Server-Sent Events.
type Handler struct {
	// KeepAlive enables sending non-event data when not 0.
	// Useful when dealing with e.g. legacy proxy servers.
	// The spec recommends "... every 15 seconds or so."
	// Note that is unrelated to other methods of keeping connections alive,
	// such as the "Connection: keep-alive" header.
	KeepAlive time.Duration

	handler     NewEventStreamHandler
	chanBufSize uint
	bufPool     bufferPool
}

// NewHandler returns a *Handler which will call newEventStream on each http request,
// which provides an EventStream for sending events to the client.
func NewHandler(newEventStream NewEventStreamHandler) *Handler {
	return NewHandlerBuffered(newEventStream, 0)
}

// NewHandlerBuffered allows for configuring the buffer size of Go channels used internally.
// The default is unbuffered.
// See NewHandler.
func NewHandlerBuffered(newEventStream NewEventStreamHandler, chanBufSize uint) *Handler {
	return &Handler{
		handler:     newEventStream,
		chanBufSize: chanBufSize,
		bufPool:     newBufferPool(),
	}
}

// ServeHTTP is Handler's implementation of http.Handler, and should not normally need to be
// used directly by user of the API.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	flush := canFlush(w)
	if flush == nil {
		http.Error(w, "Flushing must be supported", http.StatusNotImplemented)
		return
	}

	if !isAcceptable(r) {
		http.Error(w, `User agent must accept "Content-Type: text/event-stream"`, http.StatusNotAcceptable)
		return
	}

	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Content-Type", "text/event-stream")

	lastEventID := r.Header.Get("Last-Event-ID")
	stream := EventStream{
		ctx:    r.Context(),
		events: make(chan Event, h.chanBufSize),
	}
	if err := h.handler(stream, lastEventID); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	var keepAlive <-chan time.Time
	if h.KeepAlive > 0 {
		ticker := time.NewTicker(h.KeepAlive)
		defer ticker.Stop()
		keepAlive = ticker.C
	}

	for {
		select {
		case <-r.Context().Done():
			return

		case evt, ok := <-stream.events:
			if !ok {
				return
			}

			buf := h.bufPool.Get()
			if writeEvent(&buf, &evt) {
				buf.WriteByte('\n')
				w.Write(buf.Bytes())
				flush()
			}
			h.bufPool.Put(buf)

		case <-keepAlive:
			w.Write([]byte(": keep-alive\n\n"))
		}
	}
}

func writeEvent(buf *bytes.Buffer, evt *Event) (wrote bool) {
	if len(evt.Event) != 0 {
		buf.WriteString("event:")
		buf.WriteString(evt.Event)
		buf.WriteByte('\n')
	}

	if len(evt.Data) != 0 {
		lines := bytes.Split(evt.Data, []byte{'\n'})
		for _, line := range lines {
			buf.WriteString("data:")
			buf.Write(line)
			buf.WriteByte('\n')
		}
	}

	if len(evt.ID) != 0 {
		if evt.ID == " " {
			buf.WriteString("id\n")
		} else {
			buf.WriteString("id:")
			buf.WriteString(evt.ID)
			buf.WriteByte('\n')
		}
	}

	if evt.Retry > 0 {
		retry := strconv.FormatInt(evt.Retry.Milliseconds(), 10)
		buf.WriteString("retry:")
		buf.WriteString(retry)
		buf.WriteByte('\n')
	}

	return buf.Len() > 0
}

func canFlush(w http.ResponseWriter) func() {
	f, ok := w.(http.Flusher)
	if !ok {
		return nil
	}
	return f.Flush
}

func isAcceptable(r *http.Request) bool {
	acceptedTypes := r.Header.Values("Accept")
	if len(acceptedTypes) == 0 {
		return true
	}

	for _, contentType := range acceptedTypes {
		parts := strings.SplitN(contentType, ";", 2)
		parts[0] = strings.TrimSpace(parts[0])

		if parts[0] == "text/event-stream" || parts[0] == "text/*" || parts[0] == "*/*" {
			return true
		}
	}

	return false
}

type bufferPool struct{ sync.Pool }

func (p *bufferPool) Get() bytes.Buffer { return p.Pool.Get().(bytes.Buffer) }
func (p *bufferPool) Put(buf bytes.Buffer) {
	buf.Reset()
	p.Pool.Put(buf)
}

func newBufferPool() bufferPool {
	return bufferPool{
		Pool: sync.Pool{
			New: func() interface{} { return bytes.Buffer{} },
		},
	}
}
