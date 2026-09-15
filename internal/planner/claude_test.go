package planner

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"
)

// claudeServer answers the Messages endpoint with reply and records the
// request body and beta header it got.
func claudeServer(t *testing.T, status int, reply string, body *map[string]any, beta *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
			t.Errorf("request %s %s, want POST /v1/messages", r.Method, r.URL.Path)
		}
		if body != nil {
			if err := json.NewDecoder(r.Body).Decode(body); err != nil {
				t.Errorf("request body: %v", err)
			}
		}
		if beta != nil {
			*beta = r.Header.Get("anthropic-beta")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testClaude(srv *httptest.Server) *Claude {
	return NewClaude("", option.WithBaseURL(srv.URL), option.WithAPIKey("test-key"), option.WithMaxRetries(0))
}

func message(stopReason, content string) string {
	return `{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5",` +
		`"content":` + content + `,"stop_reason":"` + stopReason + `","stop_sequence":null,` +
		`"usage":{"input_tokens":10,"output_tokens":20}}`
}

func TestClaudeRequestShape(t *testing.T) {
	w, reg := loadWorld(t), loadPacks(t)
	conv, err := Repair(Opening(referenceIntent, w, reg), `{"apiVersion":"x"}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	var beta string
	srv := claudeServer(t, http.StatusOK,
		message("end_turn", `[{"type":"thinking","thinking":"","signature":"sig"},{"type":"text","text":"{\"apiVersion\":\"keel.plan/v1\"}"}]`),
		&body, &beta)

	c := testClaude(srv)
	reply, err := c.Propose(context.Background(), conv)
	if err != nil {
		t.Fatal(err)
	}
	if reply != `{"apiVersion":"keel.plan/v1"}` {
		t.Fatalf("reply %q", reply)
	}
	if c.Name() != "claude/claude-opus-5" || body["model"] != "claude-opus-5" {
		t.Fatalf("name %s, model sent %v", c.Name(), body["model"])
	}
	if beta != "server-side-fallback-2026-07-01" {
		t.Fatalf("anthropic-beta %q", beta)
	}
	if body["fallbacks"] != "default" {
		t.Fatalf("fallbacks %v, want \"default\"", body["fallbacks"])
	}
	if th := body["thinking"].(map[string]any); th["type"] != "adaptive" {
		t.Fatalf("thinking %v", th)
	}
	if effort := body["output_config"].(map[string]any)["effort"]; effort != "low" {
		t.Fatalf("effort %v, want low with thinking off", effort)
	}
	format := body["output_config"].(map[string]any)["format"].(map[string]any)
	if format["type"] != "json_schema" {
		t.Fatalf("output format %v", format["type"])
	}
	sent, _ := json.Marshal(format["schema"])
	var want any
	if err := json.Unmarshal(conv.Schema, &want); err != nil {
		t.Fatal(err)
	}
	wantJSON, _ := json.Marshal(want)
	if string(sent) != string(wantJSON) {
		t.Fatal("output_config.format.schema is not the model schema")
	}
	system := body["system"].([]any)[0].(map[string]any)
	if system["text"] != conv.System {
		t.Fatal("system prompt not sent as the system block")
	}
	msgs := body["messages"].([]any)
	roles := make([]string, len(msgs))
	for i, m := range msgs {
		roles[i] = m.(map[string]any)["role"].(string)
	}
	if len(roles) != 3 || roles[0] != "user" || roles[1] != "assistant" || roles[2] != "user" {
		t.Fatalf("roles %v, want user, assistant, user", roles)
	}
}

// Thinking on keeps adaptive thinking at the model's default effort.
func TestClaudeThinkKeepsDefaultEffort(t *testing.T) {
	var body map[string]any
	srv := claudeServer(t, http.StatusOK, message("end_turn", `[{"type":"text","text":"{}"}]`), &body, nil)
	c := testClaude(srv)
	c.Think = true
	if _, err := c.Propose(context.Background(), Conversation{System: "s", Messages: []Message{{Role: RoleUser, Content: "i"}}, Schema: []byte(`{"type":"object"}`)}); err != nil {
		t.Fatal(err)
	}
	if th := body["thinking"].(map[string]any); th["type"] != "adaptive" {
		t.Fatalf("thinking %v", th)
	}
	if effort, ok := body["output_config"].(map[string]any)["effort"]; ok {
		t.Fatalf("effort %v sent, want the default", effort)
	}
}

func TestClaudeFailuresAreBackendErrors(t *testing.T) {
	cases := map[string]struct {
		status int
		body   string
	}{
		"refused after fallbacks": {http.StatusOK, `{"id":"msg_01","type":"message","role":"assistant","model":"claude-opus-5","content":[],"stop_reason":"refusal","stop_sequence":null,"stop_details":{"type":"refusal","category":"cyber","explanation":"declined"},"usage":{"input_tokens":0,"output_tokens":0}}`},
		"truncated":               {http.StatusOK, message("max_tokens", `[{"type":"text","text":"{\"api"}]`)},
		"no text":                 {http.StatusOK, message("end_turn", `[]`)},
		"unauthenticated":         {http.StatusUnauthorized, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`},
		"overloaded":              {529, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := claudeServer(t, tc.status, tc.body, nil, nil)
			_, err := testClaude(srv).Propose(context.Background(), Conversation{System: "s", Messages: []Message{{Role: RoleUser, Content: "i"}}, Schema: []byte(`{"type":"object"}`)})
			if !errors.Is(err, ErrBackend) {
				t.Fatalf("got %v, want ErrBackend", err)
			}
		})
	}
}
