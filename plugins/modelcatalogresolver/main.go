// Package modelcatalogresolver provides a built-in PreRequestHook plugin that resolves
// the default provider for an unprefixed model via the model catalog. It is the single
// owner of "if no provider specified, look up which providers serve this model" — the
// transport handlers, integrations router, and realtime handlers no longer do this
// inline. Governance/LB plugins run before this resolver; it only fires as a final
// fallback when no earlier routing plugin picked a provider.
package modelcatalogresolver

import (
	"fmt"
	"slices"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/modelcatalog"
)

const PluginName = "model-catalog-resolver"

// integrationTypeToDefaultProvider maps the integration-type ctx value (set by
// transports/bifrost-http/integrations/router.go on integration routes) to the
// integration's canonical provider. When the catalog returns multiple providers
// for an unprefixed model, the resolver prefers the integration's canonical
// provider if it's in the candidate list.
var integrationTypeToDefaultProvider = map[string]schemas.ModelProvider{
	"openai":    schemas.OpenAI,
	"anthropic": schemas.Anthropic,
	"genai":     schemas.Gemini,
	"bedrock":   schemas.Bedrock,
	"cohere":    schemas.Cohere,
}

// Plugin resolves the default provider for unprefixed model strings using the model catalog.
type Plugin struct {
	catalog *modelcatalog.ModelCatalog
	logger  schemas.Logger
}

// Init returns a new resolver plugin. The catalog is required; if nil, the plugin returns
// an error rather than silently no-op'ing — a nil catalog at boot is a misconfiguration.
func Init(catalog *modelcatalog.ModelCatalog, logger schemas.Logger) (*Plugin, error) {
	if catalog == nil {
		return nil, fmt.Errorf("model-catalog-resolver: catalog is required")
	}
	return &Plugin{catalog: catalog, logger: logger}, nil
}

// GetName implements schemas.BasePlugin.
func (p *Plugin) GetName() string { return PluginName }

// Cleanup implements schemas.BasePlugin.
func (p *Plugin) Cleanup() error { return nil }

// PreRequestHook fills in req.Provider from the model catalog when no provider was specified.
// Skips passthrough requests and requests that already have a provider set (e.g., from a model
// string like "openai/gpt-5", or from an earlier routing plugin — governance, LB).
//
// When the catalog returns multiple providers for an unprefixed model, the resolver prefers the
// integration's canonical provider (looked up from BifrostContextKeyIntegrationType set by the
// integration router) if it's in the candidate list. Otherwise it picks the first candidate.
//
// If the catalog returns zero providers, the resolver leaves req.Provider empty — the
// empty-provider validation in handleRequest/handleStreamRequest then returns a clear error.
func (p *Plugin) PreRequestHook(ctx *schemas.BifrostContext, req *schemas.BifrostRequest) error {
	if req.RequestType == schemas.PassthroughRequest || req.RequestType == schemas.PassthroughStreamRequest {
		return nil
	}
	provider, model, _ := req.GetRequestFields()
	if provider != "" || model == "" {
		return nil
	}

	providers := p.catalog.GetProvidersForModel(model)
	if len(providers) == 0 {
		return nil
	}

	// Respect the routing-allowlist set by an earlier plugin (e.g., governance VK governance):
	// intersect catalog candidates with the allowlist so the VK's provider restrictions hold
	// even when no earlier routing plugin set req.Provider.
	if allowed, ok := ctx.Value(schemas.BifrostContextKeyRoutingAllowedProviders).([]schemas.ModelProvider); ok {
		filtered := providers[:0:0]
		for _, prov := range providers {
			if slices.Contains(allowed, prov) {
				filtered = append(filtered, prov)
			}
		}
		providers = filtered
		if len(providers) == 0 {
			ctx.AppendRoutingEngineLog(schemas.RoutingEngineModelCatalog, schemas.LogLevelInfo, fmt.Sprintf(
				"No catalog providers for model %s remain after routing-allowlist filter %v; leaving req.Provider empty",
				model, allowed,
			))
			return nil
		}
	}

	selected := providers[0]
	if integrationType, ok := ctx.Value(schemas.BifrostContextKeyIntegrationType).(string); ok && integrationType != "" {
		if integrationDefault, mapped := integrationTypeToDefaultProvider[integrationType]; mapped && integrationDefault != "" {
			if slices.Contains(providers, integrationDefault) {
				selected = integrationDefault
			}
		}
	}
	req.SetProvider(selected)

	providerStrs := make([]string, len(providers))
	for i, prov := range providers {
		providerStrs[i] = string(prov)
	}
	ctx.AppendRoutingEngineLog(schemas.RoutingEngineModelCatalog, schemas.LogLevelInfo, fmt.Sprintf(
		"No provider specified for model %s, found %d options in model catalog: [%s], selected: %s",
		model, len(providers), strings.Join(providerStrs, ", "), selected,
	))
	schemas.AppendToContextList(ctx, schemas.BifrostContextKeyRoutingEnginesUsed, schemas.RoutingEngineModelCatalog)
	return nil
}

// PreLLMHook implements schemas.LLMPlugin (no-op).
func (p *Plugin) PreLLMHook(_ *schemas.BifrostContext, req *schemas.BifrostRequest) (*schemas.BifrostRequest, *schemas.LLMPluginShortCircuit, error) {
	return req, nil, nil
}

// PostLLMHook implements schemas.LLMPlugin (no-op).
func (p *Plugin) PostLLMHook(_ *schemas.BifrostContext, resp *schemas.BifrostResponse, bifrostErr *schemas.BifrostError) (*schemas.BifrostResponse, *schemas.BifrostError, error) {
	return resp, bifrostErr, nil
}
