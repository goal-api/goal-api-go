package goalapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
)

// Params are query parameters. Nil and empty values are dropped, so you can build one
// unconditionally:
//
//	goalapi.Params{"leagueId": leagueID, "status": "SCHEDULED", "limit": 100}
//
// Booleans go out as "true"/"false", which is what the validators check for. Slices are
// comma-joined, as /players/compare expects. Accepted keys: ENDPOINTS.md
type Params map[string]any

func (p Params) encode() string {
	if len(p) == 0 {
		return ""
	}
	values := url.Values{}
	for key, raw := range p {
		if encoded, ok := encodeValue(raw); ok {
			values.Set(key, encoded)
		}
	}
	return values.Encode()
}

func encodeValue(raw any) (string, bool) {
	switch v := raw.(type) {
	case nil:
		return "", false
	case string:
		return v, v != ""
	case bool:
		return strconv.FormatBool(v), true
	case int:
		return strconv.Itoa(v), true
	case int32:
		return strconv.FormatInt(int64(v), 10), true
	case int64:
		return strconv.FormatInt(v, 10), true
	case float32:
		return strconv.FormatFloat(float64(v), 'f', -1, 32), true
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), true
	case []string:
		joined := strings.Join(v, ",")
		return joined, joined != ""
	case []int:
		parts := make([]string, len(v))
		for i, n := range v {
			parts[i] = strconv.Itoa(n)
		}
		joined := strings.Join(parts, ",")
		return joined, joined != ""
	case fmt.Stringer:
		s := v.String()
		return s, s != ""
	default:
		s := fmt.Sprint(v)
		return s, s != ""
	}
}

// Pagination is the envelope's pagination block on list endpoints.
type Pagination struct {
	Total   int  `json:"total"`
	Limit   int  `json:"limit"`
	Offset  int  `json:"offset"`
	HasMore bool `json:"hasMore"`
}

// Page is a collection response. Data stays json.RawMessage because rows are
// provider-shaped and change; decode into your own type:
//
//	var teams []Team
//	if err := page.Into(&teams); err != nil { ... }
type Page struct {
	Success    bool            `json:"success"`
	Data       json.RawMessage `json:"data"`
	Pagination *Pagination     `json:"pagination,omitempty"`
	Source     string          `json:"source,omitempty"`

	// Set on the betting endpoints (/fixtures/{id}/odds, /live-odds, /commentary).
	FixtureID  string `json:"fixtureId,omitempty"`
	MatchAPIID string `json:"matchApiId,omitempty"`
	Count      int    `json:"count,omitempty"`
}

// Into decodes the Data array into dst, which should be a pointer to a slice.
func (p *Page) Into(dst any) error {
	if len(p.Data) == 0 {
		return nil
	}
	if err := json.Unmarshal(p.Data, dst); err != nil {
		return fmt.Errorf("goalapi: decoding page data: %w", err)
	}
	return nil
}

