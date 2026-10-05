package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/peccancy/mcp/internal/platform"
)

// fakePlatform answers from fixed data and records what it was asked.
type fakePlatform struct {
	disputes   []platform.Dispute
	total      int
	categories []platform.Category
	err        error
	catErr     error

	gotQuery platform.ListQuery
	gotTags  []string
	gotLang  string
	gotID    string
}

func (f *fakePlatform) ListDisputes(_ context.Context, q platform.ListQuery) ([]platform.Dispute, int, error) {
	f.gotQuery = q
	return f.disputes, f.total, f.err
}

func (f *fakePlatform) SearchByTags(_ context.Context, tags []string, lang string, _, _ int) ([]platform.Dispute, int, error) {
	f.gotTags, f.gotLang = tags, lang
	return f.disputes, f.total, f.err
}

func (f *fakePlatform) GetDispute(_ context.Context, id string) (*platform.Dispute, error) {
	f.gotID = id
	if f.err != nil {
		return nil, f.err
	}
	return &f.disputes[0], nil
}

func (f *fakePlatform) Categories(context.Context) ([]platform.Category, error) {
	return f.categories, f.catErr
}

func intptr(i int) *int       { return &i }
func strptr(s string) *string { return &s }

const brazilID = "01a10af3-840e-7516-b31d-7373433e5c44"

func brazil() platform.Dispute {
	return platform.Dispute{
		ID:          brazilID,
		Description: "president of Brazil\n\nWho will become the president of Brazil?",
		Variants: []platform.Variant{
			{ID: "v1", Description: "Flávio Bolsonaro", Amount: 16, CountOfBets: 3, Coefficient: 1.36},
			{ID: "v2", Description: "Lula", Amount: 6, CountOfBets: 2, Coefficient: 3.63},
			// The API reports 1 for an outcome nobody has backed.
			{ID: "v3", Description: "Someone else", Coefficient: 1},
		},
		Status:      "in_process",
		Lang:        "ua",
		CategoryID:  intptr(201),
		Country:     strptr("BR"),
		StopDate:    time.Date(2026, 10, 24, 7, 25, 2, 0, time.UTC),
		FinishDate:  time.Date(2026, 10, 25, 8, 27, 2, 0, time.UTC),
		TotalBets:   5,
		TotalAmount: 22,
	}
}

func tree() []platform.Category {
	return []platform.Category{
		{ID: 2, Name: "Political"},
		{ID: 201, Name: "Elections", ParentID: intptr(2)},
		{ID: 2011, Name: "Presidential", ParentID: intptr(201)},
		{ID: 1, Name: "Sport"},
	}
}

// connect wires a real MCP client to the server in memory, so the tests go
// through schema inference and argument validation exactly as a remote agent
// would.
func connect(t *testing.T, p Platform) *mcp.ClientSession {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, &mcp.ServerOptions{Instructions: Instructions})
	Register(server, p, slog.New(slog.NewTextHandler(io.Discard, nil)))

	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	ss, err := server.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "client", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// call runs a tool and decodes its structured result into out. It returns the
// tool's error text, empty on success.
func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any, out any) string {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: protocol error: %v", name, err)
	}
	if res.IsError {
		var b strings.Builder
		for _, c := range res.Content {
			if text, ok := c.(*mcp.TextContent); ok {
				b.WriteString(text.Text)
			}
		}
		return b.String()
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("%s: structured result does not decode: %v\n%s", name, err, raw)
	}
	return ""
}

func TestEveryToolIsDeclaredReadOnly(t *testing.T) {
	cs := connect(t, &fakePlatform{})
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"search_disputes": true, "search_disputes_by_tags": true, "get_dispute": true, "list_categories": true}
	if len(res.Tools) != len(want) {
		t.Fatalf("server offers %d tools, want %d", len(res.Tools), len(want))
	}
	for _, tool := range res.Tools {
		if !want[tool.Name] {
			t.Errorf("unexpected tool %q", tool.Name)
		}
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s is not marked read-only", tool.Name)
		}
		if tool.Description == "" {
			t.Errorf("%s has no description", tool.Name)
		}
	}
}

