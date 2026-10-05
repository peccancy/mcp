// Package tools defines what an agent can ask disputes.online: four read-only
// tools over the public data.
package tools

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/peccancy/mcp/internal/platform"
)

const siteURL = "https://disputes.online"

// Instructions is sent to every client on connect. It carries what an agent
// has to know to read the numbers correctly, so each tool description can stay
// about its own job.
const Instructions = `disputes.online is a social prediction platform. People open "disputes" — questions about real-world events with two or more outcomes — and stake the platform's internal currency, QOT, on the outcome they expect.

Reading the numbers:
- Payouts are pari-mutuel: winners share the whole pool. An outcome's implied_probability is its share of the stakes — what the crowd's money says, not a forecast by the platform. Give it little weight when total_bets is small.
- payout_multiplier is what a winning stake is multiplied by at the current split; it moves until betting closes.
- Amounts are in QOT, an internal currency. Never present them as dollars.

Languages: every dispute is written in one language (en, ua, pl, de, …) or marked "all". Lists return one language plus "all", and default to English — pass lang to see disputes written in another language.

When you cite a dispute, link its url and mention the pool size or number of bets.

get_dispute also finds disputes that have been settled: it returns which outcome won next to how the stakes had been split, so a past prediction can be checked against what happened.

Everything here is read-only. Placing bets and creating disputes happen on the site.`

// Platform is what the tools need from disputes.online.
type Platform interface {
	ListDisputes(ctx context.Context, q platform.ListQuery) ([]platform.Dispute, int, error)
	SearchByTags(ctx context.Context, tags []string, lang string, limit, offset int) ([]platform.Dispute, int, error)
	GetDispute(ctx context.Context, id string) (*platform.Dispute, error)
	GetSettled(ctx context.Context, id string) (*platform.Settled, error)
	Categories(ctx context.Context) ([]platform.Category, error)
}

// Outcome is one possible result of a dispute.
type Outcome struct {
	ID                 string  `json:"id" jsonschema:"outcome id"`
	Name               string  `json:"name" jsonschema:"the outcome as its author worded it"`
	StakedQOT          float64 `json:"staked_qot" jsonschema:"QOT staked on this outcome"`
	Bets               int     `json:"bets" jsonschema:"number of bets on this outcome"`
	ImpliedProbability float64 `json:"implied_probability" jsonschema:"this outcome's share of all stakes, from 0 to 1; 0 when nobody has bet on the dispute yet"`
	PayoutMultiplier   float64 `json:"payout_multiplier" jsonschema:"what a winning stake is multiplied by at the current split of stakes; 0 when nobody has backed this outcome, and for a settled dispute"`
	Won                bool    `json:"won,omitempty" jsonschema:"true for the outcome that won; only ever set on a settled dispute"`
}

// Dispute is a dispute as the tools present it.
type Dispute struct {
	ID             string    `json:"id" jsonschema:"dispute id (UUID)"`
	URL            string    `json:"url" jsonschema:"the dispute's page on disputes.online; link this when citing"`
	Title          string    `json:"title" jsonschema:"the question, taken from the first line of the description"`
	Description    string    `json:"description" jsonschema:"the full text, including how the result will be decided"`
	Status         string    `json:"status" jsonschema:"new: no bets yet; in_process, hot, very_hot: accepting bets, by popularity; ready: betting closed, awaiting the result; done or dispute: decided but still being paid out or contested; settled: decided and archived, with the winning outcome marked"`
	Language       string    `json:"language,omitempty" jsonschema:"language code, or all; absent for a settled dispute"`
	Category       string    `json:"category,omitempty" jsonschema:"category name"`
	CategoryID     int       `json:"category_id,omitempty" jsonschema:"category id"`
	Country        string    `json:"country,omitempty" jsonschema:"ISO 3166-1 alpha-2 country the dispute is about"`
	Tags           []string  `json:"tags" jsonschema:"lowercase tags"`
	BettingCloses  string    `json:"betting_closes,omitempty" jsonschema:"when betting closes, RFC 3339 UTC; absent for a settled dispute"`
	ResultDue      string    `json:"result_due,omitempty" jsonschema:"when the result is due, RFC 3339 UTC; absent for a settled dispute"`
	SettledAt      string    `json:"settled_at,omitempty" jsonschema:"when the dispute was decided, RFC 3339 UTC; present only for a settled dispute"`
	WinningOutcome string    `json:"winning_outcome,omitempty" jsonschema:"name of the outcome that won; present only for a settled dispute that had a winner"`
	DatesEstimated bool      `json:"dates_estimated" jsonschema:"true when the event has no fixed date and the two dates are estimates"`
	TotalBets      int       `json:"total_bets" jsonschema:"number of bets placed"`
	TotalStakedQOT float64   `json:"total_staked_qot" jsonschema:"pool size in QOT, the platform's internal currency — not dollars"`
	Outcomes       []Outcome `json:"outcomes" jsonschema:"the possible results and how the stakes are split between them — for a settled dispute, how they were split when it was decided"`
}

