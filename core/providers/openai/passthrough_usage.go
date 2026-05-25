package openai

import (
	"bytes"
	"mime/multipart"
	"strconv"
	"strings"

	"github.com/bytedance/sonic"
	providerUtils "github.com/maximhq/bifrost/core/providers/utils"
	"github.com/maximhq/bifrost/core/schemas"
)

// ExtractOpenAIPassthroughUsage extracts usage from a completed OpenAI/Azure
// passthrough response. path is the stripped request path, reqBody is the original
// request body (needed for speech char count and image parameters), accBody is the
// full accumulated response body.
func ExtractOpenAIPassthroughUsage(path string, reqBody, accBody []byte) *schemas.BifrostPassthroughUsage {
	if idx := strings.IndexByte(path, '?'); idx >= 0 {
		path = path[:idx]
	}

	switch {
	case strings.HasSuffix(path, "/chat/completions"),
		strings.HasSuffix(path, "/completions"):
		return extractOAIChatUsage(accBody)

	case strings.HasSuffix(path, "/responses"):
		return extractOAIResponsesUsage(accBody)

	case strings.HasSuffix(path, "/embeddings"):
		return extractOAIEmbeddingUsage(accBody)

	case strings.HasSuffix(path, "/audio/speech"):
		return extractOAISpeechUsage(reqBody)

	case strings.HasSuffix(path, "/audio/transcriptions"),
		strings.HasSuffix(path, "/audio/translations"):
		return extractOAITranscriptionUsage(accBody)

	case strings.HasSuffix(path, "/images/generations"),
		strings.HasSuffix(path, "/images/edits"),
		strings.HasSuffix(path, "/images/variations"):
		return extractOAIImageUsage(reqBody, accBody)

	case strings.Contains(path, "/video"):
		return extractOAIVideoUsage(reqBody)

	case strings.HasSuffix(path, "/containers"):
		// Collection path serves both create (POST, billable) and list (GET, free);
		// extractOAIContainerUsage disambiguates by response shape. Retrieve/delete use
		// /containers/{id} and never match this suffix.
		return extractOAIContainerUsage(accBody)
	}

	return nil
}

// ---- video generation ----
const openAIVideoDefaultSeconds = 4

func extractOAIVideoUsage(reqBody []byte) *schemas.BifrostPassthroughUsage {
	secs := openAIVideoDefaultSeconds
	if len(reqBody) > 0 {
		firstLine, _, _ := bytes.Cut(reqBody, []byte("\n"))
		boundary := strings.TrimRight(
			strings.TrimPrefix(string(firstLine), "--"), "\r")
		if boundary != "" {
			mr := multipart.NewReader(bytes.NewReader(reqBody), boundary)
			if form, err := mr.ReadForm(32 << 20); err == nil {
				// ReadForm spills parts over maxMemory to temp files; clean them up.
				defer form.RemoveAll()
				if values := form.Value["seconds"]; len(values) > 0 && values[0] != "" {
					if f, parseErr := strconv.ParseFloat(values[0], 64); parseErr == nil && f > 0 {
						secs = int(f)
					}
				}
			}
		}
	}
	return &schemas.BifrostPassthroughUsage{VideoSeconds: &secs}
}

// ---- chat / text completions ----
// BifrostLLMUsage is OpenAI-compatible so we can unmarshal directly.

type oaiChatUsageWrapper struct {
	Usage       *schemas.BifrostLLMUsage `json:"usage"`
	ServiceTier *string                  `json:"service_tier"`
}

func extractOAIChatUsage(accBody []byte) *schemas.BifrostPassthroughUsage {
	body := providerUtils.LastSSEOrBody(accBody)
	if body == nil {
		return nil
	}
	var w oaiChatUsageWrapper
	if err := sonic.Unmarshal(body, &w); err != nil || w.Usage == nil || w.Usage.TotalTokens == 0 {
		return nil
	}
	u := &schemas.BifrostPassthroughUsage{LLMUsage: w.Usage}
	if w.ServiceTier != nil {
		t := schemas.BifrostServiceTier(*w.ServiceTier)
		u.ServiceTier = &t
	}
	return u
}

// ---- responses API ----
// A single wrapper handles both response formats in one unmarshal pass:
//   - streaming: "response.completed" event nests usage under "response"
//   - non-streaming: usage sits at the top level
type oaiResponsesWrapper struct {
	Response *struct {
		Usage       *schemas.ResponsesResponseUsage `json:"usage"`
		ServiceTier *string                         `json:"service_tier"`
	} `json:"response"`
	Usage       *schemas.ResponsesResponseUsage `json:"usage"`
	ServiceTier *string                         `json:"service_tier"`
}

