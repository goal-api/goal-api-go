package goalapi

import "context"

// One service per endpoint group. Accepted params and limit ceilings: ENDPOINTS.md

func (c *Client) page(ctx context.Context, path string, params Params) (*Page, error) {
	var page Page
	if err := c.Get(ctx, path, params, &page); err != nil {
		return nil, err
	}
	return &page, nil
}

func (c *Client) item(ctx context.Context, path string, params Params) (*Item, error) {
	var item Item
	if err := c.Get(ctx, path, params, &item); err != nil {
		return nil, err
	}
	return &item, nil
}

// pathOf percent-encodes and joins segments onto a prefix:
// pathOf("/standings", leagueID, "team", teamID).
func pathOf(prefix string, parts ...string) (string, error) {
	path := prefix
	for _, part := range parts {
		escaped, err := segment(part)
		if err != nil {
			return "", err
		}
		path += "/" + escaped
	}
	return path, nil
}

// StatusService covers the unauthenticated status and coverage endpoints, rate limited by
// IP rather than by key.
//
// These return Raw, not Page or Item: /public/* answers with bare objects.
type StatusService struct{ c *Client }

func (s *StatusService) raw(ctx context.Context, path string, params Params) (Raw, error) {
	var out Raw
	if err := s.c.Get(ctx, path, params, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Get returns {status, updatedAt, measurement, components[]}.
func (s *StatusService) Get(ctx context.Context) (Raw, error) {
	return s.raw(ctx, "/public/status", nil)
}

// Coverage returns {leagues, countries, teams, players, fixtures, ...}.
func (s *StatusService) Coverage(ctx context.Context) (Raw, error) {
	return s.raw(ctx, "/public/coverage", nil)
}

// CoverageLeagues returns {leagues[], total, page, limit, pages}. Paginates with "page"
// and "limit", not "offset". Also accepts "q" and "country".
func (s *StatusService) CoverageLeagues(ctx context.Context, params Params) (Raw, error) {
	return s.raw(ctx, "/public/coverage/leagues", params)
}

// CoverageCountries returns {countries[], total}.
func (s *StatusService) CoverageCountries(ctx context.Context) (Raw, error) {
	return s.raw(ctx, "/public/coverage/countries", nil)
}

// CoverageLeague returns a bare league object.
func (s *StatusService) CoverageLeague(ctx context.Context, leagueID string) (Raw, error) {
	path, err := pathOf("/public/coverage/leagues", leagueID)
	if err != nil {
		return nil, err
	}
	return s.raw(ctx, path, nil)
}

// CountriesService covers /countries.
type CountriesService struct{ c *Client }

func (s *CountriesService) List(ctx context.Context, params Params) (*Page, error) {
	return s.c.page(ctx, "/countries", params)
}

func (s *CountriesService) Get(ctx context.Context, countryID string) (*Item, error) {
	path, err := pathOf("/countries", countryID)
	if err != nil {
		return nil, err
	}
	return s.c.item(ctx, path, nil)
}

func (s *CountriesService) Leagues(ctx context.Context, countryID string, params Params) (*Page, error) {
	path, err := pathOf("/countries", countryID, "leagues")
	if err != nil {
		return nil, err
	}
	return s.c.page(ctx, path, params)
}

// LeaguesService covers /leagues.
type LeaguesService struct{ c *Client }

func (s *LeaguesService) List(ctx context.Context, params Params) (*Page, error) {
	return s.c.page(ctx, "/leagues", params)
}

func (s *LeaguesService) Get(ctx context.Context, leagueID string) (*Item, error) {
	path, err := pathOf("/leagues", leagueID)
	if err != nil {
		return nil, err
	}
	return s.c.item(ctx, path, nil)
}

func (s *LeaguesService) Teams(ctx context.Context, leagueID string, params Params) (*Page, error) {
	path, err := pathOf("/leagues", leagueID, "teams")
	if err != nil {
		return nil, err
	}
	return s.c.page(ctx, path, params)
}

func (s *LeaguesService) Standings(ctx context.Context, leagueID string, params Params) (*Page, error) {
	path, err := pathOf("/leagues", leagueID, "standings")
	if err != nil {
		return nil, err
	}
	return s.c.page(ctx, path, params)
}

func (s *LeaguesService) Fixtures(ctx context.Context, leagueID string, params Params) (*Page, error) {
	path, err := pathOf("/leagues", leagueID, "fixtures")
	if err != nil {
		return nil, err
	}
	return s.c.page(ctx, path, params)
}

func (s *LeaguesService) TopScorers(ctx context.Context, leagueID string, params Params) (*Page, error) {
	path, err := pathOf("/leagues", leagueID, "top-scorers")
	if err != nil {
		return nil, err
	}
	return s.c.page(ctx, path, params)
}

func (s *LeaguesService) Results(ctx context.Context, leagueID string, params Params) (*Page, error) {
	path, err := pathOf("/leagues", leagueID, "results")
	if err != nil {
		return nil, err
	}
	return s.c.page(ctx, path, params)
}

// TeamsService covers /teams.
type TeamsService struct{ c *Client }

func (s *TeamsService) List(ctx context.Context, params Params) (*Page, error) {
	return s.c.page(ctx, "/teams", params)
}

func (s *TeamsService) Get(ctx context.Context, teamID string, params Params) (*Item, error) {
	path, err := pathOf("/teams", teamID)
	if err != nil {
		return nil, err
	}
	return s.c.item(ctx, path, params)
}

func (s *TeamsService) Players(ctx context.Context, teamID string, params Params) (*Page, error) {
	path, err := pathOf("/teams", teamID, "players")
	if err != nil {
		return nil, err
	}
	return s.c.page(ctx, path, params)
}

func (s *TeamsService) Fixtures(ctx context.Context, teamID string, params Params) (*Page, error) {
	path, err := pathOf("/teams", teamID, "fixtures")
	if err != nil {
		return nil, err
	}
	return s.c.page(ctx, path, params)
}

func (s *TeamsService) Results(ctx context.Context, teamID string, params Params) (*Page, error) {
	path, err := pathOf("/teams", teamID, "results")
	if err != nil {
		return nil, err
	}
	return s.c.page(ctx, path, params)
}

func (s *TeamsService) Statistics(ctx context.Context, teamID string, params Params) (*Item, error) {
	path, err := pathOf("/teams", teamID, "statistics")
	if err != nil {
		return nil, err
	}
	return s.c.item(ctx, path, params)
}

func (s *TeamsService) Upcoming(ctx context.Context, teamID string, params Params) (*Page, error) {
	path, err := pathOf("/teams", teamID, "upcoming")
	if err != nil {
		return nil, err
	}
	return s.c.page(ctx, path, params)
}

// FixturesService covers /fixtures.
type FixturesService struct{ c *Client }

func (s *FixturesService) List(ctx context.Context, params Params) (*Page, error) {
	return s.c.page(ctx, "/fixtures", params)
}

// Live returns matches in play. Takes an optional "leagueId".
func (s *FixturesService) Live(ctx context.Context, params Params) (*Page, error) {
	return s.c.page(ctx, "/fixtures/live", params)
}

// ByDate takes a YYYY-MM-DD date.
func (s *FixturesService) ByDate(ctx context.Context, date string, params Params) (*Page, error) {
	path, err := pathOf("/fixtures/date", date)
	if err != nil {
		return nil, err
	}
	return s.c.page(ctx, path, params)
}

func (s *FixturesService) Get(ctx context.Context, fixtureID string) (*Item, error) {
	path, err := pathOf("/fixtures", fixtureID)
	if err != nil {
		return nil, err
	}
	return s.c.item(ctx, path, nil)
}

func (s *FixturesService) Events(ctx context.Context, fixtureID string) (*Page, error) {
	path, err := pathOf("/fixtures", fixtureID, "events")
	if err != nil {
		return nil, err
	}
	return s.c.page(ctx, path, nil)
}

func (s *FixturesService) Lineups(ctx context.Context, fixtureID string) (*Item, error) {
	path, err := pathOf("/fixtures", fixtureID, "lineups")
	if err != nil {
		return nil, err
	}
	return s.c.item(ctx, path, nil)
}

// Statistics takes an optional "half": HalfFull, HalfFirst or HalfSecond.
func (s *FixturesService) Statistics(ctx context.Context, fixtureID string, params Params) (*Item, error) {
	path, err := pathOf("/fixtures", fixtureID, "statistics")
	if err != nil {
		return nil, err
	}
	return s.c.item(ctx, path, params)
}

func (s *FixturesService) Cards(ctx context.Context, fixtureID string) (*Page, error) {
	path, err := pathOf("/fixtures", fixtureID, "cards")
	if err != nil {
		return nil, err
	}
	return s.c.page(ctx, path, nil)
}

func (s *FixturesService) Substitutions(ctx context.Context, fixtureID string) (*Page, error) {
	path, err := pathOf("/fixtures", fixtureID, "substitutions")
	if err != nil {
		return nil, err
	}
	return s.c.page(ctx, path, nil)
}

// Odds accepts a matchApiId as well as a fixture id, as do the three below.
func (s *FixturesService) Odds(ctx context.Context, fixtureID string) (*Page, error) {
	path, err := pathOf("/fixtures", fixtureID, "odds")
	if err != nil {
		return nil, err
	}
	return s.c.page(ctx, path, nil)
}

func (s *FixturesService) Predictions(ctx context.Context, fixtureID string) (*Item, error) {
	path, err := pathOf("/fixtures", fixtureID, "predictions")
	if err != nil {
		return nil, err
	}
	return s.c.item(ctx, path, nil)
}

func (s *FixturesService) LiveOdds(ctx context.Context, fixtureID string) (*Page, error) {
	path, err := pathOf("/fixtures", fixtureID, "live-odds")
	if err != nil {
		return nil, err
	}
	return s.c.page(ctx, path, nil)
}

func (s *FixturesService) Commentary(ctx context.Context, fixtureID string) (*Page, error) {
	path, err := pathOf("/fixtures", fixtureID, "commentary")
	if err != nil {
		return nil, err
	}
	return s.c.page(ctx, path, nil)
}

// StandingsService covers /standings. Every method takes an optional "stage".
//
// Home and Away 404 for leagues where the provider has no home/away split, even when the
// base table has rows.
type StandingsService struct{ c *Client }

func (s *StandingsService) Get(ctx context.Context, leagueID string, params Params) (*Page, error) {
	path, err := pathOf("/standings", leagueID)
	if err != nil {
		return nil, err
	}
	return s.c.page(ctx, path, params)
}

func (s *StandingsService) Team(ctx context.Context, leagueID, teamID string) (*Item, error) {
	path, err := pathOf("/standings", leagueID, "team", teamID)
	if err != nil {
		return nil, err
	}
	return s.c.item(ctx, path, nil)
}

func (s *StandingsService) Home(ctx context.Context, leagueID string, params Params) (*Page, error) {
	path, err := pathOf("/standings", leagueID, "home")
	if err != nil {
		return nil, err
	}
	return s.c.page(ctx, path, params)
}

func (s *StandingsService) Away(ctx context.Context, leagueID string, params Params) (*Page, error) {
	path, err := pathOf("/standings", leagueID, "away")
	if err != nil {
		return nil, err
	}
	return s.c.page(ctx, path, params)
}

func (s *StandingsService) Form(ctx context.Context, leagueID string, params Params) (*Page, error) {
	path, err := pathOf("/standings", leagueID, "form")
	if err != nil {
		return nil, err
	}
	return s.c.page(ctx, path, params)
}

func (s *StandingsService) Zones(ctx context.Context, leagueID string, params Params) (*Item, error) {
	path, err := pathOf("/standings", leagueID, "zones")
	if err != nil {
		return nil, err
	}
	return s.c.item(ctx, path, params)
}

// PlayersService covers /players.
type PlayersService struct{ c *Client }

func (s *PlayersService) List(ctx context.Context, params Params) (*Page, error) {
	return s.c.page(ctx, "/players", params)
}

// Search needs a 2-100 character query.
func (s *PlayersService) Search(ctx context.Context, query string, params Params) (*Page, error) {
	merged := mergeParams(params, "q", query)
	return s.c.page(ctx, "/players/search", merged)
}

// Compare takes 2–5 player ids.
func (s *PlayersService) Compare(ctx context.Context, ids ...string) (*Page, error) {
	return s.c.page(ctx, "/players/compare", Params{"ids": ids})
}

// Top ranks players by a stat. See the Stat* constants.
func (s *PlayersService) Top(ctx context.Context, stat string, params Params) (*Page, error) {
	path, err := pathOf("/players/top", stat)
	if err != nil {
		return nil, err
	}
	return s.c.page(ctx, path, params)
}

func (s *PlayersService) Get(ctx context.Context, playerID string) (*Item, error) {
	path, err := pathOf("/players", playerID)
	if err != nil {
		return nil, err
	}
	return s.c.item(ctx, path, nil)
}

func (s *PlayersService) Statistics(ctx context.Context, playerID string, params Params) (*Item, error) {
	path, err := pathOf("/players", playerID, "statistics")
	if err != nil {
		return nil, err
	}
	return s.c.item(ctx, path, params)
}

// CoachesService covers /coaches.
type CoachesService struct{ c *Client }

func (s *CoachesService) List(ctx context.Context, params Params) (*Page, error) {
	return s.c.page(ctx, "/coaches", params)
}

func (s *CoachesService) Search(ctx context.Context, query string, params Params) (*Page, error) {
	return s.c.page(ctx, "/coaches/search", mergeParams(params, "q", query))
}

func (s *CoachesService) ByCountry(ctx context.Context, country string, params Params) (*Page, error) {
	path, err := pathOf("/coaches/country", country)
	if err != nil {
		return nil, err
	}
	return s.c.page(ctx, path, params)
}

func (s *CoachesService) ByTeam(ctx context.Context, teamID string) (*Page, error) {
	path, err := pathOf("/coaches/team", teamID)
	if err != nil {
		return nil, err
	}
	return s.c.page(ctx, path, nil)
}

func (s *CoachesService) Get(ctx context.Context, coachID string) (*Item, error) {
	path, err := pathOf("/coaches", coachID)
	if err != nil {
		return nil, err
	}
	return s.c.item(ctx, path, nil)
}

// H2HService covers /h2h. The two ids must differ, and 404 means the teams have never met.
type H2HService struct{ c *Client }

func (s *H2HService) Get(ctx context.Context, team1ID, team2ID string) (*Item, error) {
	path, err := pathOf("/h2h", team1ID, team2ID)
	if err != nil {
		return nil, err
	}
	return s.c.item(ctx, path, nil)
}

func (s *H2HService) Direct(ctx context.Context, team1ID, team2ID string, params Params) (*Page, error) {
	path, err := pathOf("/h2h", team1ID, team2ID, "direct")
	if err != nil {
		return nil, err
	}
	return s.c.page(ctx, path, params)
}

func (s *H2HService) Stats(ctx context.Context, team1ID, team2ID string) (*Item, error) {
	path, err := pathOf("/h2h", team1ID, team2ID, "stats")
	if err != nil {
		return nil, err
	}
	return s.c.item(ctx, path, nil)
}

// ResultsService covers /results. Its list endpoints take limit up to 500.
type ResultsService struct{ c *Client }

func (s *ResultsService) List(ctx context.Context, params Params) (*Page, error) {
	return s.c.page(ctx, "/results", params)
}

func (s *ResultsService) Today(ctx context.Context) (*Page, error) {
	return s.c.page(ctx, "/results/today", nil)
}

func (s *ResultsService) Yesterday(ctx context.Context) (*Page, error) {
	return s.c.page(ctx, "/results/yesterday", nil)
}

func (s *ResultsService) Stats(ctx context.Context, params Params) (*Item, error) {
	return s.c.item(ctx, "/results/stats", params)
}

func (s *ResultsService) HighScoring(ctx context.Context, params Params) (*Page, error) {
	return s.c.page(ctx, "/results/high-scoring", params)
}

func (s *ResultsService) ByDate(ctx context.Context, date string) (*Page, error) {
	path, err := pathOf("/results/date", date)
	if err != nil {
		return nil, err
	}
	return s.c.page(ctx, path, nil)
}

func (s *ResultsService) ByLeague(ctx context.Context, leagueID string, params Params) (*Page, error) {
	path, err := pathOf("/results/league", leagueID)
	if err != nil {
		return nil, err
	}
	return s.c.page(ctx, path, params)
}

func (s *ResultsService) ByTeam(ctx context.Context, teamID string, params Params) (*Page, error) {
	path, err := pathOf("/results/team", teamID)
	if err != nil {
		return nil, err
	}
	return s.c.page(ctx, path, params)
}

// VideosService covers /videos.
type VideosService struct{ c *Client }

func (s *VideosService) List(ctx context.Context, params Params) (*Page, error) {
	return s.c.page(ctx, "/videos", params)
}

func (s *VideosService) Recent(ctx context.Context, params Params) (*Page, error) {
	return s.c.page(ctx, "/videos/recent", params)
}

func (s *VideosService) ByMatch(ctx context.Context, matchID string) (*Page, error) {
	path, err := pathOf("/videos/match", matchID)
	if err != nil {
		return nil, err
	}
	return s.c.page(ctx, path, nil)
}

func (s *VideosService) ByLeague(ctx context.Context, leagueID string, params Params) (*Page, error) {
	path, err := pathOf("/videos/league", leagueID)
	if err != nil {
		return nil, err
	}
	return s.c.page(ctx, path, params)
}

func (s *VideosService) ByDate(ctx context.Context, date string, params Params) (*Page, error) {
	path, err := pathOf("/videos/date", date)
	if err != nil {
		return nil, err
	}
	return s.c.page(ctx, path, params)
}

// OddsService covers /odds: "bookmaker", "matchId", "limit" (max 200, default 50), "offset".
type OddsService struct{ c *Client }

func (s *OddsService) List(ctx context.Context, params Params) (*Page, error) {
	return s.c.page(ctx, "/odds", params)
}

// PredictionsService covers /predictions: "matchId", "leagueName", "limit" (max 200,
// default 50), "offset".
type PredictionsService struct{ c *Client }

func (s *PredictionsService) List(ctx context.Context, params Params) (*Page, error) {
	return s.c.page(ctx, "/predictions", params)
}

// mergeParams copies before setting, so a caller's map is never mutated.
func mergeParams(params Params, key string, value any) Params {
	merged := make(Params, len(params)+1)
	for k, v := range params {
		merged[k] = v
	}
	merged[key] = value
	return merged
}