func TestSearchPresentsOddsAndLinks(t *testing.T) {
	p := &fakePlatform{disputes: []platform.Dispute{brazil()}, total: 1, categories: tree()}
	var got DisputePage
	if msg := call(t, connect(t, p), "search_disputes", map[string]any{"lang": "UA"}, &got); msg != "" {
		t.Fatal(msg)
	}

	if got.Total != 1 || len(got.Disputes) != 1 {
		t.Fatalf("got %d of %d disputes, want 1 of 1", len(got.Disputes), got.Total)
	}
	d := got.Disputes[0]
	if d.URL != "https://disputes.online/d/"+brazilID {
		t.Errorf("url = %q", d.URL)
	}
	if d.Title != "president of Brazil" {
		t.Errorf("title = %q, want the first line of the description", d.Title)
	}
	if d.Category != "Elections" || d.Country != "BR" {
		t.Errorf("category = %q, country = %q", d.Category, d.Country)
	}
	if d.BettingCloses != "2026-10-24T07:25:02Z" || d.ResultDue != "2026-10-25T08:27:02Z" {
		t.Errorf("dates = %q, %q", d.BettingCloses, d.ResultDue)
	}
	if d.Tags == nil {
		t.Error("tags should be an empty list, not null")
	}

	if len(d.Outcomes) != 3 {
		t.Fatalf("got %d outcomes, want 3", len(d.Outcomes))
	}
	if o := d.Outcomes[0]; o.ImpliedProbability != 0.7273 || o.PayoutMultiplier != 1.36 {
		t.Errorf("first outcome: probability %v, multiplier %v; want 0.7273 and 1.36", o.ImpliedProbability, o.PayoutMultiplier)
	}
	if o := d.Outcomes[2]; o.ImpliedProbability != 0 || o.PayoutMultiplier != 0 {
		t.Errorf("an outcome nobody backed should have no probability or multiplier, got %v and %v", o.ImpliedProbability, o.PayoutMultiplier)
	}

	if p.gotQuery.Lang != "ua" {
		t.Errorf("lang passed on as %q, want it lowercased", p.gotQuery.Lang)
	}
	if p.gotQuery.Sort != "amount" || p.gotQuery.Order != "desc" || p.gotQuery.Limit != 10 {
		t.Errorf("defaults = %+v, want the largest pools first, ten per page", p.gotQuery)
	}
}

func TestSearchTranslatesSortAndClampsPaging(t *testing.T) {
	p := &fakePlatform{categories: tree()}
	var got DisputePage
	args := map[string]any{"sort": "closing_soon", "limit": 500, "offset": 40, "country": "br"}
	if msg := call(t, connect(t, p), "search_disputes", args, &got); msg != "" {
		t.Fatal(msg)
	}
	q := p.gotQuery
	if q.Sort != "stop_date" || q.Order != "asc" {
		t.Errorf("closing_soon became %s %s, want stop_date asc", q.Sort, q.Order)
	}
	if q.Limit != platform.PageLimit || q.Offset != 40 || q.Country != "BR" {
		t.Errorf("query = %+v", q)
	}
	if got.Disputes == nil {
		t.Error("an empty page should be an empty list, not null")
	}
}

func TestSearchRejectsAnUnknownSort(t *testing.T) {
	msg := call(t, connect(t, &fakePlatform{}), "search_disputes", map[string]any{"sort": "newest"}, &DisputePage{})
	if !strings.Contains(msg, "closing_soon") {
		t.Errorf("error should list the accepted values, got %q", msg)
	}
}

// The API matches category ids exactly, so a parent has to be widened to its
// whole subtree or "Political" would find nothing filed under "Elections".
func TestSearchByCategoryIncludesSubcategories(t *testing.T) {
	p := &fakePlatform{categories: tree()}
	if msg := call(t, connect(t, p), "search_disputes", map[string]any{"category_id": 2}, &DisputePage{}); msg != "" {
		t.Fatal(msg)
	}
	got := map[int]bool{}
	for _, id := range p.gotQuery.CategoryIDs {
		got[id] = true
	}
	if len(got) != 3 || !got[2] || !got[201] || !got[2011] {
		t.Errorf("category 2 widened to %v, want 2, 201 and 2011", p.gotQuery.CategoryIDs)
	}
}

func TestSearchByUnknownCategoryFails(t *testing.T) {
	msg := call(t, connect(t, &fakePlatform{categories: tree()}), "search_disputes", map[string]any{"category_id": 999}, &DisputePage{})
	if !strings.Contains(msg, "list_categories") {
		t.Errorf("error should point at list_categories, got %q", msg)
	}
}

