package proxy

import (
	"context"
	"encoding/json"
	"log"
	"strings"

	"github.com/local/nano-proxy/internal/store"
)

// chatCompletionResponse is the slim subset of the OpenAI-compatible response
// we care about for telemetry. We deliberately ignore choices[i].message.content
// and tool_calls[*].function.arguments — they can be megabytes, and the
// dashboard never reads them back.
type chatCompletionResponse struct {
	Usage    *usageBlock `json:"usage,omitempty"`
	Pricing  *pricingBlk `json:"x_nanogpt_pricing,omitempty"`
	Choices  []choice    `json:"choices,omitempty"`
	ReqID    string      `json:"id,omitempty"`
}

type usageBlock struct {
	PromptTokens         int             `json:"prompt_tokens"`
	CompletionTokens     int             `json:"completion_tokens"`
	TotalTokens          int             `json:"total_tokens"`
	CachedTokens         int             `json:"prompt_tokens_details.cached_tokens"`  // OpenAI nested path
	ReasoningTokens      int             `json:"completion_tokens_details.reasoning_tokens"`
	CacheCreationTokens  int             `json:"cache_creation_input_tokens"`
	CacheReadTokens      int             `json:"cache_read_input_tokens"`

	// Nested objects — populated via raw Unmarshal below so we don't depend
	// on a custom unmarshaller just for these.
	PromptDetails     *tokenDetails `json:"prompt_tokens_details,omitempty"`
	CompletionDetails *tokenDetails `json:"completion_tokens_details,omitempty"`
}

type tokenDetails struct {
	CachedTokens    int `json:"cached_tokens"`
	ReasoningTokens int `json:"reasoning_tokens"`
}

type pricingBlk struct {
	Cost          float64 `json:"cost"`
	InputTokens   int     `json:"inputTokens"`
	OutputTokens  int     `json:"outputTokens"`
	PaymentSource string  `json:"paymentSource"`
}

type choice struct {
	Index        int    `json:"index"`
	FinishReason string `json:"finish_reason"`
	Message      *struct {
		Role      string       `json:"role"`
		Content   any          `json:"content"`
		ToolCalls []toolCallV1 `json:"tool_calls"`
	} `json:"message,omitempty"`
}

type toolCallV1 struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function *struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// recordFromResponse extracts usage + pricing + tool info from a (non-stream)
// chat completion response and persists a single request row.
//
// The raw body is parsed lazily — anything that fails to decode just yields
// zeros for the metrics (the request still gets a row with status_code).
func (p *Proxy) recordFromResponse(ctx context.Context, key store.APIKey, model string,
	stream bool, status int, latencyMS int, body []byte, clientIP, ua string) {

	rec := store.RequestRecord{
		APIKeyID:   key.ID,
		Model:      model,
		Stream:     stream,
		StatusCode: status,
		LatencyMS:  latencyMS,
		ClientIP:   clientIP,
		UserAgent:  ua,
	}
	if status >= 400 {
		rec.ErrorType = upstreamErrorBucket(status)
		if len(body) > 0 {
			rec.ErrorMessage = firstErrorMessage(body)
		}
	}

	var resp chatCompletionResponse
	if err := json.Unmarshal(body, &resp); err == nil {
		if resp.Usage != nil {
			rec.PromptTokens = resp.Usage.PromptTokens
			rec.CompletionTokens = resp.Usage.CompletionTokens
			rec.ReasoningTokens = resp.Usage.ReasoningTokens
			rec.CacheCreationTokens = resp.Usage.CacheCreationTokens
			rec.CacheReadTokens = resp.Usage.CacheReadTokens
			// Prefer the nested OpenAI-style detail fields when present.
			if resp.Usage.PromptDetails != nil {
				rec.CachedTokens = resp.Usage.PromptDetails.CachedTokens
			} else {
				rec.CachedTokens = resp.Usage.CachedTokens
			}
			if resp.Usage.CompletionDetails != nil && resp.Usage.CompletionDetails.ReasoningTokens != 0 {
				rec.ReasoningTokens = resp.Usage.CompletionDetails.ReasoningTokens
			}
			if rec.TotalTokens == 0 {
				rec.TotalTokens = resp.Usage.TotalTokens
			}
			if rec.TotalTokens == 0 {
				rec.TotalTokens = rec.PromptTokens + rec.CompletionTokens
			}
		}
		if resp.Pricing != nil {
			rec.CostUSD = resp.Pricing.Cost
			rec.PaymentSource = resp.Pricing.PaymentSource
		} else if status < 400 {
			rec.ErrorType = "missing_pricing_info"
		}

		// Tool call aggregation: sum across choices.
		toolsCount := 0
		toolsBad := false
		toolsBadMsg := ""
		for _, ch := range resp.Choices {
			if ch.Message == nil {
				continue
			}
			toolsCount += len(ch.Message.ToolCalls)
			for _, tc := range ch.Message.ToolCalls {
				if tc.Function == nil {
					continue
				}
				// Heuristic: an "error" tool result is one whose arguments
				// couldn't parse as JSON (e.g. the upstream surfaced a
				// failure string). Detect by leading '{' with no closing '}'
				// or any non-JSON control character.
				if tc.Function.Arguments != "" && !looksLikeJSON(tc.Function.Arguments) {
					toolsBad = true
					if toolsBadMsg == "" {
						toolsBadMsg = "tool arguments not valid JSON"
					}
				}
			}
		}
		rec.ToolCallsCount = toolsCount
		rec.HasToolCalls = toolsCount > 0
		rec.ToolError = toolsBad
		rec.ToolErrorMessage = toolsBadMsg
		if toolsBad && rec.ErrorType == "" {
			rec.ErrorType = "tool_error"
		}
	}

	if _, err := p.St.RecordRequest(ctx, rec); err != nil {
		log.Printf("record request: %v", err)
	}
}

// upstreamErrorBucket classifies a non-2xx upstream status for the dashboard.
func upstreamErrorBucket(code int) string {
	switch {
	case code >= 500:
		return "upstream_5xx"
	case code >= 400:
		return "upstream_4xx"
	case code >= 300:
		return "upstream_3xx"
	default:
		return ""
	}
}

// firstErrorMessage extracts the first human-readable message from a non-2xx
// upstream body. Returns "" if nothing usable is found.
func firstErrorMessage(body []byte) string {
	var generic struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &generic); err == nil {
		if generic.Error.Message != "" {
			return generic.Error.Message
		}
		if generic.Error.Type != "" {
			return generic.Error.Type
		}
	}
	if len(body) > 0 && len(body) < 512 {
		return string(body)
	}
	return ""
}

// looksLikeJSON is a cheap, conservative check. Real tool arguments are short
// JSON objects; anything starting with '{' or '[' that doesn't end with the
// matching closer is suspicious.
func looksLikeJSON(s string) bool {
	if s == "" {
		return true
	}
	s = strings.TrimSpace(s)
	first := s[0]
	last := s[len(s)-1]
	switch first {
	case '{':
		return last == '}'
	case '[':
		return last == ']'
	case '"':
		// bare string — valid
		return last == '"'
	default:
		// Numbers, booleans, null — consider valid.
		return true
	}
}