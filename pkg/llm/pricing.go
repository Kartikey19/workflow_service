package llm

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Price is dollars per million tokens.
type Price struct {
	InputPerMTok  float64 `json:"input_per_mtok"`
	OutputPerMTok float64 `json:"output_per_mtok"`
}

// Pricing maps a model name, or a prefix of one, to its Price. The longest
// matching key wins, so "claude-sonnet" prices a whole family and an exact
// model name overrides it.
type Pricing map[string]Price

// DefaultPricing holds estimates per Anthropic model family. They are only
// estimates: override them with LLM_PRICING_JSON to match the real price list.
func DefaultPricing() Pricing {
	return Pricing{
		"claude-fable":  {InputPerMTok: 15, OutputPerMTok: 75},
		"claude-opus":   {InputPerMTok: 5, OutputPerMTok: 25},
		"claude-sonnet": {InputPerMTok: 3, OutputPerMTok: 15},
		"claude-haiku":  {InputPerMTok: 1, OutputPerMTok: 5},
	}
}

// ParsePricing decodes a JSON object of model to Price and merges it over the
// defaults. An empty text yields the defaults.
func ParsePricing(text string) (Pricing, error) {
	pricing := DefaultPricing()
	if strings.TrimSpace(text) == "" {
		return pricing, nil
	}
	var overrides map[string]Price
	if err := json.Unmarshal([]byte(text), &overrides); err != nil {
		return nil, fmt.Errorf("decode pricing: %w", err)
	}
	for model, price := range overrides {
		if price.InputPerMTok < 0 || price.OutputPerMTok < 0 {
			return nil, fmt.Errorf("pricing for %q is negative", model)
		}
		pricing[model] = price
	}
	return pricing, nil
}

// Cost returns the estimated dollar cost of usage on model and whether the
// model has a known price.
func (p Pricing) Cost(model string, usage Usage) (float64, bool) {
	best, found := "", false
	for key := range p {
		if strings.HasPrefix(model, key) && (!found || len(key) > len(best)) {
			best, found = key, true
		}
	}
	if !found {
		return 0, false
	}
	price := p[best]
	return (float64(usage.InputTokens)*price.InputPerMTok + float64(usage.OutputTokens)*price.OutputPerMTok) / 1e6, true
}
