package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/local/nano-proxy/internal/store"
)

// streamAccumulator collects usage, pricing, tool_calls, and finish_reason
// across all SSE chunks in a streamed chat-completion response.
//
// Memory footprint: one small struct + a map of tool-call parts keyed by
// index. The full response content is never copied here.
type streamAccumulator struct {
	usage          *usageBlock
	pricing        *pricingBlk
	finishReason   string
	toolParts      map[int]*toolCallStream // keyed by tool-call index
	hasErrorInBody bool
}

type toolCallStream struct {
	ID        string
	Name      string
	ArgsBuf   bytes.Buffer
	ErrorFlag bool
}

// handleStream implements the streaming path for /v1/chat/completions.
//
// Streaming strategy:
//   - Each upstream SSE chunk is forwarded to the client immediately,
//     then parsed in-place to update the accumulator.
//   - We never accumulate `delta.content` — that's the entire content of
//     a 100k-token response and would defeat the point of streaming.
//   - We DO accumulate `tool_calls[*].function.arguments` because broken
//     tool args must surface as tool errors in the dashboard.
func (p *Proxy) handleStream(w http.ResponseWriter, r *http.Request,
	key store.APIKey, body []byte, model string) {

	upstreamKey := p.Keys.Get()
	if upstreamKey == "" {
		writeProxyError(w, http.StatusBadGateway, "upstream_key_missing",
			"upstream NanoGPT API key is not configured — set it via the admin dashboard")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), p.Cfg.Upstream.RequestTimeout)
	defer cancel()

	upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.upstreamURL("/chat/completions"), bytes.NewReader(body))
	if err != nil {
		writeProxyError(w, http.StatusInternalServerError, "request_build_error", err.Error())
		return
	}
	copyHeaders(upstreamReq.Header, r.Header, "")
	upstreamReq.Header.Set("Authorization", "Bearer "+upstreamKey)
	upstreamReq.Header.Set("Content-Type", "application/json")
	upstreamReq.Header.Set("Accept", "text/event-stream")

	resp, err := p.getUpstream().Do(upstreamReq)
	if err != nil {
		writeProxyError(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	defer resp.Body.Close()

	copyHeaders(w.Header(), resp.Header, "Content-Length", "Content-Encoding")
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)

	acc := &streamAccumulator{toolParts: make(map[int]*toolCallStream)}
	streamErr := relayAndAccumulate(resp.Body, w, flusher, acc)

	ip, ua := clientMeta(r)
	rec := store.RequestRecord{
		APIKeyID:   key.ID,
		Model:      model,
		Stream:     true,
		StatusCode: resp.StatusCode,
		ClientIP:   ip,
		UserAgent:  ua,
	}
	applyAccumulator(acc, &rec)
	if streamErr != nil {
		rec.ErrorType = "stream_aborted"
		rec.ErrorMessage = streamErr.Error()
	}
	if rec.StatusCode >= 400 && rec.ErrorType == "" {
		rec.ErrorType = upstreamErrorBucket(rec.StatusCode)
	}
	if _, err := p.St.RecordRequest(context.Background(), rec); err != nil {
		// Telemetry failures must never fail the already-sent response.
		_ = err
	}
}

// relayAndAccumulate copies SSE chunks from src to dst and updates acc with
// usage, pricing, tool_calls, and finish_reason parsed from each `data:` line.
//
// Returns the stream error (io.EOF or transport error), or nil if the stream
// ended cleanly with [DONE].
func relayAndAccumulate(src io.Reader, dst io.Writer, flusher http.Flusher,
	acc *streamAccumulator) error {

	buf := make([]byte, 4096)
	var pending []byte

	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			if _, werr := dst.Write(chunk); werr != nil {
				return werr
			}
			if flusher != nil {
				flusher.Flush()
			}
			pending = append(pending, chunk...)

			// Process complete SSE lines.
			for {
				lineEnd := indexNewline(pending)
				if lineEnd < 0 {
					break
				}
				line := pending[:lineEnd]
				pending = pending[lineEnd+1:]

				// Strip trailing \r if present.
				if len(line) > 0 && line[len(line)-1] == '\r' {
					line = line[:len(line)-1]
				}
				// Skip event: lines and empty lines — we only care about data:.
				if len(line) == 0 || line[0] == ':' {
					continue
				}
				const dataPrefix = "data:"
				if len(line) < len(dataPrefix) {
					continue
				}
				if !bytes.Equal(line[:len(dataPrefix)], []byte(dataPrefix)) {
					continue
				}
				payload := bytes.TrimSpace(line[len(dataPrefix):])
				if len(payload) == 0 {
					continue
				}
				if bytes.Equal(payload, []byte("[DONE]")) {
					return nil
				}
				accumulateOneChunk(payload, acc)
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return nil
			}
			return rerr
		}
	}
}

