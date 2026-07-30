package goalapi

// Runs against the real API. Skipped unless GOAL_API_KEY is set.
//
//	GOAL_API_KEY=... go test ./...
//
// Endpoint-by-endpoint coverage lives in tools/sweep.py. This checks the parts
// that are the SDK's job: auth, param encoding, envelope handling, pagination across real
// pages, error mapping and the quota snapshot.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"
)

func liveClient(t *testing.T) *Client {
	t.Helper()
	apiKey := os.Getenv("GOAL_API_KEY")
	if apiKey == "" {
		t.Skip("GOAL_API_KEY is not set")
	}
	client, err := New(apiKey, WithTimeout(30*time.Second))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

func liveContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestLiveStatusReturnsBareBody(t *testing.T) {
	client, ctx := liveClient(t), liveContext(t)

	raw, err := client.Status.Get(ctx)
	if err != nil {
		t.Fatalf("Status.Get: %v", err)
	}

	body, err := raw.Map()
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if _, ok := body["status"].(string); !ok {
		t.Errorf("no status field: %v", body)
	}
	if _, wrapped := body["data"]; wrapped {
		t.Error("/public/* must not be wrapped in an envelope")
	}
}

func TestLiveCoverageLeaguesPaginatesWithPage(t *testing.T) {
	client, ctx := liveClient(t), liveContext(t)

	raw, err := client.Status.CoverageLeagues(ctx, Params{"page": 1, "limit": 5})
	if err != nil {
		t.Fatalf("CoverageLeagues: %v", err)
	}
	var page struct {
		Leagues []map[string]any `json:"leagues"`
		Total   int              `json:"total"`
		Pages   int              `json:"pages"`
	}
	if err := raw.Into(&page); err != nil {
		t.Fatalf("Into: %v", err)
	}
	if len(page.Leagues) > 5 || page.Pages == 0 {
		t.Errorf("got %d leagues, %d pages", len(page.Leagues), page.Pages)
	}
}

func TestLiveLeaguesListReturnsEnvelope(t *testing.T) {
	client, ctx := liveClient(t), liveContext(t)

	page, err := client.Leagues.List(ctx, Params{"isActive": true, "limit": 3})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !page.Success {
		t.Error("success was false")
	}
	if page.Len() > 3 {
		t.Errorf("limit ignored: got %d rows", page.Len())
	}
	if page.Pagination == nil {
		t.Error("no pagination block")
	}
	if page.Source != "cache" && page.Source != "database" {
		t.Errorf("source = %q", page.Source)
	}
}

func TestLiveQuotaSnapshot(t *testing.T) {
	client, ctx := liveClient(t), liveContext(t)

	if _, err := client.Leagues.List(ctx, Params{"limit": 1}); err != nil {
		t.Fatalf("List: %v", err)
	}
	quota := client.RateLimit()
	if quota.Limit == 0 {
		t.Error("limit was not captured")
	}
	if quota.Remaining > quota.Limit {
		t.Errorf("remaining %d > limit %d", quota.Remaining, quota.Limit)
	}
	if quota.Type != "DAILY" && quota.Type != "MONTHLY" {
		t.Errorf("type = %q", quota.Type)
	}
}

func TestLiveNestedLeagueEndpoints(t *testing.T) {
	client, ctx := liveClient(t), liveContext(t)

	page, err := client.Leagues.List(ctx, Params{"isActive": true, "limit": 1})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	var leagues []struct {
		ID string `json:"id"`
	}
	if err := page.Into(&leagues); err != nil {
		t.Fatalf("Into: %v", err)
	}
	if len(leagues) == 0 {
		t.Skip("no active leagues returned")
	}

	if _, err := client.Leagues.Teams(ctx, leagues[0].ID, Params{"limit": 5}); err != nil {
		t.Errorf("Teams: %v", err)
	}
	if _, err := client.Leagues.Fixtures(ctx, leagues[0].ID, Params{"limit": 5}); err != nil {
		t.Errorf("Fixtures: %v", err)
	}
}

func TestLivePaginatorWalksRealPages(t *testing.T) {
	client, ctx := liveClient(t), liveContext(t)

	pager := client.Paginate(func(ctx context.Context, p Params) (*Page, error) {
		return client.Countries.List(ctx, p)
	}, &PaginateOptions{PageSize: 20, MaxItems: 45})

	seen := map[string]bool{}
	for pager.Next(ctx) {
		var row struct {
			ID string `json:"id"`
		}
		if err := pager.Into(&row); err != nil {
			t.Fatalf("Into: %v", err)
		}
		if seen[row.ID] {
			t.Fatalf("pagination returned %s twice", row.ID)
		}
		seen[row.ID] = true
	}
	if err := pager.Err(); err != nil {
		t.Fatalf("Err: %v", err)
	}
	if len(seen) <= 20 {
		t.Errorf("expected pagination to cross a page boundary, got %d rows", len(seen))
	}
}

func TestLiveUnknownIDIsNotFound(t *testing.T) {
	client, ctx := liveClient(t), liveContext(t)

	_, err := client.Fixtures.Get(ctx, "definitely-not-a-real-id")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *Error, got %T", err)
	}
	if apiErr.Code != "FIXTURE_NOT_FOUND" {
		t.Errorf("code = %q", apiErr.Code)
	}
	// football-service puts the text in `error`, not `message`; the SDK normalises both.
	if apiErr.Message == "" {
		t.Error("message was empty")
	}
}

func TestLiveBadEnumIsValidationError(t *testing.T) {
	client, ctx := liveClient(t), liveContext(t)

	_, err := client.Players.Top(ctx, "not-a-real-stat", nil)
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("expected ErrValidation, got %v", err)
	}

	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *Error, got %T", err)
	}
	if apiErr.Code != "VALIDATION_ERROR" {
		t.Errorf("code = %q", apiErr.Code)
	}
	// Service validation errors send an array here, not an object.
	var details []map[string]any
	if err := json.Unmarshal(apiErr.Details, &details); err != nil {
		t.Errorf("details was not an array: %s", apiErr.Details)
	}
}

func TestLiveBadKeyIsAuthenticationError(t *testing.T) {
	if os.Getenv("GOAL_API_KEY") == "" {
		t.Skip("GOAL_API_KEY is not set")
	}
	client, err := New("gapi_not_a_real_key", WithMaxRetries(0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := client.Leagues.List(liveContext(t), Params{"limit": 1}); !errors.Is(err, ErrAuthentication) {
		t.Fatalf("expected ErrAuthentication, got %v", err)
	}
}