// DisputePage is one page of a list of disputes.
type DisputePage struct {
	Total    int       `json:"total" jsonschema:"number of disputes matching the request across all pages"`
	Offset   int       `json:"offset" jsonschema:"how many disputes were skipped before this page"`
	Disputes []Dispute `json:"disputes" jsonschema:"this page of disputes"`
}

// Category is one node of the category tree.
type Category struct {
	ID       int    `json:"id" jsonschema:"category id, as accepted by search_disputes"`
	Name     string `json:"name" jsonschema:"category name"`
	ParentID int    `json:"parent_id,omitempty" jsonschema:"id of the parent category; absent for a top-level category"`
}

// CategoryList is the whole category tree.
type CategoryList struct {
	Categories []Category `json:"categories" jsonschema:"every category, as a flat list"`
}

// SearchInput filters the list of open disputes.
type SearchInput struct {
	Lang       string `json:"lang,omitempty" jsonschema:"language code of the disputes to return: en, ua, pl, de and so on. Defaults to en. Disputes marked all are always included."`
	CategoryID int    `json:"category_id,omitempty" jsonschema:"only disputes in this category or any category below it; ids come from list_categories"`
	Country    string `json:"country,omitempty" jsonschema:"ISO 3166-1 alpha-2 code of the country the dispute is about, for example US"`
	Sort       string `json:"sort,omitempty" jsonschema:"amount (largest pool first; the default), bets (most bets first), closing_soon (betting closes soonest first), result_soon (result due soonest first)"`
	Limit      int    `json:"limit,omitempty" jsonschema:"page size, 1 to 20; defaults to 10"`
	Offset     int    `json:"offset,omitempty" jsonschema:"number of disputes to skip, for paging"`
}

// TagSearchInput searches disputes by tag.
type TagSearchInput struct {
	Tags   []string `json:"tags" jsonschema:"one or more tags; a dispute carrying any of them matches. Tags are single lowercase words, for example election or bitcoin."`
	Lang   string   `json:"lang,omitempty" jsonschema:"language code of the disputes to return; defaults to en"`
	Limit  int      `json:"limit,omitempty" jsonschema:"page size, 1 to 20; defaults to 10"`
	Offset int      `json:"offset,omitempty" jsonschema:"number of disputes to skip, for paging"`
}

// GetInput names one dispute.
type GetInput struct {
	Dispute string `json:"dispute" jsonschema:"the dispute's id (UUID) or the URL of its page on disputes.online"`
}

// NoInput is for tools that take no arguments.
type NoInput struct{}

// sorts maps the names the tools offer onto the API's sort field and direction.
var sorts = map[string][2]string{
	"":             {"amount", "desc"},
	"amount":       {"amount", "desc"},
	"bets":         {"count_of_bets", "desc"},
	"closing_soon": {"stop_date", "asc"},
	"result_soon":  {"finish_date", "asc"},
}

var uuidPattern = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

// Register adds the tools to the server.
func Register(server *mcp.Server, p Platform, log *slog.Logger) {
	h := handlers{p: p, log: log}

	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_disputes",
		Description: "List disputes on disputes.online that are open to bets, with each outcome's share of the stakes. Filter by language, category and country; sort by pool size, number of bets or closing time. Use it to find what people are currently predicting about a topic, or what the crowd thinks the odds are.",
		Annotations: readOnly("Search open disputes"),
	}, h.search)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "search_disputes_by_tags",
		Description: "Find open disputes on disputes.online by tag. Use it for a specific subject — a person, a team, an asset — when category and country are too coarse.",
		Annotations: readOnly("Search disputes by tag"),
	}, h.searchByTags)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "get_dispute",
		Description: "Get one dispute from disputes.online by its id or page URL: the question, how it will be decided, the outcomes, how the stakes are split between them, and the deadlines. Also finds a dispute that has already been settled and returns which outcome won next to how the crowd had bet — use it to check a past prediction.",
		Annotations: readOnly("Get a dispute"),
	}, h.get)

	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_categories",
		Description: "List the categories disputes on disputes.online are filed under, as a flat tree. Use it to find the category_id for a topic before calling search_disputes.",
		Annotations: readOnly("List categories"),
	}, h.categories)
}

func readOnly(title string) *mcp.ToolAnnotations {
	closed := false
	return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: &closed}
}

