// GOAL_API_KEY=... go run ./examples/basic
//
// The status section works without a key.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	goalapi "github.com/Devara-sarl/goal-api-go"
)

type component struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Uptime struct {
		Day float64 `json:"24h"`
	} `json:"uptime"`
}

type platformStatus struct {
	Status     string      `json:"status"`
	Components []component `json:"components"`
}

type team struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type league struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type fixture struct {
	HomeTeam  team `json:"homeTeam"`
	AwayTeam  team `json:"awayTeam"`
	HomeScore *int `json:"homeScore"`
	AwayScore *int `json:"awayScore"`
	Minute    *int `json:"minute"`
}

type standingRow struct {
	Position int    `json:"position"`
	Points   int    `json:"points"`
	Team     team   `json:"team"`
	TeamName string `json:"teamName"`
}

func main() {
	apiKey := os.Getenv("GOAL_API_KEY")
	if apiKey == "" {
		apiKey = "unset" // the public status endpoint ignores it
	}

	client, err := goalapi.New(apiKey, goalapi.WithTimeout(15*time.Second))
	if err != nil {
		fail(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// --- public status: no API key required ---------------------------------
	// Note: /public/* returns a bare object, not the {success, data} envelope.
	body, err := client.Status.Get(ctx)
	if err != nil {
		fail(err)
	}
	var status platformStatus
	if err := body.Into(&status); err != nil {
		fail(err)
	}
	fmt.Printf("Platform: %s\n", status.Status)
	for _, c := range status.Components {
		fmt.Printf("  %-28s %-14s 24h %.3f%%\n", c.Name, c.Status, c.Uptime.Day)
	}

	if os.Getenv("GOAL_API_KEY") == "" {
		fmt.Println("\nSet GOAL_API_KEY to run the authenticated examples.")
		return
	}

	// --- live matches -------------------------------------------------------
	page, err := client.Fixtures.Live(ctx, nil)
	if err != nil {
		fail(err)
	}
	var live []fixture
	if err := page.Into(&live); err != nil {
		fail(err)
	}
	fmt.Printf("\n%d match(es) in play\n", len(live))
	for i, match := range live {
		if i == 5 {
			break
		}
		fmt.Printf("  %s %s-%s %s (%s')\n",
			match.HomeTeam.Name, num(match.HomeScore), num(match.AwayScore),
			match.AwayTeam.Name, num(match.Minute))
	}

	// --- pick a league and read its table -----------------------------------
	leaguePage, err := client.Leagues.List(ctx, goalapi.Params{"isActive": true, "limit": 1})
	if err != nil {
		fail(err)
	}
	var leagues []league
	if err := leaguePage.Into(&leagues); err != nil {
		fail(err)
	}
	if len(leagues) == 0 {
		return
	}
	current := leagues[0]

	tablePage, err := client.Leagues.Standings(ctx, current.ID, nil)
	if err != nil {
		fail(err)
	}
	var table []standingRow
	if err := tablePage.Into(&table); err != nil {
		fail(err)
	}
	fmt.Printf("\nStandings: %s\n", current.Name)
	for i, row := range table {
		if i == 5 {
			break
		}
		name := row.Team.Name
		if name == "" {
			name = row.TeamName
		}
		fmt.Printf("  %2d. %-24s %d pts\n", row.Position, name, row.Points)
	}

	// --- pagination across every team in the league --------------------------
	var teams []team
	err = client.CollectInto(ctx, func(ctx context.Context, p goalapi.Params) (*goalapi.Page, error) {
		return client.Leagues.Teams(ctx, current.ID, p)
	}, nil, &teams)
	if err != nil {
		fail(err)
	}
	fmt.Printf("\n%d teams in %s\n", len(teams), current.Name)

	quota := client.RateLimit()
	fmt.Printf("\nQuota: %d/%d remaining (%s, resets at %s)\n",
		quota.Remaining, quota.Limit, quota.Type, time.Unix(quota.Reset, 0).Format(time.RFC3339))
}

func num(value *int) string {
	if value == nil {
		return "?"
	}
	return fmt.Sprint(*value)
}

func fail(err error) {
	var apiErr *goalapi.Error
	switch {
	case errors.Is(err, goalapi.ErrRateLimited):
		errors.As(err, &apiErr)
		fmt.Fprintf(os.Stderr, "Rate limited (%s). Retry in %ds.\n", apiErr.RateLimitType, apiErr.RetryAfter)
	case errors.As(err, &apiErr):
		fmt.Fprintf(os.Stderr, "%s: %s [%s]\n", apiErr.Code, apiErr.Message, apiErr.CorrelationID)
	default:
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(1)
}
