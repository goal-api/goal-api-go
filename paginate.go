package goalapi

import (
	"context"
	"encoding/json"
)

// PageFunc fetches one page. The paginator supplies limit and offset; merge in whatever
// else the endpoint needs:
//
//	func(ctx context.Context, p Params) (*Page, error) {
//	    p["leagueId"] = leagueID
//	    return client.Teams.List(ctx, p)
//	}
type PageFunc func(ctx context.Context, params Params) (*Page, error)

// PaginateOptions tunes a Paginator.
type PaginateOptions struct {
	// PageSize defaults to 100, the limit ceiling on most endpoints. /results and
	// /countries take 500.
	PageSize int
	// MaxItems caps the total rows returned. Zero means no cap.
	MaxItems int
	// StartOffset begins partway into the collection.
	StartOffset int
}

// Paginator walks every page of a list endpoint.
//
//	pager := client.Paginate(func(ctx context.Context, p Params) (*Page, error) {
//	    return client.Leagues.Teams(ctx, leagueID, p)
//	}, nil)
//
//	for pager.Next(ctx) {
//	    var team Team
//	    if err := json.Unmarshal(pager.Row(), &team); err != nil { ... }
//	    fmt.Println(team.Name)
//	}
//	if err := pager.Err(); err != nil { ... }
type Paginator struct {
	fetch    PageFunc
	pageSize int
	maxItems int
	offset   int

	buffer  []json.RawMessage
	index   int
	yielded int
	done    bool
	err     error
	page    *Page
}

// Paginate builds a Paginator. opts may be nil for the defaults.
func (c *Client) Paginate(fetch PageFunc, opts *PaginateOptions) *Paginator {
	pageSize, maxItems, offset := 100, 0, 0
	if opts != nil {
		if opts.PageSize > 0 {
			pageSize = opts.PageSize
		}
		maxItems = opts.MaxItems
		offset = opts.StartOffset
	}
	return &Paginator{fetch: fetch, pageSize: pageSize, maxItems: maxItems, offset: offset}
}

// Next advances one row, fetching another page when the buffer empties. Returns false at
// the end or on the first error; check Err.
func (p *Paginator) Next(ctx context.Context) bool {
	if p.done || p.err != nil {
		return false
	}
	if p.maxItems > 0 && p.yielded >= p.maxItems {
		p.done = true
		return false
	}

	if p.index >= len(p.buffer) {
		if !p.fetchNext(ctx) {
			return false
		}
	}

	p.index++
	p.yielded++
	return true
}

func (p *Paginator) fetchNext(ctx context.Context) bool {
	page, err := p.fetch(ctx, Params{"limit": p.pageSize, "offset": p.offset})
	if err != nil {
		p.err = err
		return false
	}
	p.page = page

	var rows []json.RawMessage
	if unmarshalErr := page.Into(&rows); unmarshalErr != nil {
		p.err = unmarshalErr
		return false
	}
	if len(rows) == 0 {
		p.done = true
		return false
	}

	p.buffer = rows
	p.index = 0
	p.offset += len(rows)

	// Some endpoints omit pagination; a short page is the only other signal. Either way
	// the current buffer is still consumed.
	if page.Pagination != nil {
		if !page.Pagination.HasMore {
			p.exhaustAfterBuffer()
		}
	} else if len(rows) < p.pageSize {
		p.exhaustAfterBuffer()
	}
	return true
}

// exhaustAfterBuffer ends iteration once the current buffer drains.
func (p *Paginator) exhaustAfterBuffer() {
	p.fetch = func(context.Context, Params) (*Page, error) {
		return &Page{Success: true, Data: json.RawMessage("[]")}, nil
	}
}

// Row returns the current row as raw JSON. Only valid after Next returned true.
func (p *Paginator) Row() json.RawMessage {
	if p.index == 0 || p.index > len(p.buffer) {
		return nil
	}
	return p.buffer[p.index-1]
}

// Into decodes the current row into dst.
func (p *Paginator) Into(dst any) error {
	row := p.Row()
	if row == nil {
		return nil
	}
	return json.Unmarshal(row, dst)
}

// Page returns the envelope the current row came from, for Pagination.Total and Source.
func (p *Paginator) Page() *Page { return p.page }

// Err returns the error that stopped iteration.
func (p *Paginator) Err() error { return p.err }

// Count returns how many rows have been yielded.
func (p *Paginator) Count() int { return p.yielded }

// CollectInto walks every page and decodes all rows into dst, a pointer to a slice.
//
//	var teams []Team
//	err := client.CollectInto(ctx, fetch, nil, &teams)
func (c *Client) CollectInto(ctx context.Context, fetch PageFunc, opts *PaginateOptions, dst any) error {
	pager := c.Paginate(fetch, opts)
	rows := make([]json.RawMessage, 0, 128)
	for pager.Next(ctx) {
		rows = append(rows, pager.Row())
	}
	if err := pager.Err(); err != nil {
		return err
	}

	combined, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	return json.Unmarshal(combined, dst)
}

// CollectRows walks every page and returns generic maps. Fine for scripts; use
// CollectInto elsewhere.
func (c *Client) CollectRows(ctx context.Context, fetch PageFunc, opts *PaginateOptions) ([]map[string]any, error) {
	var rows []map[string]any
	if err := c.CollectInto(ctx, fetch, opts, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}
