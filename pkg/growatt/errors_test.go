package growatt

import (
	"errors"
	"testing"
)

func TestAPIError(t *testing.T) {
	err := NewAPIError(10011, "permission denied")

	if err.Code != 10011 {
		t.Errorf("expected code 10011, got %d", err.Code)
	}

	if err.Message != "permission denied" {
		t.Errorf("expected message %q, got %q", "permission denied", err.Message)
	}

	expectedStr := "growatt api error 10011: permission denied"
	if err.Error() != expectedStr {
		t.Errorf("expected error string %q, got %q", expectedStr, err.Error())
	}
}

func TestIsPermissionDenied(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "permission denied error",
			err:      &APIError{Code: 10011, Message: "permission denied"},
			expected: true,
		},
		{
			name:     "different api error",
			err:      &APIError{Code: 10012, Message: "plant not found"},
			expected: false,
		},
		{
			name:     "non-api error",
			err:      errors.New("some error"),
			expected: false,
		},
		{
			name:     "nil error",
			err:      nil,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsPermissionDenied(tt.err)
			if result != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, result)
			}
		})
	}
}

func TestIsPlantNotFound(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "plant not found error",
			err:      &APIError{Code: 10012, Message: "plant not found"},
			expected: true,
		},
		{
			name:     "different api error",
			err:      &APIError{Code: 10011, Message: "permission denied"},
			expected: false,
		},
		{
			name:     "non-api error",
			err:      errors.New("some error"),
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsPlantNotFound(tt.err)
			if result != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, result)
			}
		})
	}
}
