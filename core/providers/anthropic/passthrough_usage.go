package anthropic

import (
	"strings"

	"github.com/bytedance/sonic"
	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
)

// ExtractAnthropicPassthroughUsage extracts usage from a completed Anthropic
// passthrough response. path is the stripped request path, reqBody is the original
// request body, accBody is the full accumulated response body.
func ExtractAnthropicPassthroughUsage(path string, _, accBody []byte) *schemas.BifrostPassthroughUsage {
	if idx := strings.IndexByte(path, '?'); idx >= 0 {
		path = path[:idx]
	}

	switch {
	case strings.HasSuffix(path, "/messages"):
		return extractAnthropicMessagesUsage(accBody)
	case strings.HasSuffix(path, "/complete"):
		return extractAnthropicCompleteUsage(accBody)
	}
	return nil
}

// buildAnthropicPassthroughUsage converts AnthropicUsage directly into BifrostPassthroughUsage.
func buildAnthropicPassthroughUsage(au *AnthropicUsage) *schemas.BifrostPassthroughUsage {
	if au == nil {
		return nil
	}
	totalInput := au.InputTokens + au.CacheReadInputTokens + au.CacheCreationInputTokens
	total := totalInput + au.OutputTokens
	if total == 0 {
		return nil
	}

	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     totalInput,
		CompletionTokens: au.OutputTokens,
		TotalTokens:      total,
	}

	if au.CacheReadInputTokens > 0 || au.CacheCreationInputTokens > 0 {
		details := &schemas.ChatPromptTokensDetails{
			CachedReadTokens:  au.CacheReadInputTokens,
			CachedWriteTokens: au.CacheCreationInputTokens,
		}
		if au.CacheCreation.Ephemeral5mInputTokens > 0 || au.CacheCreation.Ephemeral1hInputTokens > 0 {
			details.CachedWriteTokenDetails = &schemas.ChatCachedWriteTokenDetails{
				CachedWriteTokens5m: au.CacheCreation.Ephemeral5mInputTokens,
				CachedWriteTokens1h: au.CacheCreation.Ephemeral1hInputTokens,
			}
		}
		usage.PromptTokensDetails = details
	}

	if au.ServerToolUse != nil && au.ServerToolUse.WebSearchRequests > 0 {
		n := au.ServerToolUse.WebSearchRequests
		usage.CompletionTokensDetails = &schemas.ChatCompletionTokensDetails{
			NumSearchQueries: &n,
		}
	}

	u := &schemas.BifrostPassthroughUsage{LLMUsage: usage}
	if au.ServiceTier != nil {
		t := MapAnthropicServiceTierToBifrost(*au.ServiceTier)
		u.ServiceTier = &t
	}
	return u
}

// extractAnthropicMessagesUsage assembles usage from the /v1/messages response.
// Anthropic SSE streams split usage across two events:
//   - message_start: input tokens (including cache creation/read tokens)
//   - message_delta: output tokens
//
// Plain JSON non-streaming responses carry the full usage block at the top level.
func extractAnthropicMessagesUsage(accBody []byte) *schemas.BifrostPassthroughUsage {
	lines := providerUtils.ScanSSEDataLines(accBody)
	if len(lines) == 0 {
		// Not SSE — plain JSON non-streaming response.
		return extractAnthropicMessagesNonStream(accBody)
	}

	combined := &AnthropicUsage{}
	for _, line := range lines {
		var evt AnthropicStreamEvent
		if err := sonic.Unmarshal(line, &evt); err != nil {
			continue
		}
		switch evt.Type {
		case AnthropicStreamEventTypeMessageStart:
			// message_start.message.usage has input tokens + cache tokens
			if evt.Message != nil && evt.Message.Usage != nil {
				u := evt.Message.Usage
				combined.InputTokens = u.InputTokens
				combined.CacheCreationInputTokens = u.CacheCreationInputTokens
				combined.CacheReadInputTokens = u.CacheReadInputTokens
				combined.CacheCreation = u.CacheCreation
				combined.ServiceTier = u.ServiceTier
			}
		case AnthropicStreamEventTypeMessageDelta:
			// message_delta.usage has output tokens
			if evt.Usage != nil {
				combined.OutputTokens = evt.Usage.OutputTokens
				if evt.Usage.ServerToolUse != nil {
					combined.ServerToolUse = evt.Usage.ServerToolUse
				}
			}
		}
	}

	return buildAnthropicPassthroughUsage(combined)
}

// extractAnthropicMessagesNonStream handles plain JSON /v1/messages responses.
func extractAnthropicMessagesNonStream(body []byte) *schemas.BifrostPassthroughUsage {
	var resp AnthropicMessageResponse
	if err := sonic.Unmarshal(body, &resp); err != nil || resp.Usage == nil {
		return nil
	}
	return buildAnthropicPassthroughUsage(resp.Usage)
}

// extractAnthropicCompleteUsage handles the legacy /v1/complete endpoint.
func extractAnthropicCompleteUsage(accBody []byte) *schemas.BifrostPassthroughUsage {
	body := providerUtils.LastSSEOrBody(accBody)
	if body == nil {
		return nil
	}
	var resp struct {
		Usage *struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := sonic.Unmarshal(body, &resp); err != nil || resp.Usage == nil {
		return nil
	}
	total := resp.Usage.InputTokens + resp.Usage.OutputTokens
	if total == 0 {
		return nil
	}
	return &schemas.BifrostPassthroughUsage{
		LLMUsage: &schemas.BifrostLLMUsage{
			PromptTokens:     resp.Usage.InputTokens,
			CompletionTokens: resp.Usage.OutputTokens,
			TotalTokens:      total,
		},
	}
}
