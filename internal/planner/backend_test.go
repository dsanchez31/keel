package planner

import (
	"errors"
	"testing"
)

func TestNewBackend(t *testing.T) {
	p, err := NewBackend(BackendOllama, BackendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if o, ok := p.(*Ollama); !ok || o.BaseURL != DefaultOllamaURL || o.Model != DefaultOllamaModel || o.Think {
		t.Fatalf("ollama backend %#v, want the defaults", p)
	}
	if p, err = NewBackend(BackendOllama, BackendOptions{Model: "llama3", OllamaURL: "http://gpu-box:11434/", Think: true}); err != nil {
		t.Fatal(err)
	}
	if o := p.(*Ollama); o.BaseURL != "http://gpu-box:11434" || o.Model != "llama3" || !o.Think {
		t.Fatalf("ollama backend %#v", o)
	}
	if p, err = NewBackend(BackendClaude, BackendOptions{Think: true}); err != nil {
		t.Fatal(err)
	}
	if c, ok := p.(*Claude); !ok || c.Model != DefaultClaudeModel || !c.Think {
		t.Fatalf("claude backend %#v, want the default model", p)
	}
	if _, err := NewBackend("gpt", BackendOptions{}); !errors.Is(err, ErrUnknownBackend) {
		t.Fatalf("unknown backend: err %v, want ErrUnknownBackend", err)
	}
}

func TestOllamaHostURL(t *testing.T) {
	cases := map[string]string{
		"":                       "",
		"localhost":              "http://localhost:11434",
		"10.0.0.2:8080":          "http://10.0.0.2:8080",
		"https://ollama.example": "https://ollama.example:11434",
		"http://gpu-box:11434/":  "http://gpu-box:11434",
	}
	for in, want := range cases {
		if got := OllamaHostURL(in); got != want {
			t.Errorf("OllamaHostURL(%q) = %q, want %q", in, got, want)
		}
	}
}
