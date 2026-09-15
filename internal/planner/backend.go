package planner

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// The backend names an operator chooses from, on keelctl's command line and
// in keeld's configuration and compile requests.
const (
	BackendOllama = "ollama"
	BackendClaude = "claude"
)

// ollamaDefaultPort is the port Ollama listens on when OLLAMA_HOST omits it.
const ollamaDefaultPort = "11434"

// ErrUnknownBackend reports a backend name that is neither BackendOllama nor
// BackendClaude.
var ErrUnknownBackend = errors.New("planner: unknown backend")

// Backends lists the backend names, sorted.
func Backends() []string { return []string{BackendClaude, BackendOllama} }

// BackendOptions configures the backend NewBackend returns.
type BackendOptions struct {
	// Model empty means the backend's default.
	Model string
	// OllamaURL empty means DefaultOllamaURL. The Claude backend ignores it.
	OllamaURL string
	// Think lets the model reason before replying. Off, Ollama is asked not
	// to think and Claude thinks at low effort (see each backend). The
	// validator judges the reply either way.
	Think bool
}

// NewBackend returns the planner a backend name selects. The Claude backend
// reads its key from the environment, as the SDK does.
func NewBackend(name string, o BackendOptions) (Planner, error) {
	switch name {
	case BackendOllama:
		p := NewOllama(o.OllamaURL, o.Model)
		p.Think = o.Think
		return p, nil
	case BackendClaude:
		p := NewClaude(o.Model)
		p.Think = o.Think
		return p, nil
	default:
		return nil, fmt.Errorf("%w %q, want %s", ErrUnknownBackend, name, strings.Join(Backends(), " or "))
	}
}

// OllamaHostURL turns OLLAMA_HOST, which Ollama accepts as host, host:port or
// a full URL, into the URL a client dials. Empty means the default, and is
// returned empty. The caller reads the variable: ambient input stays at the
// edge of the program.
func OllamaHostURL(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if !strings.Contains(v, "://") {
		v = "http://" + v
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" {
		return v
	}
	if u.Port() == "" {
		u.Host = net.JoinHostPort(u.Hostname(), ollamaDefaultPort)
	}
	return strings.TrimRight(u.String(), "/")
}
