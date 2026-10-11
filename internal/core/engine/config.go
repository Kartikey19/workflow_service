package engine

import "time"

// Config tunes one worker process. The application maps its own configuration
// onto it, so the engine does not depend on how settings are loaded.
type Config struct {
	// ID identifies the worker in leases; it must be unique per replica.
	ID            string
	Concurrency   int           // node executions in flight
	BatchSize     int           // rows claimed per poll
	PollInterval  time.Duration // claim fallback when no NOTIFY arrives
	LeaseTTL      time.Duration
	SweepInterval time.Duration
}