func extractOAIResponsesUsage(accBody []byte) *schemas.BifrostPassthroughUsage {
	body := providerUtils.LastSSEOrBody(accBody)
	if body == nil {
		return nil
	}

	var w oaiResponsesWrapper
	if err := sonic.Unmarshal(body, &w); err != nil {
		return nil
	}

	// Streaming takes priority: nested under "response" with a non-zero total.
	ru, tier := w.Usage, w.ServiceTier
	if w.Response != nil && w.Response.Usage != nil && w.Response.Usage.TotalTokens > 0 {
		ru, tier = w.Response.Usage, w.Response.ServiceTier
	}
	if ru == nil || ru.TotalTokens == 0 {
		return nil
	}
	return buildOAIResponsesUsage(ru, tier)
}

func buildOAIResponsesUsage(ru *schemas.ResponsesResponseUsage, serviceTier *string) *schemas.BifrostPassthroughUsage {
	usage := &schemas.BifrostLLMUsage{
		PromptTokens:     ru.InputTokens,
		CompletionTokens: ru.OutputTokens,
		TotalTokens:      ru.TotalTokens,
	}
	if ru.InputTokensDetails != nil {
		usage.PromptTokensDetails = &schemas.ChatPromptTokensDetails{
			CachedReadTokens:  ru.InputTokensDetails.CachedReadTokens,
			CachedWriteTokens: ru.InputTokensDetails.CachedWriteTokens,
		}
	}
	if ru.OutputTokensDetails != nil {
		usage.CompletionTokensDetails = &schemas.ChatCompletionTokensDetails{
			ReasoningTokens: ru.OutputTokensDetails.ReasoningTokens,
		}
		if ru.OutputTokensDetails.NumSearchQueries != nil {
			usage.CompletionTokensDetails.NumSearchQueries = ru.OutputTokensDetails.NumSearchQueries
		}
	}
	u := &schemas.BifrostPassthroughUsage{LLMUsage: usage}
	if serviceTier != nil {
		t := schemas.BifrostServiceTier(*serviceTier)
		u.ServiceTier = &t
	}
	return u
}

// ---- embeddings ----
// Embeddings are not typically streamed; accBody is plain JSON.

func extractOAIEmbeddingUsage(accBody []byte) *schemas.BifrostPassthroughUsage {
	var w oaiChatUsageWrapper
	if err := sonic.Unmarshal(accBody, &w); err != nil || w.Usage == nil || w.Usage.TotalTokens == 0 {
		return nil
	}
	return &schemas.BifrostPassthroughUsage{LLMUsage: w.Usage}
}

// ---- speech (TTS) ----
// Response is binary audio; pricing is based on input character count from the request.

func extractOAISpeechUsage(reqBody []byte) *schemas.BifrostPassthroughUsage {
	if len(reqBody) == 0 {
		return nil
	}
	var req OpenAISpeechRequest
	if err := sonic.Unmarshal(reqBody, &req); err != nil || req.Input == "" {
		return nil
	}
	return &schemas.BifrostPassthroughUsage{
		AudioInputChars: len([]rune(req.Input)),
	}
}

// ---- transcription / translation ----

type oaiTranscriptionResponseWrapper struct {
	Usage    *schemas.TranscriptionUsage `json:"usage"`
	Duration float64                     `json:"duration"` // seconds fallback for older models
}

func extractOAITranscriptionUsage(accBody []byte) *schemas.BifrostPassthroughUsage {
	var r oaiTranscriptionResponseWrapper
	if err := sonic.Unmarshal(accBody, &r); err != nil {
		return nil
	}
	u := &schemas.BifrostPassthroughUsage{}
	if r.Usage != nil && r.Usage.TotalTokens != nil && *r.Usage.TotalTokens > 0 {
		promptTokens := 0
		if r.Usage.InputTokens != nil {
			promptTokens = *r.Usage.InputTokens
		}
		u.LLMUsage = &schemas.BifrostLLMUsage{
			PromptTokens: promptTokens,
			TotalTokens:  *r.Usage.TotalTokens,
		}
		if r.Usage.InputTokenDetails != nil {
			u.AudioTokenDetails = &schemas.TranscriptionUsageInputTokenDetails{
				AudioTokens: r.Usage.InputTokenDetails.AudioTokens,
				TextTokens:  r.Usage.InputTokenDetails.TextTokens,
			}
		}
		u.AudioSeconds = r.Usage.Seconds
	} else if r.Duration > 0 {
		secs := int(r.Duration)
		u.AudioSeconds = &secs
	}
	if u.LLMUsage == nil && u.AudioSeconds == nil {
		return nil
	}
	return u
}

