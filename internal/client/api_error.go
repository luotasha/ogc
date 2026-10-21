package client

import "fmt"

// APIError preserves upstream status separately from the provider response body.
type APIError struct {
	StatusCode int
	Body       string
	RequestID  string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("upstream HTTP %d (request_id=%s): %s", e.StatusCode, e.RequestID, e.Body)
}
