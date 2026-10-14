package server

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// BUG-3703: a list request's field filters are bounded before any store work;
// at the bound it is still answered.
func TestBUG3703_TooManyFieldFiltersAreRefusedBeforeAnyProbe(t *testing.T) {
	bothBackends(t, func(t *testing.T, srv *Server) {
		slug, _ := task3462Workspace(t, srv, "Cap 3703", "startup")
		path := func(n int) string {
			q := make([]string, n)
			for i := range q {
				q[i] = fmt.Sprintf("k%03d=x", i)
			}
			return "/api/v1/workspaces/" + slug + "/items?" + strings.Join(q, "&")
		}
		rr := doRequest(srv, "GET", path(maxListFilterKeys+1), nil)
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "invalid list filter: 51 field filters, at most 50") {
			t.Fatalf("over the cap: %d %s", rr.Code, rr.Body.String())
		}
		// At the cap the request reaches the key check, which refuses the
		// first key (k000) no schema declares and no item stores.
		rr = doRequest(srv, "GET", path(maxListFilterKeys), nil)
		if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), `invalid list filter \"k000\"`) {
			t.Fatalf("at the cap: %d %s", rr.Code, rr.Body.String())
		}
	})
}
