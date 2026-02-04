package growatt

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"
)

const (
	DefaultBaseURL     = "https://openapi.growatt.com/v1/"
	DefaultTimeout     = 30 * time.Second
	DefaultRateLimit   = 3 * time.Second
	EnvAPIKey          = "GROWATT_API_KEY"
	EnvBaseURL         = "GROWATT_BASE_URL"
)

// Client is the Growatt API client
type Client struct {
	baseURL    string
	token      string
	httpClient *http.Client
	rateLimit  time.Duration
	lastCall   time.Time
}

// ClientOption is a function that configures the client
type ClientOption func(*Client)

// WithBaseURL sets a custom base URL
func WithBaseURL(url string) ClientOption {
	return func(c *Client) {
		c.baseURL = url
	}
}

// WithHTTPClient sets a custom HTTP client
func WithHTTPClient(client *http.Client) ClientOption {
	return func(c *Client) {
		c.httpClient = client
	}
}

// WithTimeout sets the request timeout
func WithTimeout(d time.Duration) ClientOption {
	return func(c *Client) {
		c.httpClient.Timeout = d
	}
}

// WithRateLimit sets the minimum delay between API calls
func WithRateLimit(d time.Duration) ClientOption {
	return func(c *Client) {
		c.rateLimit = d
	}
}

// NewClient creates a new Growatt API client
func NewClient(token string, opts ...ClientOption) *Client {
	c := &Client{
		baseURL:   DefaultBaseURL,
		token:     token,
		rateLimit: DefaultRateLimit,
		httpClient: &http.Client{
			Timeout: DefaultTimeout,
		},
	}

	for _, opt := range opts {
		opt(c)
	}

	return c
}

// NewClientFromEnv creates a client using environment variables
func NewClientFromEnv(opts ...ClientOption) (*Client, error) {
	token := os.Getenv(EnvAPIKey)
	if token == "" {
		return nil, ErrNoToken
	}

	c := NewClient(token, opts...)

	if baseURL := os.Getenv(EnvBaseURL); baseURL != "" {
		c.baseURL = baseURL
	}

	return c, nil
}

// SetRateLimit sets the minimum delay between API calls
func (c *Client) SetRateLimit(d time.Duration) {
	c.rateLimit = d
}

// Token returns the current API token
func (c *Client) Token() string {
	return c.token
}

// BaseURL returns the current base URL
func (c *Client) BaseURL() string {
	return c.baseURL
}

// enforceRateLimit waits if necessary to respect rate limiting
func (c *Client) enforceRateLimit() {
	if c.rateLimit > 0 && !c.lastCall.IsZero() {
		elapsed := time.Since(c.lastCall)
		if elapsed < c.rateLimit {
			time.Sleep(c.rateLimit - elapsed)
		}
	}
	c.lastCall = time.Now()
}

// doRequest performs an HTTP request to the API
func (c *Client) doRequest(ctx context.Context, method, endpoint string, params url.Values) ([]byte, error) {
	c.enforceRateLimit()

	fullURL := c.baseURL + endpoint
	if len(params) > 0 {
		fullURL += "?" + params.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, method, fullURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("token", c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	return body, nil
}

// get performs a GET request
func (c *Client) get(ctx context.Context, endpoint string, params url.Values) ([]byte, error) {
	return c.doRequest(ctx, http.MethodGet, endpoint, params)
}

// checkResponse checks if the API response indicates an error
func checkResponse(body []byte) error {
	var resp Response[any]
	if err := json.Unmarshal(body, &resp); err != nil {
		return fmt.Errorf("parsing response: %w", err)
	}

	if resp.ErrorCode != 0 {
		return NewAPIError(resp.ErrorCode, resp.ErrorMsg)
	}

	return nil
}

// parseResponse parses a JSON response into the given type
func parseResponse[T any](body []byte) (*T, error) {
	if err := checkResponse(body); err != nil {
		return nil, err
	}

	var resp Response[T]
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parsing response data: %w", err)
	}

	return &resp.Data, nil
}
