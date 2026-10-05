// Package platform reads disputes.online through its public API.
//
// The MCP server deliberately has no database access of its own: everything it
// can say is something the public API already says, so it cannot leak anything
// the site does not publish.
package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrNotFound is returned when the platform has no such dispute.
var ErrNotFound = errors.New("not found")

const (
	// PageLimit is the most the API returns per request.
	PageLimit = 20
	// categoriesTTL: the category tree changes a few times a year.
	categoriesTTL = 10 * time.Minute
	// maxBody guards against an upstream answering with something enormous.
	maxBody = 4 << 20
)

// Dispute is a dispute as the public API returns it.
type Dispute struct {
	ID            string    `json:"id"`
	Description   string    `json:"description"`
	Variants      []Variant `json:"variants"`
	Status        string    `json:"status"`
	Lang          string    `json:"lang"`
	CategoryID    *int      `json:"category_id"`
	Country       *string   `json:"country"`
	City          *string   `json:"city"`
	Image         *string   `json:"image"`
	StopDate      time.Time `json:"stop_date"`
	FinishDate    time.Time `json:"finish_date"`
	UncertainDate bool      `json:"uncertain_date"`
	TotalBets     int       `json:"total_bets"`
	TotalAmount   float64   `json:"total_amount"`
	Tags          []string  `json:"tags"`
	CreatedAt     int64     `json:"created_at"`
}

// Variant is one outcome of a dispute.
type Variant struct {
	ID          string  `json:"id"`
	Description string  `json:"description"`
	Amount      float64 `json:"amount"`
	CountOfBets int     `json:"count_of_bets"`
	Coefficient float64 `json:"coefficient"`
}

// Settled is a dispute that has been decided and moved to the archive.
type Settled struct {
	ID          string
	Title       string
	Description string
	FinishedAt  time.Time
	TotalBets   int
	TotalAmount float64
	Variants    []SettledVariant
}

// SettledVariant is one outcome of a settled dispute.
type SettledVariant struct {
	ID          string
	Description string
	Amount      float64
	CountOfBets int
	IsWinner    bool
}

// Category is one node of the category tree.
type Category struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	ParentID *int   `json:"parent_id"`
}

// ListQuery filters the list of disputes open to bets.
type ListQuery struct {
	Lang        string
	CategoryIDs []int
	Country     string
	Sort        string
	Order       string
	Limit       int
	Offset      int
}

// Client talks to the dispute and category services.
type Client struct {
	disputesURL   string
	categoriesURL string
	historyURL    string
	http          *http.Client

	mu           sync.Mutex
	categories   []Category
	categoriesAt time.Time
}

// New returns a client. The three arguments are the base URLs of the dispute,
// category and history services: inside the cluster http://disputes:8080,
// http://categories:8082 and http://history:8095, from outside
// https://disputes.online/disputes, /category and /history.
func New(disputesURL, categoriesURL, historyURL string, timeout time.Duration) *Client {
	return &Client{
		disputesURL:   strings.TrimRight(disputesURL, "/"),
		categoriesURL: strings.TrimRight(categoriesURL, "/"),
		historyURL:    strings.TrimRight(historyURL, "/"),
		http:          &http.Client{Timeout: timeout},
	}
}

// ListDisputes returns one page of disputes still accepting bets and the total
// number matching the query.
func (c *Client) ListDisputes(ctx context.Context, q ListQuery) ([]Dispute, int, error) {
	v := url.Values{}
	// Only moderated disputes: the unmoderated ones are not shown on the site
	// either, and an agent should not quote what the site itself withholds.
	v.Set("moderation", "true")
	setPaging(v, q.Limit, q.Offset)
	if q.Lang != "" {
		v.Set("lang", q.Lang)
	}
	if len(q.CategoryIDs) > 0 {
		ids := make([]string, len(q.CategoryIDs))
		for i, id := range q.CategoryIDs {
			ids[i] = strconv.Itoa(id)
		}
		v.Set("category_ids", strings.Join(ids, ","))
	}
	if q.Country != "" {
		v.Set("country", q.Country)
	}
	if q.Sort != "" {
		v.Set("sort", q.Sort)
	}
	if q.Order != "" {
		v.Set("sortby", q.Order)
	}
	return c.disputePage(ctx, c.disputesURL+"/finder?"+v.Encode())
}

