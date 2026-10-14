package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
)

// BUG-3812: a unique_scope field is checked on the pool before the write,
// with nothing held between the check and the write, so two writers carrying
// the same value can both pass and both write. Only invocation_slug had a
// database index behind it.

const bug3812Writers = 8

func bug3812Collection(t *testing.T, srv *Server, slug string) {
	t.Helper()
	// The race needs many requests from one address; the per-IP limiter would
	// answer some of them 429 and hide the result. Read per request
	// (Server.RateLimit), so turning it off here affects this server only.
	srv.rateLimiters.Stop()
	srv.rateLimiters = nil
	if rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections", map[string]any{
		"name": "Codes", "slug": "codes", "prefix": "CODE",
		"schema": `{"fields":[{"key":"code","type":"text","unique_scope":"workspace_collection"}]}`,
	}); rr.Code != http.StatusCreated {
		t.Fatalf("create collection: %d %s", rr.Code, rr.Body.String())
	}
}

// holders counts the live items whose code is value.
func bug3812Holders(t *testing.T, srv *Server, slug, value string) int {
	t.Helper()
	rr := doRequest(srv, "GET", "/api/v1/workspaces/"+slug+"/collections/codes/items?code="+value, nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rr.Code, rr.Body.String())
	}
	var items []map[string]any
	parseJSON(t, rr, &items)
	return len(items)
}

// race runs fn on bug3812Writers goroutines released together, and answers
// their status codes.
func bug3812Race(fn func(i int) int) []int {
	codes := make([]int, bug3812Writers)
	var ready, done sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < bug3812Writers; i++ {
		ready.Add(1)
		done.Add(1)
		go func(i int) {
			defer done.Done()
			ready.Done()
			<-start
			codes[i] = fn(i)
		}(i)
	}
	ready.Wait()
	close(start)
	done.Wait()
	return codes
}

func bug3812Count(codes []int, want int) int {
	n := 0
	for _, c := range codes {
		if c == want {
			n++
		}
	}
	return n
}

func TestBUG3812_ConcurrentCreatesHoldOneValueOnce(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		slug := createWSWithCollections(t, srv)
		bug3812Collection(t, srv, slug)
		for round := 0; round < 10; round++ {
			value := fmt.Sprintf("c%d", round)
			codes := bug3812Race(func(i int) int {
				return doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections/codes/items",
					map[string]any{"title": fmt.Sprintf("r%d-w%d", round, i), "fields": `{"code":"` + value + `"}`}).Code
			})
			if got := bug3812Holders(t, srv, slug, value); got != 1 || bug3812Count(codes, http.StatusCreated) != 1 {
				t.Fatalf("round %d: %d items hold %q; codes %v. Want exactly one 201 and the rest 409", round, got, value, codes)
			}
			if bug3812Count(codes, http.StatusConflict) != bug3812Writers-1 {
				t.Fatalf("round %d: codes %v; want the losers refused 409", round, codes)
			}
		}
	})
}

