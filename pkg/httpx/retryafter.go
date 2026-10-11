package httpx

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// maxRetryAfter caps what an upstream can ask us to wait.
const maxRetryAfter = time.Hour

// ParseRetryAfter reads a Retry-After header, either delta-seconds or an HTTP
// date. A missing or malformed value yields 0.
func ParseRetryAfter(header string) time.Duration {
	if header == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(strings.TrimSpace(header)); err == nil && seconds > 0 {
		return min(time.Duration(seconds)*time.Second, maxRetryAfter)
	}
	if at, err := http.ParseTime(header); err == nil {
		return min(max(time.Until(at), 0), maxRetryAfter)
	}
	return 0
}