type handlers struct {
	p   Platform
	log *slog.Logger
}

func (h handlers) search(ctx context.Context, _ *mcp.CallToolRequest, in SearchInput) (*mcp.CallToolResult, DisputePage, error) {
	order, ok := sorts[in.Sort]
	if !ok {
		return nil, DisputePage{}, fmt.Errorf("unknown sort %q: use amount, bets, closing_soon or result_soon", in.Sort)
	}

	q := platform.ListQuery{
		Lang:    strings.ToLower(strings.TrimSpace(in.Lang)),
		Country: strings.ToUpper(strings.TrimSpace(in.Country)),
		Sort:    order[0],
		Order:   order[1],
		Limit:   pageSize(in.Limit),
		Offset:  max(in.Offset, 0),
	}

	// Categories are fetched even without a filter, to name each dispute's
	// category. Failing to get them is not worth failing the search over —
	// unless the caller asked for a category, which cannot be honoured blind.
	categories, err := h.p.Categories(ctx)
	if in.CategoryID != 0 {
		if err != nil {
			return nil, DisputePage{}, h.platformError(err)
		}
		q.CategoryIDs = withDescendants(categories, in.CategoryID)
		if q.CategoryIDs == nil {
			return nil, DisputePage{}, fmt.Errorf("no category with id %d: see list_categories", in.CategoryID)
		}
	}

	disputes, total, err := h.p.ListDisputes(ctx, q)
	if err != nil {
		return nil, DisputePage{}, h.platformError(err)
	}
	return nil, page(disputes, total, q.Offset, categories), nil
}

func (h handlers) searchByTags(ctx context.Context, _ *mcp.CallToolRequest, in TagSearchInput) (*mcp.CallToolResult, DisputePage, error) {
	tags := make([]string, 0, len(in.Tags))
	for _, t := range in.Tags {
		if t = strings.ToLower(strings.TrimSpace(t)); t != "" {
			tags = append(tags, t)
		}
	}
	if len(tags) == 0 {
		return nil, DisputePage{}, errors.New("give at least one tag")
	}

	offset := max(in.Offset, 0)
	disputes, total, err := h.p.SearchByTags(ctx, tags, strings.ToLower(strings.TrimSpace(in.Lang)), pageSize(in.Limit), offset)
	if err != nil {
		return nil, DisputePage{}, h.platformError(err)
	}
	categories, _ := h.p.Categories(ctx)
	return nil, page(disputes, total, offset, categories), nil
}

func (h handlers) get(ctx context.Context, _ *mcp.CallToolRequest, in GetInput) (*mcp.CallToolResult, Dispute, error) {
	id := uuidPattern.FindString(in.Dispute)
	if id == "" {
		return nil, Dispute{}, errors.New("give the dispute's id (a UUID) or the URL of its page")
	}
	id = strings.ToLower(id)

	d, err := h.p.GetDispute(ctx, id)
	if err == nil {
		categories, _ := h.p.Categories(ctx)
		return nil, present(*d, categoryNames(categories)), nil
	}
	if !errors.Is(err, platform.ErrNotFound) {
		return nil, Dispute{}, h.platformError(err)
	}

	// Not among the open disputes: once decided, a dispute moves to the archive.
	settled, err := h.p.GetSettled(ctx, id)
	if err == nil {
		return nil, presentSettled(*settled), nil
	}
	if !errors.Is(err, platform.ErrNotFound) {
		return nil, Dispute{}, h.platformError(err)
	}
	return nil, Dispute{}, fmt.Errorf("no dispute with id %s: it never existed, was removed, or belongs to a private team", id)
}

func (h handlers) categories(ctx context.Context, _ *mcp.CallToolRequest, _ NoInput) (*mcp.CallToolResult, CategoryList, error) {
	categories, err := h.p.Categories(ctx)
	if err != nil {
		return nil, CategoryList{}, h.platformError(err)
	}
	out := CategoryList{Categories: make([]Category, 0, len(categories))}
	for _, c := range categories {
		item := Category{ID: c.ID, Name: c.Name}
		if c.ParentID != nil {
			item.ParentID = *c.ParentID
		}
		out.Categories = append(out.Categories, item)
	}
	return nil, out, nil
}

// platformError logs the cause and gives the agent something it can act on.
// The cause itself names internal addresses, which are nobody's business.
func (h handlers) platformError(err error) error {
	h.log.Error("platform request failed", "error", err)
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return errors.New("disputes.online took too long to answer; try again")
	}
	return errors.New("disputes.online could not answer right now; try again shortly")
}

