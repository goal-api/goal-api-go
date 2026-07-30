package goalapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestClient wires a client to a local test server, with retries off unless a test
// asks for them.
func newTestClient(t *testing.T, handler http.HandlerFunc, opts ...Option) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	all := append([]Option{WithBaseURL(server.URL), WithMaxRetries(0)}, opts...)
	client, err := New("test-key", all...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

// The returned slice is guarded by mu: httptest serves each request on its own
// goroutine, and a test that abandons a request leaves that goroutine running.
func jsonHandler(status int, body string, headers map[string]string) (http.HandlerFunc, *[]*http.Request) {
	var (
		mu   sync.Mutex
		seen []*http.Request
	)
	return func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Clone(r.Context()))
		mu.Unlock()
		for key, value := range headers {
			w.Header().Set(key, value)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}, &seen
}

func TestNewRequiresAPIKey(t *testing.T) {
	if _, err := New(""); err == nil {
		t.Fatal("expected an error for an empty API key")
	}
}

func TestSendsBearerHeaderToTheRightPath(t *testing.T) {
	handler, seen := jsonHandler(200, `{"success":true,"data":[{"id":"x"}]}`, nil)
	client := newTestClient(t, handler)

	page, err := client.Fixtures.Live(context.Background(), nil)
	if err != nil {
		t.Fatalf("Live: %v", err)
	}
	if page.Len() != 1 {
		t.Fatalf("expected 1 row, got %d", page.Len())
	}

	request := (*seen)[0]
	if got := request.URL.Path; got != "/fixtures/live" {
		t.Errorf("path = %q", got)
	}
	if got := request.Header.Get("Authorization"); got != "Bearer test-key" {
		t.Errorf("Authorization = %q", got)
	}
	if got := request.Header.Get("User-Agent"); got != "goal-api-go/"+Version {
		t.Errorf("User-Agent = %q", got)
	}
}

