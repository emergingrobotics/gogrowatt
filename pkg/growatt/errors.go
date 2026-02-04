package growatt

import (
	"errors"
	"fmt"
	"strings"
)

// APIError represents an error returned by the Growatt API
type APIError struct {
	Code    int
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("growatt api error %d: %s", e.Code, e.Message)
}

// Common API errors
var (
	ErrPermissionDenied = &APIError{Code: 10011, Message: "permission denied"}
	ErrPlantNotFound    = &APIError{Code: 10012, Message: "plant not found"}
	ErrFrequentAccess   = &APIError{Code: 10012, Message: "frequently access (rate limited)"}
	ErrInvalidToken     = &APIError{Code: 10011, Message: "invalid token"}
)

// Client errors
var (
	ErrNoToken       = errors.New("no API token provided")
	ErrInvalidDate   = errors.New("invalid date format")
	ErrEmptyResponse = errors.New("empty response from API")
)

// IsPermissionDenied checks if the error is a permission denied error
func IsPermissionDenied(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code == 10011
	}
	return false
}

// IsPlantNotFound checks if the error is a plant not found error
func IsPlantNotFound(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code == 10012 && !IsRateLimited(err)
	}
	return false
}

// IsRateLimited checks if the error is a rate limit error
func IsRateLimited(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code == 10012 && (apiErr.Message == "error_frequently_access" ||
			strings.Contains(apiErr.Message, "frequently"))
	}
	return false
}

// NewAPIError creates a new API error from code and message
func NewAPIError(code int, message string) *APIError {
	return &APIError{Code: code, Message: message}
}
