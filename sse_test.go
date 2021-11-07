package sse

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHandlerServeHTTP(t *testing.T) {
	t.Parallel()

	t.Run("handles unsupported flushing", func(t *testing.T) {
		t.Parallel()

		h := NewHandler(func(stream EventStream, lastEventID string) error {
			return nil
		})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var badWrapper = struct {
				http.ResponseWriter
			}{
				ResponseWriter: w,
			}

			h.ServeHTTP(badWrapper, r)
		}))
		defer srv.Close()

		client := srv.Client()
		resp, err := client.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNotImplemented {
			t.Errorf("expected status %d, but got %d", http.StatusNotImplemented, resp.StatusCode)
		}
	})

	t.Run("handles Accept header", func(t *testing.T) {
		t.Parallel()

		h := NewHandler(func(stream EventStream, lastEventID string) error {
			return stream.Close()
		})
		srv := httptest.NewServer(h)
		defer srv.Close()

		t.Run("not acceptable", func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
			req.Header.Set("Accept", "application/json")

			client := srv.Client()
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusNotAcceptable {
				t.Errorf("expected status %d, but got %d", http.StatusNotAcceptable, resp.StatusCode)
			}
		})

		t.Run("exact match", func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
			req.Header.Set("Accept", "text/event-stream")

			client := srv.Client()
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Errorf("expected status %d, but got %d", http.StatusOK, resp.StatusCode)
			}
		})

		t.Run("matches one option", func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
			req.Header.Add("Accept", "application/json")
			req.Header.Add("Accept", "text/event-stream")

			client := srv.Client()
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Errorf("expected status %d, but got %d", http.StatusOK, resp.StatusCode)
			}
		})

		t.Run("matches one option, with q values", func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
			req.Header.Add("Accept", "application/json; q=0.2")
			req.Header.Add("Accept", "text/event-stream; q=0.1")

			client := srv.Client()
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Errorf("expected status %d, but got %d", http.StatusOK, resp.StatusCode)
			}
		})
	})

	t.Run("Sets headers", func(t *testing.T) {
		t.Parallel()

		h := NewHandler(func(stream EventStream, lastEventID string) error {
			return stream.Close()
		})
		srv := httptest.NewServer(h)
		defer srv.Close()

		client := srv.Client()
		resp, err := client.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected status %d, but got %d", http.StatusOK, resp.StatusCode)
		}

		if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("Expected response header 'Cache-Control: no-cache', but was 'Cache-Control: %s'", cc)
		}

		if cc := resp.Header.Get("Connection"); cc != "keep-alive" {
			t.Errorf("Expected response header 'Connection: keep-alive', but was 'Connection: %s'", cc)
		}

		if cc := resp.Header.Get("Content-Type"); cc != "text/event-stream" {
			t.Errorf("Expected response header 'Content-Type: text/event-stream', but was 'Content-Type: %s'", cc)
		}
	})

	t.Run("Passes Last-Event-ID", func(t *testing.T) {
		t.Parallel()

		h := NewHandler(func(stream EventStream, lastEventID string) error {
			if lastEventID != "last-event-id" {
				t.Errorf("Expected Last-Event-ID header to be 'last-event-id', but was %q", lastEventID)
			}
			return stream.Close()
		})
		srv := httptest.NewServer(h)
		defer srv.Close()

		req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
		req.Header.Set("Last-Event-ID", "last-event-id")

		client := srv.Client()
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected status %d, but got %d", http.StatusOK, resp.StatusCode)
		}
	})

	t.Run("Sends Keep-Alive comments", func(t *testing.T) {
		t.Parallel()

		h := NewHandler(func(stream EventStream, lastEventID string) error {
			go func() {
				<-time.After(750 * time.Millisecond)
				stream.Close()
			}()
			return nil
		})
		h.KeepAlive = 500 * time.Millisecond
		srv := httptest.NewServer(h)
		defer srv.Close()

		client := srv.Client()
		resp, err := client.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected status %d, but got %d", http.StatusOK, resp.StatusCode)
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal("failed to read response body:", err)
		}

		if !bytes.Contains(body, []byte(": keep-alive")) {
			t.Errorf("expected ': keep-alive' in response body, but was not found: %q", body)
		}
	})

	t.Run("writes", func(t *testing.T) {
		t.Parallel()

		h := NewHandler(func(stream EventStream, lastEventID string) error {
			go func() {
				stream.Send(Event{
					Event: "hello",
					Data:  []byte("this is a server-sent event"),
					ID:    "1",
					Retry: 250 * time.Millisecond,
				})
				stream.Close()
			}()
			return nil
		})
		srv := httptest.NewServer(h)
		defer srv.Close()

		client := srv.Client()
		resp, err := client.Get(srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected status %d, but got %d", http.StatusOK, resp.StatusCode)
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal("failed to read response body:", err)
		}

		if !bytes.Contains(body, []byte("event:hello")) {
			t.Errorf("expected 'event:hello' in response body, but was not found: %q", body)
		}

		if !bytes.Contains(body, []byte("data:this is a server-sent event")) {
			t.Errorf("expected 'data:this is a server-sent event' in response body, but was not found: %q", body)
		}

		if !bytes.Contains(body, []byte("id:1")) {
			t.Errorf("expected 'id:1' in response body, but was not found: %q", body)
		}

		if !bytes.Contains(body, []byte("retry:250")) {
			t.Errorf("expected 'retry:250' in response body, but was not found: %q", body)
		}
	})
}