func page(disputes []platform.Dispute, total, offset int, categories []platform.Category) DisputePage {
	names := categoryNames(categories)
	out := DisputePage{Total: total, Offset: offset, Disputes: make([]Dispute, 0, len(disputes))}
	for _, d := range disputes {
		out.Disputes = append(out.Disputes, present(d, names))
	}
	return out
}

func present(d platform.Dispute, categories map[int]string) Dispute {
	out := Dispute{
		ID:             d.ID,
		URL:            siteURL + "/d/" + d.ID,
		Title:          title(d.Description),
		Description:    d.Description,
		Status:         d.Status,
		Language:       d.Lang,
		Tags:           d.Tags,
		BettingCloses:  d.StopDate.UTC().Format(time.RFC3339),
		ResultDue:      d.FinishDate.UTC().Format(time.RFC3339),
		DatesEstimated: d.UncertainDate,
		TotalBets:      d.TotalBets,
		TotalStakedQOT: d.TotalAmount,
		Outcomes:       make([]Outcome, 0, len(d.Variants)),
	}
	if out.Tags == nil {
		out.Tags = []string{}
	}
	if d.CategoryID != nil {
		out.CategoryID = *d.CategoryID
		out.Category = categories[*d.CategoryID]
	}
	if d.Country != nil {
		out.Country = *d.Country
	}

	for _, v := range d.Variants {
		o := Outcome{ID: v.ID, Name: v.Description, StakedQOT: v.Amount, Bets: v.CountOfBets}
		if d.TotalAmount > 0 {
			o.ImpliedProbability = math.Round(v.Amount/d.TotalAmount*10000) / 10000
		}
		// The API reports a multiplier of 1 for an outcome nobody has backed,
		// which reads as "you get your stake back". There is simply no price yet.
		if v.Amount > 0 {
			o.PayoutMultiplier = v.Coefficient
		}
		out.Outcomes = append(out.Outcomes, o)
	}
	return out
}

// presentSettled shows an archived dispute: what the crowd had staked on each
// outcome when it was decided, and which outcome won.
func presentSettled(s platform.Settled) Dispute {
	heading := s.Title
	if heading == "" {
		heading = title(s.Description)
	}
	out := Dispute{
		ID:             s.ID,
		URL:            siteURL + "/h/" + s.ID,
		Title:          heading,
		Description:    s.Description,
		Status:         "settled",
		Tags:           []string{},
		SettledAt:      s.FinishedAt.UTC().Format(time.RFC3339),
		TotalBets:      s.TotalBets,
		TotalStakedQOT: s.TotalAmount,
		Outcomes:       make([]Outcome, 0, len(s.Variants)),
	}

	// The archive keeps the pool size on the dispute and the stakes on each
	// outcome separately. Shares are taken from the outcomes' own sum so that
	// they add up to 1 whatever the dispute-level figure says.
	var staked float64
	for _, v := range s.Variants {
		staked += v.Amount
	}
	for _, v := range s.Variants {
		o := Outcome{ID: v.ID, Name: v.Description, StakedQOT: v.Amount, Bets: v.CountOfBets, Won: v.IsWinner}
		if staked > 0 {
			o.ImpliedProbability = math.Round(v.Amount/staked*10000) / 10000
		}
		if v.IsWinner {
			out.WinningOutcome = v.Description
		}
		out.Outcomes = append(out.Outcomes, o)
	}
	return out
}

// title is the first non-empty line of a description, which is what the site
// shows as the dispute's heading.
func title(description string) string {
	for _, line := range strings.Split(description, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

func categoryNames(categories []platform.Category) map[int]string {
	names := make(map[int]string, len(categories))
	for _, c := range categories {
		names[c.ID] = c.Name
	}
	return names
}

// withDescendants returns the category and everything below it, or nil when
// there is no such category. The API matches category ids exactly, so asking it
// for "Sport" alone would miss every dispute filed under a sport.
func withDescendants(categories []platform.Category, root int) []int {
	children := make(map[int][]int)
	known := false
	for _, c := range categories {
		if c.ID == root {
			known = true
		}
		if c.ParentID != nil {
			children[*c.ParentID] = append(children[*c.ParentID], c.ID)
		}
	}
	if !known {
		return nil
	}

	ids := []int{root}
	seen := map[int]bool{root: true}
	for i := 0; i < len(ids); i++ {
		for _, child := range children[ids[i]] {
			// Guards against a cycle in the data turning this into a loop.
			if !seen[child] {
				seen[child] = true
				ids = append(ids, child)
			}
		}
	}
	return ids
}

func pageSize(limit int) int {
	if limit <= 0 {
		return 10
	}
	return min(limit, platform.PageLimit)
}
