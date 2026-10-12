package providers

import (
	_ "embed"
	"encoding/json"
)

// Kind describes a provider supported by both account setup and model selection.
type Kind struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	BaseURL     string `json:"base_url,omitempty"`
	KeyOptional bool   `json:"key_optional,omitempty"`
}

func (k Kind) Native() bool { return k.ID == Anthropic || k.ID == OpenAI || k.ID == Gemini }

//go:embed catalog.json
var catalogJSON []byte

var Catalog = func() []Kind {
	var kinds []Kind
	if err := json.Unmarshal(catalogJSON, &kinds); err != nil {
		panic("invalid embedded provider catalog")
	}
	return kinds
}()

func Lookup(id string) (Kind, bool) {
	for _, k := range Catalog {
		if k.ID == id {
			return k, true
		}
	}
	return Kind{}, false
}
