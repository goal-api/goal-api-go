// Package goalapi is the Go client for the GOAL API: football fixtures, live scores,
// standings, player stats and odds. Standard library only.
//
// https://goal-api.com/documentation
//
//	client, err := goalapi.New(os.Getenv("GOAL_API_KEY"))
//	if err != nil {
//	    log.Fatal(err)
//	}
//	page, err := client.Fixtures.Live(ctx, nil)
package goalapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultBaseURL is the production GOAL API endpoint.
	DefaultBaseURL = "https://api.goal-api.com/v1"
	// Version of this SDK, sent as part of the User-Agent.
	Version = "1.0.0"

	defaultTimeout    = 30 * time.Second
	defaultMaxRetries = 2
	maxErrorBodyBytes = 1 << 20
)

// Client is a GOAL API client, safe for concurrent use. Share one: the http.Client
// pools connections and the rate-limit snapshot is per-client.
type Client struct {
	apiKey     string
	baseURL    string
	userAgent  string
	httpClient *http.Client
	maxRetries int
	headers    map[string]string

	rateLimit rateLimitStore

	// Resource groups.
	Status      *StatusService
	Countries   *CountriesService
	Leagues     *LeaguesService
	Teams       *TeamsService
	Fixtures    *FixturesService
	Standings   *StandingsService
	Players     *PlayersService
	Coaches     *CoachesService
	H2H         *H2HService
	Results     *ResultsService
	Videos      *VideosService
	Odds        *OddsService
	Predictions *PredictionsService
}

// Option configures a Client.
type Option func(*Client)

// WithBaseURL overrides the API endpoint, e.g. for staging.
func WithBaseURL(baseURL string) Option {
	return func(c *Client) { c.baseURL = strings.TrimRight(baseURL, "/") }
}

// WithHTTPClient supplies your own *http.Client, for a custom transport or proxy. Its
// Timeout, if set, applies per attempt and overrides WithTimeout.
func WithHTTPClient(hc *http.Client) Option {
	return func(c *Client) { c.httpClient = hc }
}

// WithTimeout sets the per-attempt timeout. Default 30s.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.httpClient.Timeout = d }
}

// WithMaxRetries sets retries for 429, 5xx and network errors. Default 2, zero to disable.
func WithMaxRetries(n int) Option {
	return func(c *Client) { c.maxRetries = n }
}

// WithUserAgent overrides the User-Agent header.
func WithUserAgent(ua string) Option {
	return func(c *Client) { c.userAgent = ua }
}

// WithHeader adds a header to every request.
func WithHeader(key, value string) Option {
	return func(c *Client) {
		if c.headers == nil {
			c.headers = map[string]string{}
		}
		c.headers[key] = value
	}
}

// New creates a client. Get an API key at https://goal-api.com/dashboard.
func New(apiKey string, opts ...Option) (*Client, error) {
	if apiKey == "" {
		return nil, errors.New("goalapi: apiKey is required. Create one at https://goal-api.com/dashboard")
	}

	c := &Client{
		apiKey:     apiKey,
		baseURL:    DefaultBaseURL,
		userAgent:  "goal-api-go/" + Version,
		httpClient: &http.Client{Timeout: defaultTimeout},
		maxRetries: defaultMaxRetries,
	}
	for _, opt := range opts {
		opt(c)
	}

	c.Status = &StatusService{c}
	c.Countries = &CountriesService{c}
	c.Leagues = &LeaguesService{c}
	c.Teams = &TeamsService{c}
	c.Fixtures = &FixturesService{c}
	c.Standings = &StandingsService{c}
	c.Players = &PlayersService{c}
	c.Coaches = &CoachesService{c}
	c.H2H = &H2HService{c}
	c.Results = &ResultsService{c}
	c.Videos = &VideosService{c}
	c.Odds = &OddsService{c}
	c.Predictions = &PredictionsService{c}

	return c, nil
}

