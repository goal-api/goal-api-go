// GOAL_API_KEY=... go run ./examples/live
//
// Connects to the live socket, subscribes to whatever is in play, and reports every frame
// the server sends. Exits non-zero if nothing arrives, so it works as a smoke test.
//
// Part of the main module: the SDK ships its own WebSocket client, so this needs no
// dependencies and no go.mod of its own.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"time"

	goalapi "github.com/goal-api/goal-api-go"
)

type fixture struct {
	ID       string                `json:"id"`
	HomeTeam struct{ Name string } `json:"homeTeam"`
	AwayTeam struct{ Name string } `json:"awayTeam"`
}

func main() {
	apiKey := os.Getenv("GOAL_API_KEY")
	if apiKey == "" {
		fail(fmt.Errorf("GOAL_API_KEY is not set"))
	}

	listen := 30 * time.Second
	if v := os.Getenv("LISTEN_SECONDS"); v != "" {
		if d, err := time.ParseDuration(v + "s"); err == nil {
			listen = d
		}
	}

	client, err := goalapi.New(apiKey)
	if err != nil {
		fail(err)
	}

	// Ctrl-C ends the run cleanly instead of killing it mid-frame.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, listen+30*time.Second)
	defer cancel()

	page, err := client.Fixtures.Live(ctx, nil)
	if err != nil {
		fail(err)
	}
	var fixtures []fixture
	if err := page.Into(&fixtures); err != nil {
		fail(err)
	}
	fmt.Printf("%d match(es) in play\n", len(fixtures))
	if len(fixtures) > 5 {
		fixtures = fixtures[:5]
	}
	for _, m := range fixtures {
		fmt.Printf("  %s  %s v %s\n", m.ID, m.HomeTeam.Name, m.AwayTeam.Name)
	}

	live := client.Live()
	seen := map[string]int{}

	live.On(goalapi.LiveAuthSuccess, func(msg goalapi.LiveMessage) {
		var auth struct {
			Plan             string `json:"plan"`
			MaxSubscriptions int    `json:"maxSubscriptions"`
		}
		_ = msg.Into(&auth)
		fmt.Printf("\nauthenticated: plan=%s maxSubscriptions=%d\n", auth.Plan, auth.MaxSubscriptions)
		if auth.MaxSubscriptions == 0 {
			fmt.Println("  this plan allows 0 concurrent subscriptions, so no match_update can arrive")
		}
	})

	live.On(goalapi.LiveSubscribeResponse, func(msg goalapi.LiveMessage) {
		if msg.Success != nil && *msg.Success {
			fmt.Println("  subscribed")
			return
		}
		fmt.Printf("  subscribe rejected: %s %s\n", msg.Code, msg.Message)
	})

	// A match_update is the provider's live shape, not the REST fixture shape: scores are
	// strings and match_status is the minute. Decoding it into the type used for
	// /fixtures above would compile and quietly give you empty fields.
	live.On(goalapi.LiveMatchUpdate, func(msg goalapi.LiveMessage) {
		var update struct {
			// The fixture id we subscribed with. MatchID is the provider's.
			ID        string `json:"id"`
			MatchID   string `json:"match_id"`
			HomeName  string `json:"match_hometeam_name"`
			AwayName  string `json:"match_awayteam_name"`
			HomeScore string `json:"match_hometeam_score"`
			AwayScore string `json:"match_awayteam_score"`
			Status    string `json:"match_status"`
		}
		if err := msg.Into(&update); err != nil {
			fmt.Fprintln(os.Stderr, "  could not decode update:", err)
			return
		}
		fmt.Printf("  UPDATE  %-22s %s-%s %-22s  %s'  (%s)\n",
			update.HomeName, update.HomeScore, update.AwayScore, update.AwayName,
			update.Status, update.ID)
	})

	live.On(goalapi.LivePong, func(goalapi.LiveMessage) {
		fmt.Println("  pong")
	})

	// Counting every type, including the ones without a handler above.
	live.On(goalapi.LiveEventAny, func(msg goalapi.LiveMessage) {
		seen[msg.Type]++
	})

	// Connect returns once the server has accepted the auth frame, so subscribing
	// immediately afterwards is safe.
	if err := live.Connect(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "\nconnect failed: %v\n", err)
		os.Exit(1)
	}
	defer live.Close()

	for _, m := range fixtures {
		if err := live.Subscribe(m.ID); err != nil {
			fail(err)
		}
	}
	_ = live.ListSubscriptions()
	_ = live.Ping()

	fmt.Printf("\nlistening for %s ...\n", listen)

	// Stop listening on our own schedule rather than waiting for the socket to end.
	listenCtx, done := context.WithTimeout(ctx, listen)
	defer done()
	if err := live.Run(listenCtx); err != nil && !isExpectedStop(err) {
		fmt.Fprintf(os.Stderr, "\nlive feed stopped: %v\n", err)
		os.Exit(1)
	}

	report, _ := json.Marshal(seen)
	fmt.Printf("\nreceived: %s\n", report)
	if seen[goalapi.LiveAuthSuccess] == 0 {
		fmt.Fprintln(os.Stderr, "nothing was received from the socket")
		os.Exit(1)
	}
}

// isExpectedStop reports whether Run ended because our own deadline expired, which is how
// this example is meant to finish.
func isExpectedStop(err error) bool {
	return err == context.DeadlineExceeded || err == context.Canceled
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