// ---- image generation / edit / variation ----
// Size, Quality, N come from the request body; usage/data count from the response.

func extractOAIImageUsage(reqBody, accBody []byte) *schemas.BifrostPassthroughUsage {
	u := &schemas.BifrostPassthroughUsage{}

	// Request body: size, quality, n — reuse OpenAIImageGenerationRequest which embeds
	// schemas.ImageGenerationParameters (N, Size, Quality).
	if len(reqBody) > 0 {
		var req OpenAIImageGenerationRequest
		if err := sonic.Unmarshal(reqBody, &req); err == nil {
			if req.Size != nil {
				u.ImageSize = *req.Size
			}
			if req.Quality != nil {
				u.ImageQuality = *req.Quality
			}
			if req.N != nil && *req.N > 0 {
				if u.ImageUsage == nil {
					u.ImageUsage = &schemas.ImageUsage{}
				}
				if u.ImageUsage.OutputTokensDetails == nil {
					u.ImageUsage.OutputTokensDetails = &schemas.ImageTokenDetails{}
				}
				u.ImageUsage.OutputTokensDetails.NImages = *req.N
			}
		}
	}

	// Response body: use OpenAIImageStreamResponse (streaming SSE event) or fall back to
	// plain JSON for non-streaming passthrough routes.
	if body := providerUtils.LastSSEOrBody(accBody); body != nil {
		var resp OpenAIImageStreamResponse
		if err := sonic.Unmarshal(body, &resp); err == nil {
			if resp.Usage != nil {
				u.ImageUsage = resp.Usage
			}
			if resp.Size != "" && u.ImageSize == "" {
				u.ImageSize = resp.Size
			}
			if resp.Quality != "" && u.ImageQuality == "" {
				u.ImageQuality = resp.Quality
			}
		}
		// Mirror the native path (populateOutputImageCount): count delivered images from
		// the `data` array when the request didn't specify n and no token usage was
		// returned (e.g. DALL·E, which has no usage block).
		if dataLen := int(providerUtils.GetJSONField(body, "data.#").Int()); dataLen > 0 {
			if u.ImageUsage == nil {
				u.ImageUsage = &schemas.ImageUsage{}
			}
			if u.ImageUsage.OutputTokensDetails == nil {
				u.ImageUsage.OutputTokensDetails = &schemas.ImageTokenDetails{}
			}
			if u.ImageUsage.OutputTokensDetails.NImages == 0 {
				u.ImageUsage.OutputTokensDetails.NImages = dataLen
			}
		}
	}

	if u.ImageUsage == nil {
		u.ImageUsage = &schemas.ImageUsage{}
	}
	// Populate LLMUsage from image token counts so logs show token totals.
	if u.ImageUsage.TotalTokens > 0 {
		u.LLMUsage = &schemas.BifrostLLMUsage{
			PromptTokens:     u.ImageUsage.InputTokens,
			CompletionTokens: u.ImageUsage.OutputTokens,
			TotalTokens:      u.ImageUsage.TotalTokens,
		}
	}
	return u
}

// ---- containers (code interpreter sessions) ----
// Only the create call (POST /v1/containers) is billable — a flat per-session fee priced
// under the synthetic "container-{memory_limit}" model key (falling back to "container").
// The collection path also serves list (GET /v1/containers), so disambiguate by response
// shape: a create returns a single {"object":"container", "id":...} object, while a list
// returns {"object":"list", "data":[...]}. Retrieve/delete hit /containers/{id} and never
// reach this extractor. Containers are never streamed, so accBody is plain JSON.

func extractOAIContainerUsage(accBody []byte) *schemas.BifrostPassthroughUsage {
	if len(accBody) == 0 {
		return nil
	}
	var resp struct {
		Object      string `json:"object"`
		ID          string `json:"id"`
		MemoryLimit string `json:"memory_limit"`
	}
	if err := sonic.Unmarshal(accBody, &resp); err != nil {
		return nil
	}
	// Bill only a created container (single object), not list/other shapes.
	if resp.Object != "container" || resp.ID == "" {
		return nil
	}
	identifier := "container"
	if resp.MemoryLimit != "" {
		identifier = "container-" + resp.MemoryLimit
	}
	return &schemas.BifrostPassthroughUsage{ContainerIdentifier: identifier}
}
