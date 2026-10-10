package spec

func ApplyDefaults(document *Document) {
	if document.Timeout == nil {
		timeout := "720h"
		document.Timeout = &timeout
	}

	maxAttempts := 1
	backoff := "exponential"
	initial := "1s"
	maximum := "60s"
	jitter := 0.0

	builtInRetry := RetryPolicy{
		MaxAttempts: &maxAttempts,
		Backoff:     &backoff,
		Initial:     &initial,
		Max:         &maximum,
		Jitter:      &jitter,
	}

	for i := range document.Nodes {
		node := &document.Nodes[i]

		if node.Needs == nil {
			node.Needs = []string{}
		}

		if node.Join == nil {
			join := "all"
			node.Join = &join
		}

		if node.Timeout == nil {
			node.Timeout = document.Defaults.Timeout
		}

		inheritRetry(&node.Retry, document.Defaults.Retry)
		inheritRetry(&node.Retry, builtInRetry)
	}
}

func inheritRetry(target *RetryPolicy, fallback RetryPolicy) {
	if target.MaxAttempts == nil {
		target.MaxAttempts = fallback.MaxAttempts
	}
	if target.Backoff == nil {
		target.Backoff = fallback.Backoff
	}
	if target.Initial == nil {
		target.Initial = fallback.Initial
	}
	if target.Max == nil {
		target.Max = fallback.Max
	}
	if target.Jitter == nil {
		target.Jitter = fallback.Jitter
	}
	if target.RetryOn == nil {
		target.RetryOn = fallback.RetryOn
	}
}
