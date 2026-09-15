package planner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Ollama defaults. The model is overridable per call site; qwen3:14b fits in
// 16 GB of VRAM and follows a JSON schema reliably.
const (
	DefaultOllamaURL   = "http://localhost:11434"
	DefaultOllamaModel = "qwen3:14b"
)

// maxErrorBody bounds how much of a failed response is quoted in an error.
const maxErrorBody = 4 << 10

// Ollama is the local backend, the default: the repository runs with no
// credentials and no network beyond localhost.
//
// It speaks /api/chat directly over net/http. The request and the reply are a
// handful of fields, and the official client would pull the whole Ollama
// module in for them.
type Ollama struct {
	BaseURL string
	Model   string
	Client  *http.Client
	// Think is sent as the request's think flag. A thinking model such as
	// qwen3 reasons by default, even under a format constraint: on the
	// reference world a plan call took 11.3 s thinking and 3.7 s without, on
	// a 16 GB GPU. A model without a thinking mode accepts false and refuses
	// true, which is then the backend's error.
	Think bool
}

// NewOllama returns a backend for the given server and model, using the
// defaults for empty values.
func NewOllama(baseURL, model string) *Ollama {
	if baseURL == "" {
		baseURL = DefaultOllamaURL
	}
	if model == "" {
		model = DefaultOllamaModel
	}
	return &Ollama{BaseURL: strings.TrimRight(baseURL, "/"), Model: model, Client: http.DefaultClient}
}

// Name identifies the backend and model in outcomes and logs.
func (o *Ollama) Name() string { return "ollama/" + o.Model }

type ollamaRequest struct {
	Model    string          `json:"model"`
	Messages []Message       `json:"messages"`
	Stream   bool            `json:"stream"`
	Think    bool            `json:"think"`
	Format   json.RawMessage `json:"format"`
	Options  ollamaOptions   `json:"options"`
}

// ollamaOptions pins sampling. Temperature 0 makes the same conversation yield
// the same reply on the same model, so a failing compile can be reproduced;
// it does not make the model right, which is the validator's job.
type ollamaOptions struct {
	Temperature float64 `json:"temperature"`
}

type ollamaResponse struct {
	Message    Message `json:"message"`
	Done       bool    `json:"done"`
	DoneReason string  `json:"done_reason"`
	Error      string  `json:"error"`
}

// Propose sends the conversation to /api/chat with the schema as the output
// format and returns the reply text, without the reasoning when the model
// thought.
func (o *Ollama) Propose(ctx context.Context, c Conversation) (string, error) {
	msgs := make([]Message, 0, len(c.Messages)+1)
	msgs = append(msgs, Message{Role: "system", Content: c.System})
	msgs = append(msgs, c.Messages...)
	body, err := json.Marshal(ollamaRequest{
		Model:    o.Model,
		Messages: msgs,
		Stream:   false,
		Think:    o.Think,
		Format:   c.Schema,
		Options:  ollamaOptions{Temperature: 0},
	})
	if err != nil {
		return "", fmt.Errorf("%w: ollama: encoding request: %w", ErrBackend, err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("%w: ollama: %w", ErrBackend, err)
	}
	req.Header.Set("Content-Type", "application/json")
	client := o.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: ollama at %s: %w", ErrBackend, o.BaseURL, err)
	}
	// The body is fully read or abandoned by the time this runs; a close
	// error on a response body carries nothing the caller can act on.
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		var e ollamaResponse
		if json.Unmarshal(msg, &e) == nil && e.Error != "" {
			return "", fmt.Errorf("%w: ollama: %s: %s", ErrBackend, resp.Status, e.Error)
		}
		return "", fmt.Errorf("%w: ollama: %s: %s", ErrBackend, resp.Status, strings.TrimSpace(string(msg)))
	}

	var out ollamaResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("%w: ollama: decoding reply: %w", ErrBackend, err)
	}
	switch {
	case out.Error != "":
		return "", fmt.Errorf("%w: ollama: %s", ErrBackend, out.Error)
	case !out.Done:
		return "", fmt.Errorf("%w: ollama: reply not done", ErrBackend)
	case out.DoneReason == "length":
		return "", fmt.Errorf("%w: ollama: reply truncated at the context or token limit", ErrBackend)
	case strings.TrimSpace(out.Message.Content) == "":
		return "", fmt.Errorf("%w: ollama: empty reply", ErrBackend)
	}
	return out.Message.Content, nil
}
