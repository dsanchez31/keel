package planner

import (
	"context"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"
)

// DefaultClaudeModel is the Claude model the backend uses unless told
// otherwise.
const DefaultClaudeModel = "claude-opus-5"

// claudeMaxTokens bounds one reply. A Plan IR is a few hundred tokens; the
// headroom is for adaptive thinking, which counts against the same limit.
const claudeMaxTokens = 16000

// Claude is the opt-in hosted backend (design section 2.1).
//
// Credentials follow the SDK's own resolution: ANTHROPIC_API_KEY, then
// ANTHROPIC_AUTH_TOKEN, then an `ant auth login` profile. Nothing is read here.
type Claude struct {
	Model string
	// Think keeps the default effort. Off, the request asks for low effort
	// rather than disabling thinking: Anthropic advises against disabled
	// thinking on Opus 5, which can leak internal tags into the visible text
	// (here the rationale), and a lower effort gets most of the latency saving.
	Think  bool
	client anthropic.Client
}

// NewClaude returns a backend for the given model, the default when empty.
// opts are passed to the SDK client, which is how tests point it at a local
// server.
func NewClaude(model string, opts ...option.RequestOption) *Claude {
	if model == "" {
		model = DefaultClaudeModel
	}
	return &Claude{Model: model, client: anthropic.NewClient(opts...)}
}

// Name identifies the backend and model in outcomes and logs.
func (c *Claude) Name() string { return "claude/" + c.Model }

// Propose sends the conversation and returns the reply text.
//
// The request uses the beta Messages endpoint for server-side fallbacks: with
// fallbacks "default", a request the model's safety classifiers decline is
// re-served inside the same call by the model Anthropic recommends for that
// refusal category, instead of failing the compile. Thinking is adaptive, at
// low effort unless Think is set, and the reply is constrained by the model
// schema through structured outputs.
func (c *Claude) Propose(ctx context.Context, conv Conversation) (string, error) {
	msgs := make([]anthropic.BetaMessageParam, len(conv.Messages))
	for i, m := range conv.Messages {
		block := anthropic.NewBetaTextBlock(m.Content)
		switch m.Role {
		case RoleUser:
			msgs[i] = anthropic.NewBetaUserMessage(block)
		case RoleAssistant:
			msgs[i] = anthropic.BetaMessageParam{Role: anthropic.BetaMessageParamRoleAssistant, Content: []anthropic.BetaContentBlockParamUnion{block}}
		default:
			return "", fmt.Errorf("%w: claude: message %d has role %q", ErrBackend, i, m.Role)
		}
	}

	params := anthropic.BetaMessageNewParams{
		Model:     c.Model,
		MaxTokens: claudeMaxTokens,
		System:    []anthropic.BetaTextBlockParam{{Text: conv.System}},
		Messages:  msgs,
		Thinking:  anthropic.BetaThinkingConfigParamUnion{OfAdaptive: &anthropic.BetaThinkingConfigAdaptiveParam{}},
		OutputConfig: anthropic.BetaOutputConfigParam{
			Format: anthropic.BetaJSONOutputFormatParam{Schema: conv.Schema},
		},
		Fallbacks: anthropic.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()},
		Betas:     []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01},
	}
	if !c.Think {
		params.OutputConfig.Effort = anthropic.BetaOutputConfigEffortLow
	}

	resp, err := c.client.Beta.Messages.New(ctx, params)
	if err != nil {
		return "", fmt.Errorf("%w: claude: %w", ErrBackend, err)
	}

	// stop_reason before content: a refusal can arrive with empty or partial
	// content, and truncated output is not a plan to validate.
	switch resp.StopReason {
	case anthropic.BetaStopReasonRefusal:
		return "", fmt.Errorf("%w: claude: request refused (%s) after server-side fallbacks: %s",
			ErrBackend, resp.StopDetails.Category, resp.StopDetails.Explanation)
	case anthropic.BetaStopReasonMaxTokens:
		return "", fmt.Errorf("%w: claude: reply truncated at %d tokens", ErrBackend, claudeMaxTokens)
	}

	var b strings.Builder
	for _, block := range resp.Content {
		if block.Type == "text" {
			b.WriteString(block.Text)
		}
	}
	if strings.TrimSpace(b.String()) == "" {
		return "", fmt.Errorf("%w: claude: reply holds no text (stop reason %s)", ErrBackend, resp.StopReason)
	}
	return b.String(), nil
}
