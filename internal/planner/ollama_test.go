package planner

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ollamaServer answers /api/chat with reply and records the request it got.
func ollamaServer(t *testing.T, status int, reply string, got *map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/chat" {
			t.Errorf("request %s %s, want POST /api/chat", r.Method, r.URL.Path)
		}
		if got != nil {
			if err := json.NewDecoder(r.Body).Decode(got); err != nil {
				t.Errorf("request body: %v", err)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOllamaRequestShape(t *testing.T) {
	w, reg := loadWorld(t), loadPacks(t)
	conv := Opening(referenceIntent, w, reg)
	var got map[string]any
	srv := ollamaServer(t, http.StatusOK, `{"message":{"role":"assistant","content":"{\"apiVersion\":\"keel.plan/v1\"}"},"done":true,"done_reason":"stop"}`, &got)

	o := NewOllama(srv.URL+"/", "")
	reply, err := o.Propose(context.Background(), conv)
	if err != nil {
		t.Fatal(err)
	}
	if reply != `{"apiVersion":"keel.plan/v1"}` {
		t.Fatalf("reply %q", reply)
	}
	if o.Name() != "ollama/"+DefaultOllamaModel || got["model"] != DefaultOllamaModel {
		t.Fatalf("name %s, model sent %v", o.Name(), got["model"])
	}
	if got["stream"] != false {
		t.Fatal("the request must not stream")
	}
	if think, ok := got["think"]; !ok || think != false {
		t.Fatalf("think %v, want false sent explicitly: a thinking model reasons by default", got["think"])
	}
	if got["options"].(map[string]any)["temperature"] != 0.0 {
		t.Fatalf("options %v", got["options"])
	}
	format, err := json.Marshal(got["format"])
	if err != nil {
		t.Fatal(err)
	}
	var want any
	if err := json.Unmarshal(conv.Schema, &want); err != nil {
		t.Fatal(err)
	}
	wantJSON, _ := json.Marshal(want)
	if string(format) != string(wantJSON) {
		t.Fatal("format is not the model schema")
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("%d messages, want system and intent", len(msgs))
	}
	first, second := msgs[0].(map[string]any), msgs[1].(map[string]any)
	if first["role"] != "system" || first["content"] != conv.System || second["role"] != "user" || second["content"] != referenceIntent {
		t.Fatalf("messages %v", msgs)
	}
}

func TestOllamaFailuresAreBackendErrors(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
	}{
		"model not pulled": {http.StatusNotFound, `{"error":"model \"qwen3:14b\" not found, try pulling it first"}`},
		"server error":     {http.StatusInternalServerError, `oops`},
		"truncated":        {http.StatusOK, `{"message":{"role":"assistant","content":"{\"api"},"done":true,"done_reason":"length"}`},
		"empty":            {http.StatusOK, `{"message":{"role":"assistant","content":"  "},"done":true,"done_reason":"stop"}`},
		"not json":         {http.StatusOK, `<html>`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := ollamaServer(t, tc.status, tc.body, nil)
			_, err := NewOllama(srv.URL, "m").Propose(context.Background(), Conversation{System: "s"})
			if !errors.Is(err, ErrBackend) {
				t.Fatalf("got %v, want ErrBackend", err)
			}
		})
	}

	t.Run("unreachable", func(t *testing.T) {
		srv := ollamaServer(t, http.StatusOK, "", nil)
		srv.Close()
		if _, err := NewOllama(srv.URL, "m").Propose(context.Background(), Conversation{}); !errors.Is(err, ErrBackend) {
			t.Fatalf("got %v, want ErrBackend", err)
		}
	})
}