// accumulateOneChunk parses a single SSE data payload into the accumulator.
func accumulateOneChunk(payload []byte, acc *streamAccumulator) {
	if len(payload) == 0 || payload[0] != '{' {
		return
	}
	var raw struct {
		Usage   *usageBlock `json:"usage"`
		Pricing *pricingBlk `json:"x_nanogpt_pricing"`
		Error   *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Code    any    `json:"code"`
		} `json:"error"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Delta        *struct {
				ToolCalls []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function *struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
				Content string `json:"content"`
			} `json:"delta"`
			Message *struct {
				ToolCalls []toolCallV1 `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(payload, &raw); err != nil {
		return
	}
	if raw.Usage != nil {
		acc.usage = raw.Usage
	}
	if raw.Pricing != nil {
		acc.pricing = raw.Pricing
	}
	if raw.Error != nil && (raw.Error.Type != "" || raw.Error.Message != "") {
		acc.hasErrorInBody = true
	}
	for _, ch := range raw.Choices {
		if ch.FinishReason != "" {
			acc.finishReason = ch.FinishReason
		}
		if ch.Delta != nil {
			for _, tc := range ch.Delta.ToolCalls {
				part, ok := acc.toolParts[tc.Index]
				if !ok {
					part = &toolCallStream{ID: tc.ID, Type: tc.Type}
					acc.toolParts[tc.Index] = part
				}
				if tc.ID != "" {
					part.ID = tc.ID
				}
				if tc.Type != "" {
					part.Type = tc.Type
				}
				if tc.Function != nil {
					if tc.Function.Name != "" {
						part.Name = tc.Function.Name
					}
					part.ArgsBuf.WriteString(tc.Function.Arguments)
				}
			}
		}
		if ch.Message != nil {
			for _, tc := range ch.Message.ToolCalls {
				part, ok := acc.toolParts[0] // message-level tools live at slot 0
				if !ok {
					part = &toolCallStream{}
				}
				part.ID = tc.ID
				if tc.Function != nil {
					part.Name = tc.Function.Name
					if tc.Function.Arguments != "" {
						part.ArgsBuf.WriteString(tc.Function.Arguments)
					}
				}
				acc.toolParts[0] = part
			}
		}
	}
}

// applyAccumulator fills the store.RequestRecord from the accumulator.
func applyAccumulator(acc *streamAccumulator, rec *store.RequestRecord) {
	if acc.usage != nil {
		u := acc.usage
		rec.PromptTokens = u.PromptTokens
		rec.CompletionTokens = u.CompletionTokens
		rec.TotalTokens = u.TotalTokens
		rec.ReasoningTokens = u.ReasoningTokens
		rec.CacheCreationTokens = u.CacheCreationTokens
		rec.CacheReadTokens = u.CacheReadTokens
		if u.PromptDetails != nil {
			rec.CachedTokens = u.PromptDetails.CachedTokens
		} else {
			rec.CachedTokens = u.CachedTokens
		}
		if u.CompletionDetails != nil && u.CompletionDetails.ReasoningTokens != 0 {
			rec.ReasoningTokens = u.CompletionDetails.ReasoningTokens
		}
		if rec.TotalTokens == 0 {
			rec.TotalTokens = rec.PromptTokens + rec.CompletionTokens
		}
	}
	if acc.pricing != nil {
		rec.CostUSD = acc.pricing.Cost
		rec.PaymentSource = acc.pricing.PaymentSource
	} else if rec.StatusCode < 400 {
		rec.ErrorType = "missing_pricing_info"
	}

	rec.ToolCallsCount = len(acc.toolParts)
	rec.HasToolCalls = rec.ToolCallsCount > 0

	// Tool errors: any tool with non-JSON arguments, OR a finish_reason that
	// indicates a problem while tools were requested.
	for _, part := range acc.toolParts {
		if part == nil {
			continue
		}
		argStr := part.ArgsBuf.String()
		if argStr != "" && !looksLikeJSON(argStr) {
			part.ErrorFlag = true
		}
		if part.ErrorFlag {
			rec.ToolError = true
			if rec.ToolErrorMessage == "" {
				rec.ToolErrorMessage = "tool arguments not valid JSON"
			}
		}
	}
	if acc.finishReason == "tool_calls" && rec.ToolError {
		// already flagged
	}
	if rec.ToolError && rec.ErrorType == "" {
		rec.ErrorType = "tool_error"
	}
	if acc.hasErrorInBody && rec.ErrorType == "" {
		rec.ErrorType = "stream_body_error"
	}
}

// ─────────────────────────── helpers ───────────────────────────

func indexNewline(b []byte) int {
	for i, c := range b {
		if c == '\n' {
			return i
		}
	}
	return -1
}