// Rows decodes Data as generic maps. Fine for scripts; use Into elsewhere.
func (p *Page) Rows() ([]map[string]any, error) {
	var rows []map[string]any
	if err := p.Into(&rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// Len reports how many rows Data holds.
func (p *Page) Len() int {
	rows, err := p.Rows()
	if err != nil {
		return 0
	}
	return len(rows)
}

// Raw is a response body with no {success, data} envelope. The /public/* endpoints return
// bare objects, so Page or Item would invent a .data field that isn't there.
type Raw json.RawMessage

// Into decodes the body into dst.
func (r Raw) Into(dst any) error {
	if len(r) == 0 {
		return nil
	}
	if err := json.Unmarshal(r, dst); err != nil {
		return fmt.Errorf("goalapi: decoding response: %w", err)
	}
	return nil
}

// Map decodes the body as a generic map.
func (r Raw) Map() (map[string]any, error) {
	var out map[string]any
	if err := r.Into(&out); err != nil {
		return nil, err
	}
	return out, nil
}

// MarshalJSON round-trips the original bytes.
func (r Raw) MarshalJSON() ([]byte, error) {
	if len(r) == 0 {
		return []byte("null"), nil
	}
	return r, nil
}

// UnmarshalJSON keeps the body verbatim.
func (r *Raw) UnmarshalJSON(data []byte) error {
	*r = append((*r)[0:0], data...)
	return nil
}

func (r Raw) String() string { return string(r) }

// Item is a single-resource response.
type Item struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
	Source  string          `json:"source,omitempty"`

	FixtureID  string `json:"fixtureId,omitempty"`
	MatchAPIID string `json:"matchApiId,omitempty"`
}

// Into decodes the Data object into dst, a pointer to your struct.
func (i *Item) Into(dst any) error {
	if len(i.Data) == 0 || string(i.Data) == "null" {
		return nil
	}
	if err := json.Unmarshal(i.Data, dst); err != nil {
		return fmt.Errorf("goalapi: decoding item data: %w", err)
	}
	return nil
}

// Row decodes Data as a generic map.
func (i *Item) Row() (map[string]any, error) {
	var row map[string]any
	if err := i.Into(&row); err != nil {
		return nil, err
	}
	return row, nil
}

// RateLimit is the quota reported by the last response.
type RateLimit struct {
	// Limit is the plan's ceiling for the current window.
	Limit int
	// Remaining is how many calls are left in it.
	Remaining int
	// Reset is when the window rolls over, as unix seconds.
	Reset int64
	// Type is "DAILY" or "MONTHLY".
	Type string
}

type rateLimitStore struct {
	mu    sync.RWMutex
	value RateLimit
}

func (s *rateLimitStore) store(headers http.Header) {
	// /public/* sends RateLimit-* (draft standard) instead, so leave the snapshot alone.
	if headers.Get("X-RateLimit-Limit") == "" && headers.Get("X-RateLimit-Type") == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.value = RateLimit{
		Limit:     atoiOrZero(headers.Get("X-RateLimit-Limit")),
		Remaining: atoiOrZero(headers.Get("X-RateLimit-Remaining")),
		Reset:     int64(atoiOrZero(headers.Get("X-RateLimit-Reset"))),
		Type:      headers.Get("X-RateLimit-Type"),
	}
}

func (s *rateLimitStore) load() RateLimit {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.value
}

// Values accepted by the "status" query param.
const (
	StatusScheduled = "SCHEDULED"
	StatusLive      = "LIVE"
	StatusFinished  = "FINISHED"
	StatusHalfTime  = "HALF_TIME"
	StatusAfterET   = "AFTER_ET"
	StatusAfterPen  = "AFTER_PEN"
	StatusPostponed = "POSTPONED"
	StatusCancelled = "CANCELLED"
	StatusAwarded   = "AWARDED"
	StatusAbandoned = "ABANDONED"
	StatusSuspended = "SUSPENDED"
)

// Values accepted by the "type" query param on player endpoints.
const (
	PlayerTypeGoalkeepers = "Goalkeepers"
	PlayerTypeDefenders   = "Defenders"
	PlayerTypeMidfielders = "Midfielders"
	PlayerTypeForwards    = "Forwards"
)

// Values accepted as the stat path segment of /players/top/{stat}.
const (
	StatGoals         = "goals"
	StatAssists       = "assists"
	StatYellowCards   = "yellowCards"
	StatRedCards      = "redCards"
	StatRating        = "rating"
	StatMatchPlayed   = "matchPlayed"
	StatMinutes       = "minutes"
	StatSaves         = "saves"
	StatTackles       = "tackles"
	StatShotsTotal    = "shotsTotal"
	StatKeyPasses     = "keyPasses"
	StatPasses        = "passes"
	StatInterceptions = "interceptions"
	StatDuelsWon      = "duelsWon"
	StatDribbleSucc   = "dribbleSucc"
)

// Values accepted by the "half" query param on fixture statistics.
const (
	HalfFull   = "full"
	HalfFirst  = "1half"
	HalfSecond = "2half"
)

// MatchStatuses lists every value the "status" query param accepts.
var MatchStatuses = []string{
	StatusScheduled, StatusLive, StatusFinished, StatusHalfTime, StatusAfterET,
	StatusAfterPen, StatusPostponed, StatusCancelled, StatusAwarded, StatusAbandoned,
	StatusSuspended,
}

// PlayerStats lists every value /players/top/{stat} accepts.
var PlayerStats = []string{
	StatGoals, StatAssists, StatYellowCards, StatRedCards, StatRating, StatMatchPlayed,
	StatMinutes, StatSaves, StatTackles, StatShotsTotal, StatKeyPasses, StatPasses,
	StatInterceptions, StatDuelsWon, StatDribbleSucc,
}
