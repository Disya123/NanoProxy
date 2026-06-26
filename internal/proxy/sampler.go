// Package proxy implements the public NanoGPT reverse proxy.
//
// SamplerConfig provides per-client-key default generation parameters.
// Each parameter has an Enabled toggle so the proxy only overrides fields
// the operator explicitly chose. A special ForceNoStream flag disables
// SSE streaming for that key entirely.
package proxy

import (
	"encoding/json"
	"log"
)

// SamplerConfig holds the default generation parameters for one API key.
// Each field is a pointer so we can distinguish "not configured" from
// "configured with zero value". Stored as a JSON string in the api_keys
// table's sampler_config column.
type SamplerConfig struct {
	Temperature      *Param[float64]  `json:"temperature,omitempty"`
	TopP             *Param[float64]  `json:"top_p,omitempty"`
	FrequencyPenalty *Param[float64]  `json:"frequency_penalty,omitempty"`
	PresencePenalty  *Param[float64]  `json:"presence_penalty,omitempty"`
	MaxTokens        *Param[int]      `json:"max_tokens,omitempty"`
	Seed             *Param[int]      `json:"seed,omitempty"`
	Stop             *Param[[]string] `json:"stop,omitempty"`
	// ForceNoStream overrides the request's stream field to false regardless
	// of what the client sends. All requests for this key go through the
	// non-stream handler.
	ForceNoStream bool `json:"force_no_stream"`
}

// Param is a typed parameter with an explicit enable toggle.
// When Enabled is false the proxy does not touch this field in the body.
type Param[T any] struct {
	Enabled bool `json:"enabled"`
	Value   T    `json:"value"`
}

// ParseSamplerConfig decodes a JSON string into a SamplerConfig.
// Returns nil (no-op) for empty strings or parse errors.
func ParseSamplerConfig(raw string) *SamplerConfig {
	if raw == "" {
		return nil
	}
	var cfg SamplerConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		log.Printf("[sampler] failed to parse sampler_config: %v", err)
		return nil
	}
	return &cfg
}

// MergeSamplers applies the enabled sampler parameters from cfg into body.
// It returns:
//   - modified body bytes
//   - wasStreamForced: true if ForceNoStream caused stream→false override
//   - error if JSON operations fail
//
// When cfg is nil or has no enabled parameters the body is returned as-is.
func MergeSamplers(body []byte, cfg *SamplerConfig) ([]byte, bool, error) {
	if cfg == nil {
		return body, false, nil
	}

	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return body, false, err
	}

	changed := false

	if cfg.Temperature != nil && cfg.Temperature.Enabled {
		m["temperature"] = cfg.Temperature.Value
		changed = true
	}
	if cfg.TopP != nil && cfg.TopP.Enabled {
		m["top_p"] = cfg.TopP.Value
		changed = true
	}
	if cfg.FrequencyPenalty != nil && cfg.FrequencyPenalty.Enabled {
		m["frequency_penalty"] = cfg.FrequencyPenalty.Value
		changed = true
	}
	if cfg.PresencePenalty != nil && cfg.PresencePenalty.Enabled {
		m["presence_penalty"] = cfg.PresencePenalty.Value
		changed = true
	}
	if cfg.MaxTokens != nil && cfg.MaxTokens.Enabled {
		m["max_tokens"] = cfg.MaxTokens.Value
		changed = true
	}
	if cfg.Seed != nil && cfg.Seed.Enabled {
		m["seed"] = cfg.Seed.Value
		changed = true
	}
	if cfg.Stop != nil && cfg.Stop.Enabled && len(cfg.Stop.Value) > 0 {
		m["stop"] = cfg.Stop.Value
		changed = true
	}

	wasStreamForced := false
	if cfg.ForceNoStream {
		if streamVal, ok := m["stream"]; ok {
			if b, ok := streamVal.(bool); ok && b {
				wasStreamForced = true
			}
		}
		m["stream"] = false
		changed = true
	}

	if !changed {
		return body, false, nil
	}

	out, err := json.Marshal(m)
	if err != nil {
		return body, false, err
	}
	return out, wasStreamForced, nil
}
