// GOAL_API_KEY=... go run ./examples/export > countries.csv
//
// Walks every page of a collection and streams it out as CSV. Shows the Paginator doing
// the page walking, and that limit ceilings differ per endpoint.
package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"strconv"

	goalapi "github.com/goal-api/goal-api-go"
)

type country struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Code     string `json:"code"`
	IsActive bool   `json:"isActive"`
}

func main() {
	apiKey := os.Getenv("GOAL_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "GOAL_API_KEY is not set.")
		os.Exit(2)
	}

	client, err := goalapi.New(apiKey)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx := context.Background()

	out := csv.NewWriter(os.Stdout)
	defer out.Flush()
	_ = out.Write([]string{"id", "name", "code", "isActive"})

	// /countries accepts limit up to 500; most endpoints cap at 100.
	pager := client.Paginate(func(ctx context.Context, p goalapi.Params) (*goalapi.Page, error) {
		return client.Countries.List(ctx, p)
	}, &goalapi.PaginateOptions{PageSize: 500})

	rows := 0
	for pager.Next(ctx) {
		var row country
		if err := pager.Into(&row); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		_ = out.Write([]string{row.ID, row.Name, row.Code, strconv.FormatBool(row.IsActive)})
		rows++
	}
	if err := pager.Err(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	out.Flush()

	quota := client.RateLimit()
	fmt.Fprintf(os.Stderr, "\n%d countries exported\n", rows)
	fmt.Fprintf(os.Stderr, "quota: %d/%d left\n", quota.Remaining, quota.Limit)
}