// Both PATCH shapes: a full `fields` replace and a `fields_patch` merge.
func TestBUG3812_ConcurrentPatchesHoldOneValueOnce(t *testing.T) {
	for _, shape := range []string{"fields", "fields_patch"} {
		t.Run(shape, func(t *testing.T) {
			bothBackends(t, func(t *testing.T, srv *Server) {
				slug := createWSWithCollections(t, srv)
				bug3812Collection(t, srv, slug)
				var refs []string
				for i := 0; i < bug3812Writers; i++ {
					// By ref: an item path that looks like a ref is read as one,
					// so a slug like item-7 can name CODE-7 rather than its own item.
					refs = append(refs, createItem(t, srv, slug, "codes", map[string]any{"title": fmt.Sprintf("item %d", i)}).Ref)
				}
				for round := 0; round < 10; round++ {
					value := fmt.Sprintf("p%d", round)
					var body map[string]any
					if shape == "fields" {
						body = map[string]any{"fields": `{"code":"` + value + `"}`}
					} else {
						body = map[string]any{"fields_patch": map[string]any{"code": value}}
					}
					codes := bug3812Race(func(i int) int {
						return doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+refs[i], body).Code
					})
					if got := bug3812Holders(t, srv, slug, value); got != 1 {
						t.Fatalf("round %d: %d items hold %q; codes %v. Want exactly one", round, got, value, codes)
					}
					if bug3812Count(codes, http.StatusOK) != 1 || bug3812Count(codes, http.StatusConflict) != bug3812Writers-1 {
						t.Fatalf("round %d: codes %v; want one 200 and the rest 409", round, codes)
					}
					// Release the value so the next round starts with no holder.
					for i := range refs {
						doRequest(srv, "PATCH", "/api/v1/workspaces/"+slug+"/items/"+refs[i], map[string]any{"fields_patch": map[string]any{"code": nil}})
					}
				}
			})
		})
	}
}

// Bulk move carries each item's value into the target. The carried-collision
// check ran on the pool before the move's lock, so concurrent moves of items
// carrying one value could all land it.
func TestBUG3812_ConcurrentBulkMovesHoldOneValueOnce(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		slug := createWSWithCollections(t, srv)
		bug3812Collection(t, srv, slug)
		if rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/collections", map[string]any{
			"name": "Drafts", "slug": "drafts", "prefix": "DRAFT",
			"schema": `{"fields":[{"key":"code","type":"text"}]}`,
		}); rr.Code != http.StatusCreated {
			t.Fatalf("create drafts: %d %s", rr.Code, rr.Body.String())
		}
		for round := 0; round < 5; round++ {
			value := fmt.Sprintf("m%d", round)
			var ids []string
			for i := 0; i < bug3812Writers; i++ {
				ids = append(ids, createItem(t, srv, slug, "drafts", map[string]any{
					"title": fmt.Sprintf("r%d-d%d", round, i), "fields": `{"code":"` + value + `"}`}).ID)
			}
			// Each row's outcome: "conflict" (refused under the lock),
			// "dropped" (moved with the carried value dropped and reported in
			// not_unique, BUG-2367), "kept" (moved holding the value), or
			// something else, which fails the test.
			outcomes := make([]string, bug3812Writers)
			codes := bug3812Race(func(i int) int {
				rr := doRequest(srv, "POST", "/api/v1/workspaces/"+slug+"/items/bulk",
					map[string]any{"ids": []string{ids[i]}, "op": "move", "collection": "codes"})
				var resp struct {
					Updated []struct {
						Fields    string            `json:"fields"`
						NotUnique []json.RawMessage `json:"not_unique"`
					} `json:"updated"`
					Failed []struct {
						Code string `json:"code"`
					} `json:"failed"`
				}
				if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &resp) != nil {
					outcomes[i] = "bad response"
					return rr.Code
				}
				switch {
				case len(resp.Failed) == 1:
					outcomes[i] = resp.Failed[0].Code
				case len(resp.Updated) == 1 && len(resp.Updated[0].NotUnique) > 0:
					outcomes[i] = "dropped"
				case len(resp.Updated) == 1:
					outcomes[i] = "kept"
				default:
					outcomes[i] = "unknown"
				}
				return rr.Code
			})
			if got := bug3812Holders(t, srv, slug, value); got != 1 {
				t.Fatalf("round %d: %d items in codes hold %q; codes %v. Want exactly one", round, got, value, codes)
			}
			kept, losers := 0, 0
			for _, o := range outcomes {
				switch o {
				case "kept":
					kept++
				case "conflict", "dropped":
					losers++
				}
			}
			if kept != 1 || losers != bug3812Writers-1 {
				t.Fatalf("round %d: outcomes %v; want one kept and the rest conflict or dropped", round, outcomes)
			}
		}
	})
}