// BaseURL returns the endpoint this client talks to.
func (c *Client) BaseURL() string { return c.baseURL }

// RateLimit returns quota from the last response. Zero until the first authenticated call.
func (c *Client) RateLimit() RateLimit { return c.rateLimit.load() }

// Get decodes a raw GET into out, for endpoints not wrapped here yet. Prefer the service
// methods. out may be *Page, *Item, *Raw or your own type.
func (c *Client) Get(ctx context.Context, path string, params Params, out any) error {
	return c.do(ctx, http.MethodGet, path, params, nil, out)
}

// Post performs a raw POST. Not retried: /ws/token is single-use, so a retry would burn
// the token the first attempt may already have minted.
func (c *Client) Post(ctx context.Context, path string, body any, out any) error {
	return c.do(ctx, http.MethodPost, path, nil, body, out)
}

func (c *Client) do(ctx context.Context, method, path string, params Params, body, out any) error {
	target := c.baseURL + path
	if query := params.encode(); query != "" {
		target += "?" + query
	}

	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return fmt.Errorf("goalapi: encoding request body: %w", err)
		}
	}

	attempts := c.maxRetries
	if method != http.MethodGet {
		attempts = 0
	}

	var lastErr error
	for attempt := 0; attempt <= attempts; attempt++ {
		err := c.attempt(ctx, method, target, payload, out)
		if err == nil {
			return nil
		}
		lastErr = err

		if attempt == attempts || !isRetryable(err) {
			return err
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(backoff(attempt, err)):
		}
	}
	return lastErr
}

func (c *Client) attempt(ctx context.Context, method, target string, payload []byte, out any) error {
	var reader io.Reader
	if payload != nil {
		reader = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return fmt.Errorf("goalapi: building request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range c.headers {
		req.Header.Set(key, value)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		// Pass cancellation through unwrapped so callers can errors.Is it.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if isTimeout(err) {
			return &Error{Message: fmt.Sprintf("request to %s timed out", target), Timeout: true, wrapped: err}
		}
		return &Error{Message: fmt.Sprintf("could not reach %s: %v", target, err), Network: true, wrapped: err}
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	c.rateLimit.store(resp.Header)

	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
		return errorFromResponse(resp.StatusCode, raw, resp.Header)
	}

	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return &Error{
			StatusCode: resp.StatusCode,
			Message:    fmt.Sprintf("could not decode response from %s: %v", target, err),
			wrapped:    err,
		}
	}
	return nil
}

func isTimeout(err error) bool {
	var timeouter interface{ Timeout() bool }
	return errors.As(err, &timeouter) && timeouter.Timeout()
}

func isRetryable(err error) bool {
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		return false
	}
	if apiErr.Timeout || apiErr.Network {
		return true
	}
	switch apiErr.StatusCode {
	case http.StatusTooManyRequests, 500, 502, 503, 504:
		return true
	}
	return false
}

// backoff is exponential with jitter. A server-sent Retry-After takes priority.
func backoff(attempt int, err error) time.Duration {
	var apiErr *Error
	if errors.As(err, &apiErr) && apiErr.RetryAfter > 0 {
		if wait := time.Duration(apiErr.RetryAfter) * time.Second; wait <= time.Minute {
			return wait
		}
		return time.Minute
	}
	ceiling := 500 * (1 << attempt)
	if ceiling > 8000 {
		ceiling = 8000
	}
	return time.Duration(rand.Intn(ceiling)) * time.Millisecond //nolint:gosec // jitter, not crypto
}

// segment percent-encodes a path segment. Ids, dates and country names come from callers,
// so they can't be interpolated raw.
func segment(value string) (string, error) {
	if value == "" {
		return "", errors.New("goalapi: path segment is required")
	}
	// PathEscape covers "/", so "../admin" stays inside one segment.
	return url.PathEscape(value), nil
}

// CSV joins ids for the /players/compare endpoint.
func CSV(values ...string) string { return strings.Join(values, ",") }

func atoiOrZero(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}