func TestParamsEncodingDropsEmptyAndLowercasesBooleans(t *testing.T) {
	handler, seen := jsonHandler(200, `{"success":true,"data":[]}`, nil)
	client := newTestClient(t, handler)

	_, err := client.Fixtures.List(context.Background(), Params{
		"leagueId": "L1",
		"status":   nil,
		"teamId":   "",
		"offset":   0,
		"live":     false,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	query := (*seen)[0].URL.Query()
	if query.Get("leagueId") != "L1" {
		t.Errorf("leagueId = %q", query.Get("leagueId"))
	}
	if query.Get("offset") != "0" {
		t.Errorf("offset = %q", query.Get("offset"))
	}
	if query.Get("live") != "false" { // the API validates the literal strings
		t.Errorf("live = %q", query.Get("live"))
	}
	if _, present := query["status"]; present {
		t.Error("nil status should have been dropped")
	}
	if _, present := query["teamId"]; present {
		t.Error("empty teamId should have been dropped")
	}
}

func TestPathSegmentsAreEscaped(t *testing.T) {
	handler, seen := jsonHandler(200, `{"success":true,"data":[]}`, nil)
	client := newTestClient(t, handler)

	if _, err := client.Coaches.ByCountry(context.Background(), "Trinidad And Tobago", nil); err != nil {
		t.Fatalf("ByCountry: %v", err)
	}
	if got := (*seen)[0].URL.EscapedPath(); got != "/coaches/country/Trinidad%20And%20Tobago" {
		t.Errorf("escaped path = %q", got)
	}
}

func TestSlashInAnIDCannotEscapeItsSegment(t *testing.T) {
	handler, seen := jsonHandler(200, `{"success":true,"data":null}`, nil)
	client := newTestClient(t, handler)

	if _, err := client.Fixtures.Get(context.Background(), "../../admin"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	// The server must see one literal segment, not a traversal.
	if got := (*seen)[0].URL.Path; got != "/fixtures/../../admin" {
		t.Errorf("decoded path = %q", got)
	}
	if got := (*seen)[0].URL.EscapedPath(); got != "/fixtures/..%2F..%2Fadmin" {
		t.Errorf("escaped path = %q", got)
	}
}

func TestEmptyPathSegmentIsRejectedBeforeTheRequest(t *testing.T) {
	handler, seen := jsonHandler(200, `{}`, nil)
	client := newTestClient(t, handler)

	if _, err := client.Fixtures.Get(context.Background(), ""); err == nil {
		t.Fatal("expected an error for an empty fixture id")
	}
	if len(*seen) != 0 {
		t.Errorf("expected no request to be sent, got %d", len(*seen))
	}
}

func TestCompareJoinsIDsWithCommas(t *testing.T) {
	handler, seen := jsonHandler(200, `{"success":true,"data":[]}`, nil)
	client := newTestClient(t, handler)

	if _, err := client.Players.Compare(context.Background(), "p1", "p2", "p3"); err != nil {
		t.Fatalf("Compare: %v", err)
	}
	if got := (*seen)[0].URL.Query().Get("ids"); got != "p1,p2,p3" {
		t.Errorf("ids = %q", got)
	}
}

func TestSearchDoesNotMutateTheCallersParams(t *testing.T) {
	handler, _ := jsonHandler(200, `{"success":true,"data":[]}`, nil)
	client := newTestClient(t, handler)

	params := Params{"limit": 5}
	if _, err := client.Players.Search(context.Background(), "haaland", params); err != nil {
		t.Fatalf("Search: %v", err)
	}
	if _, leaked := params["q"]; leaked {
		t.Error("Search wrote q into the caller's map")
	}
}

func TestErrorEnvelopeIsMappedToSentinels(t *testing.T) {
	body := `{"success":false,"message":"League ID is required","code":"VALIDATION_ERROR","category":"validation","details":{"field":"id"},"correlationId":"abc-123"}`
	handler, _ := jsonHandler(400, body, nil)
	client := newTestClient(t, handler)

	_, err := client.Leagues.Get(context.Background(), "bad")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, ErrValidation) {
		t.Errorf("expected ErrValidation, got %v", err)
	}

	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *Error, got %T", err)
	}
	if apiErr.Code != "VALIDATION_ERROR" {
		t.Errorf("Code = %q", apiErr.Code)
	}
	if apiErr.CorrelationID != "abc-123" {
		t.Errorf("CorrelationID = %q", apiErr.CorrelationID)
	}
	if string(apiErr.Details) != `{"field":"id"}` {
		t.Errorf("Details = %s", apiErr.Details)
	}
}

func TestStatusToSentinelMapping(t *testing.T) {
	cases := []struct {
		status   int
		sentinel error
	}{
		{400, ErrValidation},
		{401, ErrAuthentication},
		{402, ErrPlanUpgradeRequired},
		{403, ErrPermission},
		{404, ErrNotFound},
		{409, ErrConflict},
		{422, ErrValidation},
		{429, ErrRateLimited},
		{500, ErrServer},
		{503, ErrServiceUnavailable},
	}

	for _, testCase := range cases {
		t.Run(strconv.Itoa(testCase.status), func(t *testing.T) {
			handler, _ := jsonHandler(testCase.status, `{"success":false,"message":"nope"}`, nil)
			client := newTestClient(t, handler)

			_, err := client.Fixtures.Live(context.Background(), nil)
			if !errors.Is(err, testCase.sentinel) {
				t.Errorf("status %d: expected %v, got %v", testCase.status, testCase.sentinel, err)
			}
		})
	}
}

func TestRateLimitErrorCarriesHeaders(t *testing.T) {
	headers := map[string]string{
		"Retry-After":           "42",
		"X-RateLimit-Limit":     "1000",
		"X-RateLimit-Remaining": "0",
		"X-RateLimit-Reset":     "1800000000",
		"X-RateLimit-Type":      "DAILY",
	}
	handler, _ := jsonHandler(429, `{"success":false,"message":"Too many requests","code":"RATE_LIMIT_EXCEEDED"}`, headers)
	client := newTestClient(t, handler)

	_, err := client.Fixtures.Live(context.Background(), nil)
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *Error, got %T", err)
	}
	if apiErr.RetryAfter != 42 || apiErr.Limit != 1000 || apiErr.RateLimitType != "DAILY" {
		t.Errorf("got RetryAfter=%d Limit=%d Type=%q", apiErr.RetryAfter, apiErr.Limit, apiErr.RateLimitType)
	}
}

