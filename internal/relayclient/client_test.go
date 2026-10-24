package relayclient

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/muratgozel/seasoning/internal/keys"
	"github.com/muratgozel/seasoning/internal/relayapi"
)

func TestStreamParsesServerSentEvents(t *testing.T) {
	var lastID string
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastID = r.Header.Get("Last-Event-ID")
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, ": hello\n\n"+
			"id: 7\nevent: message\ndata: {\"a\":1}\n\n"+
			": keep-alive\n\n"+
			"event: resync\ndata: line one\ndata: line two\n\n"+
			"id: 9\ndata:no-space\n\n"+
			"id: 10\nevent: session\ndata: {}") // stream cut mid-event
	}))
	defer hs.Close()
	s, _ := keys.NewSecret()
	var got []Event
	err := New(hs.URL).Stream(context.Background(), keys.Derive(s), "s_aaaaaaaaaaaaaaaaaaaaaaaa", 6, func(e Event) error {
		got = append(got, e)
		return nil
	})
	var ue *UnreachableError
	if !errors.As(err, &ue) {
		t.Fatalf("a stream that ends must report it: %v", err)
	}
	if lastID != "6" {
		t.Fatalf("Last-Event-ID = %q", lastID)
	}
	want := []Event{
		{ID: 7, Type: "message", Data: []byte(`{"a":1}`)},
		{ID: 0, Type: "resync", Data: []byte("line one\nline two")},
		{ID: 9, Type: "message", Data: []byte("no-space")},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d events: %+v", len(got), got)
	}
	for i := range want {
		if got[i].ID != want[i].ID || got[i].Type != want[i].Type || string(got[i].Data) != string(want[i].Data) {
			t.Errorf("event %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestErrorsAreTypedAndActionable(t *testing.T) {
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusBadGateway)
			io.WriteString(w, "<html>bad gateway</html>")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":{"code":"not_found","message":"project not found","hint":"ask for a fresh invite"}}`)
	}))
	s, _ := keys.NewSecret()
	c := New(hs.URL)
	_, err := c.Sessions(context.Background(), keys.Derive(s))
	if !IsCode(err, relayapi.CodeNotFound) || err.Error() != "relay: project not found (ask for a fresh invite)" {
		t.Fatalf("api error: %v", err)
	}
	if _, err := c.Health(context.Background()); !IsCode(err, relayapi.CodeInternal) {
		t.Fatalf("non-JSON error: %v", err)
	}
	hs.Close()
	var ue *UnreachableError
	if _, err := c.Health(context.Background()); !errors.As(err, &ue) {
		t.Fatalf("closed relay: %v", err)
	}
}
