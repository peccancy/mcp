package platform

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func serve(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(srv.URL+"/disputes/", srv.URL+"/category/", srv.URL+"/history/", time.Second)
}

func TestListDisputesBuildsTheQuery(t *testing.T) {
	var got string
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path + "?" + r.URL.Query().Encode()
		_, _ = w.Write([]byte(`{"data":[{"id":"a","description":"q","stop_date":"2026-10-24T07:25:02Z","total_amount":22.5,"variants":[{"id":"v","amount":1.5,"coefficient":2}]}],"total":7}`))
	})

	disputes, total, err := c.ListDisputes(context.Background(), ListQuery{
		Lang: "ua", CategoryIDs: []int{2, 201}, Country: "BR", Sort: "stop_date", Order: "asc", Limit: 500, Offset: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "/disputes/finder?category_ids=2%2C201&country=BR&lang=ua&limit=20&moderation=true&offset=20&sort=stop_date&sortby=asc"
	if got != want {
		t.Errorf("asked for\n %s\nwant\n %s", got, want)
	}
	if total != 7 || len(disputes) != 1 || disputes[0].TotalAmount != 22.5 || disputes[0].Variants[0].Coefficient != 2 {
		t.Errorf("decoded %+v, total %d", disputes, total)
	}
	if disputes[0].StopDate.IsZero() {
		t.Error("stop_date was not decoded")
	}
}

func TestSearchByTagsBuildsTheQuery(t *testing.T) {
	var got string
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path + "?" + r.URL.Query().Encode()
		_, _ = w.Write([]byte(`{"data":[],"total":0}`))
	})
	if _, _, err := c.SearchByTags(context.Background(), []string{"election", "brazil"}, "", 0, 0); err != nil {
		t.Fatal(err)
	}
	if want := "/disputes/search?limit=20&tags=election%2Cbrazil"; got != want {
		t.Errorf("asked for %s, want %s", got, want)
	}
}

func TestGetDisputeTreatsBadAndMissingIDsAlike(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusBadRequest} {
		c := serve(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(status) })
		if _, err := c.GetDispute(context.Background(), "x"); !errors.Is(err, ErrNotFound) {
			t.Errorf("status %d: got %v, want ErrNotFound", status, err)
		}
	}

	c := serve(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	if _, err := c.GetDispute(context.Background(), "x"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("a failing platform is not a missing dispute, got %v", err)
	}
}

func TestCategoriesAreCachedAndServedStaleOnFailure(t *testing.T) {
	var calls atomic.Int32
	fail := atomic.Bool{}
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/category/" {
			t.Errorf("asked for %s, want /category/", r.URL.Path)
		}
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":1,"name":"Sport","parent_id":null},{"id":4,"name":"Football","parent_id":1}]}`))
	})

	first, err := c.Categories(context.Background())
	if err != nil || len(first) != 2 || first[1].ParentID == nil || *first[1].ParentID != 1 {
		t.Fatalf("got %+v, %v", first, err)
	}
	if _, err := c.Categories(context.Background()); err != nil || calls.Load() != 1 {
		t.Errorf("a second read should come from the cache; %d requests made, err %v", calls.Load(), err)
	}

	// Expire the cache and break the service: the old tree is still better than none.
	c.categoriesAt = time.Now().Add(-2 * categoriesTTL)
	fail.Store(true)
	stale, err := c.Categories(context.Background())
	if err != nil || len(stale) != 2 {
		t.Errorf("want the stale tree, got %+v, %v", stale, err)
	}
}

func TestCategoriesFailWithNothingCached(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusInternalServerError) })
	if _, err := c.Categories(context.Background()); err == nil {
		t.Error("want an error when there is no tree to fall back on")
	}
}

func TestGetSettledReadsTheArchive(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/history/disputes/abc" {
			t.Errorf("asked for %s, want /history/disputes/abc", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"dispute":{"id":"abc","title":"Who wins?","description":"Who wins?","finished_at":"2026-10-05T10:22:59.792809Z","bet_count":4,"total_bet_amount":12.5},
			"variants":[{"id":"v1","description":"Comets","is_winner":true,"metadata":{"amount":10,"count_of_bets":3}},
			            {"id":"v2","description":"Bears","is_winner":false,"metadata":{"amount":2.5,"count_of_bets":1}}]}`))
	})

	got, err := c.GetSettled(context.Background(), "abc")
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "Who wins?" || got.TotalBets != 4 || got.TotalAmount != 12.5 || got.FinishedAt.IsZero() {
		t.Errorf("dispute decoded as %+v", got)
	}
	if len(got.Variants) != 2 || !got.Variants[0].IsWinner || got.Variants[0].Amount != 10 || got.Variants[1].CountOfBets != 1 {
		t.Errorf("variants decoded as %+v", got.Variants)
	}
}

// The archive also holds disputes private to a team. A public server must not
// distinguish "private" from "absent".
func TestGetSettledHidesPrivateDisputes(t *testing.T) {
	c := serve(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) })
	if _, err := c.GetSettled(context.Background(), "abc"); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}
