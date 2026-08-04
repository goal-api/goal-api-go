// GOAL_API_KEY=... go run ./examples/live
//
// Connects to the live socket, subscribes to whatever is in play, and prints every frame
// the server sends. Exits non-zero if nothing arrives, so it works as a smoke test.
//
// This module has no dependencies, so the example brings its own WebSocket library. It is
// in examples/ rather than the module root, so `go get` on the SDK still pulls nothing.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/coder/websocket"
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
		fmt.Fprintln(os.Stderr, "GOAL_API_KEY is not set.")
		os.Exit(2)
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
	ctx, cancel := context.WithTimeout(context.Background(), listen+30*time.Second)
	defer cancel()

	page, err := client.Fixtures.Live(ctx, nil)
	if err != nil {
		fail(err)
	}
	var live []fixture
	if err := page.Into(&live); err != nil {
		fail(err)
	}
	fmt.Printf("%d match(es) in play\n", len(live))
	if len(live) > 5 {
		live = live[:5]
	}
	for _, m := range live {
		fmt.Printf("  %s  %s v %s\n", m.ID, m.HomeTeam.Name, m.AwayTeam.Name)
	}

	conn, _, err := websocket.Dial(ctx, client.WebSocketURL(), &websocket.DialOptions{
		HTTPHeader: client.WebSocketHeader(),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "\ndial failed: %v\n", err)
		os.Exit(1)
	}
	defer conn.CloseNow()

	// Must be the first frame on the wire, or the server closes with 4001.
	if err := write(ctx, conn, client.AuthMessage()); err != nil {
		fail(err)
	}

	seen := map[string]int{}
	deadline := time.Now().Add(listen)

	for time.Now().Before(deadline) {
		readCtx, cancelRead := context.WithDeadline(ctx, deadline)
		_, data, err := conn.Read(readCtx)
		cancelRead()
		if err != nil {
			break
		}

		var msg goalapi.LiveMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		seen[msg.Type]++

		switch msg.Type {
		case goalapi.LiveAuthSuccess:
			var auth struct {
				Plan             string `json:"plan"`
				MaxSubscriptions int    `json:"maxSubscriptions"`
			}
			_ = msg.Into(&auth)
			fmt.Printf("\nauthenticated: plan=%s maxSubscriptions=%d\n", auth.Plan, auth.MaxSubscriptions)
			if auth.MaxSubscriptions == 0 {
				fmt.Println("  this plan allows 0 concurrent subscriptions, so no match_update can arrive")
			}
			for _, m := range live {
				_ = write(ctx, conn, goalapi.SubscribeMessage(m.ID))
			}
			_ = write(ctx, conn, goalapi.ListSubscriptionsMessage())
			_ = write(ctx, conn, goalapi.PingMessage())
			fmt.Printf("\nlistening for %s ...\n", listen)

		case goalapi.LiveSubscribeResponse:
			var envelope struct {
				Success bool                           `json:"success"`
				Error   struct{ Code, Message string } `json:"error"`
			}
			_ = json.Unmarshal(data, &envelope)
			if envelope.Success {
				fmt.Println("  subscribed")
			} else {
				fmt.Printf("  subscribe rejected: %s %s\n", envelope.Error.Code, envelope.Error.Message)
			}

		case goalapi.LiveMatchUpdate:
			var update struct {
				HomeTeam  struct{ Name string } `json:"homeTeam"`
				AwayTeam  struct{ Name string } `json:"awayTeam"`
				HomeScore *int                  `json:"homeScore"`
				AwayScore *int                  `json:"awayScore"`
			}
			_ = msg.Into(&update)
			fmt.Printf("  UPDATE  %s v %s\n", update.HomeTeam.Name, update.AwayTeam.Name)

		case goalapi.LivePong:
			fmt.Println("  pong")
		}
	}

	_ = conn.Close(websocket.StatusNormalClosure, "done")
	fmt.Printf("\nreceived: %v\n", seen)
	if seen[goalapi.LiveAuthSuccess] == 0 {
		fmt.Fprintln(os.Stderr, "nothing was received from the socket")
		os.Exit(1)
	}
}

func write(ctx context.Context, conn *websocket.Conn, message map[string]any) error {
	payload, err := json.Marshal(message)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, payload)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
