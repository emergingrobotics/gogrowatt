package growatt

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestNewClient(t *testing.T) {
	token := "test-token"
	client := NewClient(token)

	if client.Token() != token {
		t.Errorf("expected token %q, got %q", token, client.Token())
	}

	if client.BaseURL() != DefaultBaseURL {
		t.Errorf("expected base URL %q, got %q", DefaultBaseURL, client.BaseURL())
	}
}

func TestNewClientWithOptions(t *testing.T) {
	token := "test-token"
	customURL := "https://custom.api.com/v1/"
	customTimeout := 60 * time.Second

	httpClient := &http.Client{Timeout: customTimeout}

	client := NewClient(token,
		WithBaseURL(customURL),
		WithHTTPClient(httpClient),
		WithRateLimit(5*time.Second),
	)

	if client.BaseURL() != customURL {
		t.Errorf("expected base URL %q, got %q", customURL, client.BaseURL())
	}

	if client.httpClient.Timeout != customTimeout {
		t.Errorf("expected timeout %v, got %v", customTimeout, client.httpClient.Timeout)
	}

	if client.rateLimit != 5*time.Second {
		t.Errorf("expected rate limit %v, got %v", 5*time.Second, client.rateLimit)
	}
}

func TestNewClientFromEnv(t *testing.T) {
	// Test missing token
	os.Unsetenv(EnvAPIKey)
	_, err := NewClientFromEnv()
	if err != ErrNoToken {
		t.Errorf("expected ErrNoToken, got %v", err)
	}

	// Test with token
	os.Setenv(EnvAPIKey, "env-token")
	defer os.Unsetenv(EnvAPIKey)

	client, err := NewClientFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if client.Token() != "env-token" {
		t.Errorf("expected token %q, got %q", "env-token", client.Token())
	}

	// Test with base URL
	os.Setenv(EnvBaseURL, "https://custom.url/")
	defer os.Unsetenv(EnvBaseURL)

	client, err = NewClientFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if client.BaseURL() != "https://custom.url/" {
		t.Errorf("expected base URL %q, got %q", "https://custom.url/", client.BaseURL())
	}
}

func TestClientRequest(t *testing.T) {
	// Create test server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify token header
		if r.Header.Get("token") != "test-token" {
			t.Errorf("expected token header %q, got %q", "test-token", r.Header.Get("token"))
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"error_code": 0, "error_msg": "success", "data": {}}`))
	}))
	defer server.Close()

	client := NewClient("test-token",
		WithBaseURL(server.URL+"/"),
		WithRateLimit(0), // Disable rate limiting for tests
	)

	ctx := context.Background()
	body, err := client.get(ctx, "test", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(body) == 0 {
		t.Error("expected non-empty response body")
	}
}

func TestCheckResponse(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantErr bool
		errCode int
	}{
		{
			name:    "success",
			body:    `{"error_code": 0, "error_msg": "success", "data": {}}`,
			wantErr: false,
		},
		{
			name:    "permission denied",
			body:    `{"error_code": 10011, "error_msg": "error_permission_denied", "data": ""}`,
			wantErr: true,
			errCode: 10011,
		},
		{
			name:    "plant not found",
			body:    `{"error_code": 10012, "error_msg": "error_plant_not_found", "data": ""}`,
			wantErr: true,
			errCode: 10012,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkResponse([]byte(tt.body))
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
					return
				}
				apiErr, ok := err.(*APIError)
				if !ok {
					t.Errorf("expected APIError, got %T", err)
					return
				}
				if apiErr.Code != tt.errCode {
					t.Errorf("expected error code %d, got %d", tt.errCode, apiErr.Code)
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}

func TestSetRateLimit(t *testing.T) {
	client := NewClient("test")
	client.SetRateLimit(10 * time.Second)

	if client.rateLimit != 10*time.Second {
		t.Errorf("expected rate limit %v, got %v", 10*time.Second, client.rateLimit)
	}
}
