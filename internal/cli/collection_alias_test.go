package cli

import (
	"errors"
	"fmt"
	"testing"
)

// notFoundCollErr is the create/list wire shape for a missing collection.
func notFoundCollErr() error {
	return &APIError{Code: "not_found", Message: "Collection not found"}
}

// invalidCollErr is the move wire shape for a missing target collection.
func invalidCollErr() error {
	return &APIError{Code: "invalid_collection", Message: "Target collection not found"}
}

func TestWithCollectionAliasFallback_RawSucceeds_NoRetry(t *testing.T) {
	// The shadow case (BUG-2630): the user typed a slug that IS a real
	// collection ("plan") whose alias ("plans") also exists. The raw attempt
	// succeeds, so the fallback must NOT retry — retrying would route the user
	// to the aliased collection they did not name.
	var calls []string
	got, err := WithCollectionAliasFallback("plan", func(slug string) (string, error) {
		calls = append(calls, slug)
		return "landed:" + slug, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "landed:plan" {
		t.Fatalf("got %q, want landed:plan", got)
	}
	if len(calls) != 1 || calls[0] != "plan" {
		t.Fatalf("expected exactly one call with the raw slug, got %v", calls)
	}
}

func TestWithCollectionAliasFallback_CollectionNotFound_RetriesAlias(t *testing.T) {
	// Old-server compat: raw "task" 404s because the collection is "tasks" and
	// the server has no resolver. The fallback retries with the alias.
	var calls []string
	got, err := WithCollectionAliasFallback("task", func(slug string) (string, error) {
		calls = append(calls, slug)
		if slug == "task" {
			return "", notFoundCollErr()
		}
		return "landed:" + slug, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "landed:tasks" {
		t.Fatalf("got %q, want landed:tasks", got)
	}
	if len(calls) != 2 || calls[0] != "task" || calls[1] != "tasks" {
		t.Fatalf("expected raw then alias, got %v", calls)
	}
}

func TestWithCollectionAliasFallback_InvalidCollectionCode_RetriesAlias(t *testing.T) {
	// The move path reports a missing target collection as invalid_collection.
	var calls []string
	_, err := WithCollectionAliasFallback("task", func(slug string) (string, error) {
		calls = append(calls, slug)
		if slug == "task" {
			return "", invalidCollErr()
		}
		return "ok", nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("expected a retry on invalid_collection, got %v", calls)
	}
}

func TestWithCollectionAliasFallback_NonAliasedInput_NoRetry(t *testing.T) {
	// A slug that NormalizeSlug leaves unchanged ("widgets") has no alias to
	// try, so a not-found must return immediately without a second call.
	var calls int
	_, err := WithCollectionAliasFallback("widgets", func(slug string) (string, error) {
		calls++
		return "", notFoundCollErr()
	})
	if calls != 1 {
		t.Fatalf("expected exactly one call (no alias to retry), got %d", calls)
	}
	// The error the user sees names the slug they typed (constraint 2), with
	// the collection-not-found Code preserved for downstream inspection.
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "not_found" {
		t.Fatalf("expected a *APIError with Code=not_found, got %v", err)
	}
	if apiErr.Message != `collection "widgets" not found` {
		t.Fatalf("error should name the raw slug, got %q", apiErr.Message)
	}
}

func TestWithCollectionAliasFallback_NonCollectionError_NoRetry(t *testing.T) {
	// Constraint 1 (lead): a request aimed at a collection that really exists
	// ("plan") but that fails for a DIFFERENT reason (here a validation error)
	// must NOT be retried against the alias ("plans"). Retrying could silently
	// succeed against the wrong collection — BUG-2630 in a new costume.
	var calls []string
	sentinel := &APIError{Code: "bad_request", Message: "Title is required"}
	_, err := WithCollectionAliasFallback("plan", func(slug string) (string, error) {
		calls = append(calls, slug)
		return "", sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected the raw sentinel error back, got %v", err)
	}
	if len(calls) != 1 || calls[0] != "plan" {
		t.Fatalf("a non-collection error must not trigger the alias retry, got %v", calls)
	}
}

func TestWithCollectionAliasFallback_BothFail_SurfacesRawError(t *testing.T) {
	// Constraint 2 (lead): when both raw and alias fail, the error the user
	// sees must name the slug they typed ("task"), not the alias tried on their
	// behalf ("tasks").
	got := "sentinel-unset"
	_, err := WithCollectionAliasFallback("task", func(slug string) (string, error) {
		got = slug
		if slug == "task" {
			return "", notFoundCollErr()
		}
		return "", &APIError{Code: "not_found", Message: "Collection not found"}
	})
	if got != "tasks" {
		t.Fatalf("expected the alias to have been tried, last slug was %q", got)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected a *APIError, got %v", err)
	}
	if apiErr.Message != `collection "task" not found` {
		t.Fatalf("double-fail error must name the RAW slug, got %q", apiErr.Message)
	}
}

func TestIsCollectionNotFound(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"create/list not_found", notFoundCollErr(), true},
		{"move invalid_collection", invalidCollErr(), true},
		{"item not_found (different message)", &APIError{Code: "not_found", Message: "item TASK-9 not found"}, false},
		{"validation error", &APIError{Code: "bad_request", Message: "Title is required"}, false},
		{"plan limit", &APIError{Code: "plan_limit_exceeded", Message: "limit"}, false},
		{"wrapped not_found", fmt.Errorf("ctx: %w", notFoundCollErr()), true},
		{"non-APIError", errors.New("network down"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCollectionNotFound(tc.err); got != tc.want {
				t.Fatalf("isCollectionNotFound(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
