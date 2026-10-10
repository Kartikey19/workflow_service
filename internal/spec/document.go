package spec

type Document struct {
	APIVersion  string                `yaml:"apiVersion" json:"apiVersion"`
	Name        string                `yaml:"name" json:"name"`
	Description string                `yaml:"description" json:"description"`
	Inputs      map[string]InputField `yaml:"inputs" json:"inputs"`
	Correlation string                `yaml:"correlation" json:"correlation"`
	Timeout     *string               `yaml:"timeout" json:"timeout"`
	Defaults    Defaults              `yaml:"defaults" json:"defaults"`
	Nodes       []Node                `yaml:"nodes" json:"nodes"`
	Outputs     map[string]any        `yaml:"outputs" json:"outputs"`
}

type InputField struct {
	Type     string   `yaml:"type" json:"type"`
	Required bool     `yaml:"required" json:"required"`
	Default  any      `yaml:"default" json:"default"`
	Enum     []any    `yaml:"enum" json:"enum"`
	Format   string   `yaml:"format" json:"format"`
	Minimum  *float64 `yaml:"minimum" json:"minimum"`
	Maximum  *float64 `yaml:"maximum" json:"maximum"`
	Pattern  string   `yaml:"pattern" json:"pattern"`
}

type Defaults struct {
	Retry   RetryPolicy `yaml:"retry" json:"retry"`
	Timeout *string     `yaml:"timeout" json:"timeout"`
}

type Node struct {
	ID        string         `yaml:"id" json:"id"`
	Type      string         `yaml:"type" json:"type"`
	Needs     []string       `yaml:"needs" json:"needs"`
	Join      *string        `yaml:"join" json:"join"`
	If        string         `yaml:"if" json:"if"`
	Config    map[string]any `yaml:"config" json:"config"`
	Retry     RetryPolicy    `yaml:"retry" json:"retry"`
	Timeout   *string        `yaml:"timeout" json:"timeout"`
	OnFailure string         `yaml:"on_failure" json:"on_failure"`
	OnTimeout string         `yaml:"on_timeout" json:"on_timeout"`
}

type RetryPolicy struct {
	MaxAttempts *int     `yaml:"max_attempts" json:"max_attempts"`
	Backoff     *string  `yaml:"backoff" json:"backoff"`
	Initial     *string  `yaml:"initial" json:"initial"`
	Max         *string  `yaml:"max" json:"max"`
	Jitter      *float64 `yaml:"jitter" json:"jitter"`
	RetryOn     []string `yaml:"retry_on" json:"retry_on"`
}