func TestHTMLErrorBodyStillProducesAUsefulMessage(t *testing.T) {
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(502)
		_, _ = w.Write([]byte("<html><body>502 Bad Gateway</body></html>"))
	})

	_, err := client.Fixtures.Live(context.Background(), nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Message == "" || apiErr.Message == "HTTP 502" {
		t.Fatalf("expected the HTML excerpt in the message, got %v", err)
	}
}

func TestRateLimitSnapshotIsRecordedOnSuccess(t *testing.T) {
	headers := map[string]string{
		"X-RateLimit-Limit":     "10000",
		"X-RateLimit-Remaining": "9997",
		"X-RateLimit-Reset":     "1800000000",
		"X-RateLimit-Type":      "MONTHLY",
	}
	handler, _ := jsonHandler(200, `{"success":true,"data":[]}`, headers)
	client := newTestClient(t, handler)

	if _, err := client.Results.Today(context.Background()); err != nil {
		t.Fatalf("Today: %v", err)
	}
	got := client.RateLimit()
	want := RateLimit{Limit: 10000, Remaining: 9997, Reset: 1800000000, Type: "MONTHLY"}
	if got != want {
		t.Errorf("RateLimit() = %+v, want %+v", got, want)
	}
}

func TestPublicResponseWithoutQuotaHeadersLeavesTheSnapshotAlone(t *testing.T) {
	handler, _ := jsonHandler(200, `{"status":"operational"}`, nil)
	client := newTestClient(t, handler)

	if _, err := client.Status.Get(context.Background()); err != nil {
		t.Fatalf("Status.Get: %v", err)
	}
	if client.RateLimit().Limit != 0 {
		t.Errorf("expected a zero snapshot, got %+v", client.RateLimit())
	}
}

// /public/* answers with bare objects rather than {success, data}, so Status returns Raw.
// A Page or Item here would decode to nothing.
func TestPublicEndpointsReturnTheBareBody(t *testing.T) {
	body := `{"status":"operational","components":[{"name":"API","status":"operational"}]}`
	handler, _ := jsonHandler(200, body, nil)
	client := newTestClient(t, handler)

	raw, err := client.Status.Get(context.Background())
	if err != nil {
		t.Fatalf("Status.Get: %v", err)
	}

	var status struct {
		Status     string `json:"status"`
		Components []struct {
			Name string `json:"name"`
		} `json:"components"`
	}
	if err := raw.Into(&status); err != nil {
		t.Fatalf("Into: %v", err)
	}
	if status.Status != "operational" || len(status.Components) != 1 {
		t.Fatalf("decoded %+v", status)
	}

	asMap, err := raw.Map()
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if asMap["status"] != "operational" {
		t.Errorf("Map()[status] = %v", asMap["status"])
	}
}

func TestCoverageLeaguesPaginatesWithPageNotOffset(t *testing.T) {
	handler, seen := jsonHandler(200, `{"leagues":[],"total":0,"page":2,"limit":50,"pages":1}`, nil)
	client := newTestClient(t, handler)

	if _, err := client.Status.CoverageLeagues(context.Background(), Params{"page": 2, "limit": 50, "country": "England"}); err != nil {
		t.Fatalf("CoverageLeagues: %v", err)
	}
	query := (*seen)[0].URL.Query()
	if query.Get("page") != "2" || query.Get("limit") != "50" || query.Get("country") != "England" {
		t.Errorf("query = %v", query)
	}
}

func TestRetriesA503ThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		attempt := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if attempt == 1 {
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"success":false,"message":"down"}`))
			return
		}
		_, _ = w.Write([]byte(`{"success":true,"data":[1]}`))
	}, WithMaxRetries(2))

	if _, err := client.Fixtures.Live(context.Background(), nil); err != nil {
		t.Fatalf("Live: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("expected 2 calls, got %d", got)
	}
}

func TestDoesNotRetryA400(t *testing.T) {
	var calls atomic.Int32
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"success":false,"message":"bad","code":"VALIDATION_ERROR"}`))
	}, WithMaxRetries(3))

	if _, err := client.Fixtures.Live(context.Background(), nil); !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("expected 1 call, got %d", got)
	}
}