// SearchByTags returns one page of disputes carrying any of the tags.
func (c *Client) SearchByTags(ctx context.Context, tags []string, lang string, limit, offset int) ([]Dispute, int, error) {
	v := url.Values{}
	v.Set("tags", strings.Join(tags, ","))
	setPaging(v, limit, offset)
	if lang != "" {
		v.Set("lang", lang)
	}
	return c.disputePage(ctx, c.disputesURL+"/search?"+v.Encode())
}

// GetDispute returns one dispute by its UUID.
func (c *Client) GetDispute(ctx context.Context, id string) (*Dispute, error) {
	var out struct {
		Data *Dispute `json:"data"`
	}
	if err := c.get(ctx, c.disputesURL+"/"+url.PathEscape(id), &out); err != nil {
		return nil, err
	}
	if out.Data == nil {
		return nil, ErrNotFound
	}
	return out.Data, nil
}

// GetSettled returns a dispute from the archive: one that has been decided.
// The archive holds private team disputes too; those answer 403 and are
// reported as not found, the same as a dispute that never existed.
func (c *Client) GetSettled(ctx context.Context, id string) (*Settled, error) {
	var out struct {
		Dispute *struct {
			ID             string    `json:"id"`
			Title          string    `json:"title"`
			Description    string    `json:"description"`
			FinishedAt     time.Time `json:"finished_at"`
			BetCount       int       `json:"bet_count"`
			TotalBetAmount float64   `json:"total_bet_amount"`
		} `json:"dispute"`
		Variants []struct {
			ID          string `json:"id"`
			Description string `json:"description"`
			IsWinner    bool   `json:"is_winner"`
			Metadata    struct {
				Amount      float64 `json:"amount"`
				CountOfBets int     `json:"count_of_bets"`
			} `json:"metadata"`
		} `json:"variants"`
	}
	if err := c.get(ctx, c.historyURL+"/disputes/"+url.PathEscape(id), &out); err != nil {
		return nil, err
	}
	if out.Dispute == nil {
		return nil, ErrNotFound
	}

	settled := &Settled{
		ID:          out.Dispute.ID,
		Title:       out.Dispute.Title,
		Description: out.Dispute.Description,
		FinishedAt:  out.Dispute.FinishedAt,
		TotalBets:   out.Dispute.BetCount,
		TotalAmount: out.Dispute.TotalBetAmount,
		Variants:    make([]SettledVariant, 0, len(out.Variants)),
	}
	for _, v := range out.Variants {
		settled.Variants = append(settled.Variants, SettledVariant{
			ID:          v.ID,
			Description: v.Description,
			Amount:      v.Metadata.Amount,
			CountOfBets: v.Metadata.CountOfBets,
			IsWinner:    v.IsWinner,
		})
	}
	return settled, nil
}

// Categories returns the whole category tree as a flat list. A stale copy is
// served when the category service is unavailable: the tree is small, changes
// rarely, and is only used to name and group things.
func (c *Client) Categories(ctx context.Context) ([]Category, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.categories != nil && time.Since(c.categoriesAt) < categoriesTTL {
		return c.categories, nil
	}

	var out struct {
		Data []Category `json:"data"`
	}
	if err := c.get(ctx, c.categoriesURL+"/", &out); err != nil {
		if c.categories != nil {
			return c.categories, nil
		}
		return nil, err
	}
	c.categories, c.categoriesAt = out.Data, time.Now()
	return c.categories, nil
}

func (c *Client) disputePage(ctx context.Context, target string) ([]Dispute, int, error) {
	var out struct {
		Data  []Dispute `json:"data"`
		Total int       `json:"total"`
	}
	if err := c.get(ctx, target, &out); err != nil {
		return nil, 0, err
	}
	return out.Data, out.Total, nil
}

func (c *Client) get(ctx context.Context, target string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "disputes-online-mcp")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("platform unavailable: %w", err)
	}
	defer resp.Body.Close()

	// The services answer 400 for an id they cannot parse, 404 for one they do
	// not have and 403 for an archived dispute that is private to a team; to a
	// caller of a public, read-only server all three mean "nothing to show".
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusBadRequest ||
		resp.StatusCode == http.StatusForbidden {
		return ErrNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("platform answered %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(out); err != nil {
		return fmt.Errorf("decode platform answer: %w", err)
	}
	return nil
}

func setPaging(v url.Values, limit, offset int) {
	if limit <= 0 || limit > PageLimit {
		limit = PageLimit
	}
	v.Set("limit", strconv.Itoa(limit))
	if offset > 0 {
		v.Set("offset", strconv.Itoa(offset))
	}
}