// Category names are a nicety; losing them must not lose the search.
func TestSearchSurvivesCategoriesBeingUnavailable(t *testing.T) {
	p := &fakePlatform{disputes: []platform.Dispute{brazil()}, total: 1, catErr: errors.New("down")}
	var got DisputePage
	if msg := call(t, connect(t, p), "search_disputes", nil, &got); msg != "" {
		t.Fatal(msg)
	}
	if len(got.Disputes) != 1 || got.Disputes[0].Category != "" || got.Disputes[0].CategoryID != 201 {
		t.Errorf("want the dispute with its category id and no name, got %+v", got.Disputes)
	}
}

func TestTagSearchNormalisesTags(t *testing.T) {
	p := &fakePlatform{}
	args := map[string]any{"tags": []string{" Election ", "", "BRAZIL"}, "lang": "en"}
	if msg := call(t, connect(t, p), "search_disputes_by_tags", args, &DisputePage{}); msg != "" {
		t.Fatal(msg)
	}
	if len(p.gotTags) != 2 || p.gotTags[0] != "election" || p.gotTags[1] != "brazil" || p.gotLang != "en" {
		t.Errorf("tags = %q, lang = %q", p.gotTags, p.gotLang)
	}

	if msg := call(t, connect(t, p), "search_disputes_by_tags", map[string]any{"tags": []string{"  "}}, &DisputePage{}); msg == "" {
		t.Error("blank tags should be refused")
	}
}

func TestGetDisputeAcceptsAnIDOrAPageURL(t *testing.T) {
	for _, ref := range []string{
		brazilID,
		strings.ToUpper(brazilID),
		"https://disputes.online/d/" + brazilID + "/president-of-brazil-who-will-become-the-president-of-brazil?utm_source=x",
	} {
		p := &fakePlatform{disputes: []platform.Dispute{brazil()}, categories: tree()}
		var got Dispute
		if msg := call(t, connect(t, p), "get_dispute", map[string]any{"dispute": ref}, &got); msg != "" {
			t.Fatalf("%s: %s", ref, msg)
		}
		if p.gotID != brazilID || got.ID != brazilID {
			t.Errorf("%s: asked the platform for %q, returned %q", ref, p.gotID, got.ID)
		}
	}
}

func TestGetDisputeErrors(t *testing.T) {
	if msg := call(t, connect(t, &fakePlatform{}), "get_dispute", map[string]any{"dispute": "the brazil one"}, &Dispute{}); !strings.Contains(msg, "UUID") {
		t.Errorf("a reference with no id in it should ask for one, got %q", msg)
	}

	p := &fakePlatform{err: platform.ErrNotFound}
	if msg := call(t, connect(t, p), "get_dispute", map[string]any{"dispute": brazilID}, &Dispute{}); !strings.Contains(msg, "archive") {
		t.Errorf("a missing dispute should mention the archive, got %q", msg)
	}
}

// What went wrong inside names internal hosts; an agent — and whoever reads its
// transcript — must not see it.
func TestPlatformFailuresDoNotLeakInternals(t *testing.T) {
	p := &fakePlatform{err: errors.New(`Get "http://disputes:8080/finder": dial tcp 10.42.0.7:8080: connection refused`)}
	msg := call(t, connect(t, p), "search_disputes", nil, &DisputePage{})
	if msg == "" {
		t.Fatal("want an error")
	}
	for _, secret := range []string{"disputes:8080", "10.42", "dial tcp"} {
		if strings.Contains(msg, secret) {
			t.Errorf("error leaks %q: %s", secret, msg)
		}
	}
}

func TestListCategories(t *testing.T) {
	var got CategoryList
	if msg := call(t, connect(t, &fakePlatform{categories: tree()}), "list_categories", nil, &got); msg != "" {
		t.Fatal(msg)
	}
	if len(got.Categories) != 4 {
		t.Fatalf("got %d categories, want 4", len(got.Categories))
	}
	if got.Categories[0].ParentID != 0 || got.Categories[1].ParentID != 2 {
		t.Errorf("parents = %d, %d; want none and 2", got.Categories[0].ParentID, got.Categories[1].ParentID)
	}
}

func TestWithDescendantsStopsOnACycle(t *testing.T) {
	loop := []platform.Category{{ID: 1, ParentID: intptr(2)}, {ID: 2, ParentID: intptr(1)}}
	if got := withDescendants(loop, 1); len(got) != 2 {
		t.Errorf("got %v, want both categories exactly once", got)
	}
}