func TestCancelledContextIsNotRetried(t *testing.T) {
	var calls atomic.Int32
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		<-r.Context().Done()
	}, WithMaxRetries(3))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	if _, err := client.Fixtures.Live(ctx, nil); err == nil {
		t.Fatal("expected an error")
	}
	if got := calls.Load(); got > 1 {
		t.Errorf("expected at most 1 attempt after cancellation, got %d", got)
	}
}

func TestPaginatorWalksPagesAndStopsOnHasMoreFalse(t *testing.T) {
	pages := []string{
		`{"success":true,"data":[{"id":1},{"id":2}],"pagination":{"total":3,"limit":2,"offset":0,"hasMore":true}}`,
		`{"success":true,"data":[{"id":3}],"pagination":{"total":3,"limit":2,"offset":2,"hasMore":false}}`,
	}
	var (
		mu      sync.Mutex
		offsets []string
	)
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		offsets = append(offsets, r.URL.Query().Get("offset"))
		index := len(offsets) - 1
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		if index >= len(pages) {
			index = len(pages) - 1
		}
		_, _ = w.Write([]byte(pages[index]))
	})

	type row struct {
		ID int `json:"id"`
	}
	var rows []row
	if err := client.CollectInto(context.Background(),
		func(ctx context.Context, p Params) (*Page, error) { return client.Teams.List(ctx, p) },
		&PaginateOptions{PageSize: 2}, &rows); err != nil {
		t.Fatalf("CollectInto: %v", err)
	}

	if len(rows) != 3 || rows[2].ID != 3 {
		t.Fatalf("rows = %+v", rows)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(offsets) != 2 || offsets[1] != "2" {
		t.Errorf("offsets = %v", offsets)
	}
}

func TestPaginatorHonoursMaxItems(t *testing.T) {
	var calls atomic.Int32
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":[{"id":1},{"id":2}],"pagination":{"total":99,"limit":2,"offset":0,"hasMore":true}}`))
	})

	rows, err := client.CollectRows(context.Background(),
		func(ctx context.Context, p Params) (*Page, error) { return client.Teams.List(ctx, p) },
		&PaginateOptions{PageSize: 2, MaxItems: 2})
	if err != nil {
		t.Fatalf("CollectRows: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("expected 2 rows, got %d", len(rows))
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("expected 1 call, got %d", got)
	}
}

