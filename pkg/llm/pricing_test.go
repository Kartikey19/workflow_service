package llm

import (
	"math"
	"testing"
)

func TestCostUsesLongestMatchingPrefix(t *testing.T) {
	p, err := ParsePricing(`{"claude-sonnet-5-5": {"input_per_mtok": 2, "output_per_mtok": 10}}`)
	if err != nil {
		t.Fatal(err)
	}
	usage := Usage{InputTokens: 1_000_000, OutputTokens: 500_000}
	if cost, ok := p.Cost("claude-sonnet-5-5", usage); !ok || math.Abs(cost-7) > 1e-9 {
		t.Fatalf("exact override = %v %v, want 7", cost, ok)
	}
	if cost, ok := p.Cost("claude-sonnet-5", usage); !ok || math.Abs(cost-10.5) > 1e-9 {
		t.Fatalf("family default = %v %v, want 10.5", cost, ok)
	}
	if _, ok := p.Cost("gpt-x", usage); ok {
		t.Fatal("unknown model reported a price")
	}
}

func TestParsePricingRejectsBadInput(t *testing.T) {
	for _, text := range []string{`not json`, `{"m": {"input_per_mtok": -1}}`} {
		if _, err := ParsePricing(text); err == nil {
			t.Fatalf("ParsePricing(%q) = nil error", text)
		}
	}
	if p, err := ParsePricing(" "); err != nil || len(p) == 0 {
		t.Fatalf("empty text = %v, %v; want defaults", p, err)
	}
}