func TestPaginatorStopsOnAShortPageWithoutPaginationBlock(t *testing.T) {
	var calls atomic.Int32
	client := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"data":[{"id":1}]}`))
	})

	rows, err := client.CollectRows(context.Background(),
		func(ctx context.Context, p Params) (*Page, error) { return client.Videos.List(ctx, p) },
		&PaginateOptions{PageSize: 10})
	if err != nil {
		t.Fatalf("CollectRows: %v", err)
	}
	if got := calls.Load(); len(rows) != 1 || got != 1 {
		t.Errorf("rows=%d calls=%d", len(rows), got)
	}
}

func TestPaginatorSurfacesTheError(t *testing.T) {
	handler, _ := jsonHandler(500, `{"success":false,"message":"boom"}`, nil)
	client := newTestClient(t, handler)

	pager := client.Paginate(func(ctx context.Context, p Params) (*Page, error) {
		return client.Teams.List(ctx, p)
	}, nil)

	if pager.Next(context.Background()) {
		t.Fatal("Next should have returned false")
	}
	if !errors.Is(pager.Err(), ErrServer) {
		t.Errorf("Err() = %v", pager.Err())
	}
}

func TestWebSocketURLDerivesFromTheBaseURL(t *testing.T) {
	client, err := New("k", WithBaseURL("https://api.goal-api.com/v1"))
	if err != nil {
		t.Fatal(err)
	}
	if got := client.WebSocketURL(); got != "wss://api.goal-api.com/v1/ws" {
		t.Errorf("WebSocketURL() = %q", got)
	}
	if got := client.WebSocketHeader().Get("Authorization"); got != "Bearer k" {
		t.Errorf("Authorization = %q", got)
	}
}

func TestPageIntoDecodesRows(t *testing.T) {
	page := &Page{Data: json.RawMessage(`[{"name":"Arsenal"},{"name":"Chelsea"}]`)}
	var teams []struct {
		Name string `json:"name"`
	}
	if err := page.Into(&teams); err != nil {
		t.Fatalf("Into: %v", err)
	}
	if len(teams) != 2 || teams[0].Name != "Arsenal" {
		t.Errorf("teams = %+v", teams)
	}
}

func TestItemIntoToleratesNullData(t *testing.T) {
	item := &Item{Data: json.RawMessage(`null`)}
	var target map[string]any
	if err := item.Into(&target); err != nil {
		t.Fatalf("Into: %v", err)
	}
	if target != nil {
		t.Errorf("expected nil, got %v", target)
	}
}

// ------------------------------------------------------------------ webhooks

const testSecret = "whsec_test"

func signPayload(body string, timestamp int64) string {
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write([]byte(fmt.Sprintf("%d.%s", timestamp, body)))
	return fmt.Sprintf("t=%d,v1=%s", timestamp, hex.EncodeToString(mac.Sum(nil)))
}

func TestVerifyWebhookAcceptsACorrectSignature(t *testing.T) {
	body := `{"event":"goal.scored","matchId":"m1"}`
	raw, err := VerifyWebhook([]byte(body), signPayload(body, time.Now().Unix()), testSecret, 0)
	if err != nil {
		t.Fatalf("VerifyWebhook: %v", err)
	}
	if string(raw) != body {
		t.Errorf("raw = %s", raw)
	}
}

func TestVerifyWebhookRejectsATamperedBody(t *testing.T) {
	signature := signPayload(`{"homeScore":1}`, time.Now().Unix())
	_, err := VerifyWebhook([]byte(`{"homeScore":9}`), signature, testSecret, 0)
	if !errors.Is(err, ErrWebhookSignature) {
		t.Fatalf("expected ErrWebhookSignature, got %v", err)
	}
}

func TestVerifyWebhookRejectsAReplayedTimestamp(t *testing.T) {
	body := `{"event":"goal.scored"}`
	old := time.Now().Add(-2 * time.Hour).Unix()
	if _, err := VerifyWebhook([]byte(body), signPayload(body, old), testSecret, 0); !errors.Is(err, ErrWebhookSignature) {
		t.Fatalf("expected ErrWebhookSignature, got %v", err)
	}
	// A negative tolerance disables the check.
	if _, err := VerifyWebhook([]byte(body), signPayload(body, old), testSecret, -1); err != nil {
		t.Fatalf("expected the check to be skipped, got %v", err)
	}
}

func TestVerifyWebhookRejectsMalformedAndMissingHeaders(t *testing.T) {
	for _, header := range []string{"", "garbage", "t=abc,v1=xyz"} {
		if _, err := VerifyWebhook([]byte(`{}`), header, testSecret, 0); !errors.Is(err, ErrWebhookSignature) {
			t.Errorf("header %q: expected ErrWebhookSignature, got %v", header, err)
		}
	}
}

func TestVerifyWebhookRejectsAWrongSecret(t *testing.T) {
	body := `{"a":1}`
	if _, err := VerifyWebhook([]byte(body), signPayload(body, time.Now().Unix()), "whsec_other", 0); !errors.Is(err, ErrWebhookSignature) {
		t.Fatalf("expected ErrWebhookSignature, got %v", err)
	}
}

func TestVerifyWebhookRejectsANonJSONBody(t *testing.T) {
	body := `not json`
	if _, err := VerifyWebhook([]byte(body), signPayload(body, time.Now().Unix()), testSecret, 0); !errors.Is(err, ErrWebhookSignature) {
		t.Fatalf("expected ErrWebhookSignature, got %v", err)
	}
}
