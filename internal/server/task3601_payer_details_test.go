package server

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// TASK-3601: a plan-limit refusal says who can lift it. The payer sees
// payer_is_you=true (the client offers checkout); anyone else sees
// payer_is_you=false and the payer's name ("Ask Dana to upgrade").
func TestTask3601_PlanLimitDetailsNameThePayer(t *testing.T) {
	res := &store.LimitResult{Feature: "members_per_workspace", Limit: 3, Current: 3, Plan: "free", PayerID: "owner-1", PayerName: "Dana"}
	as := func(userID string) map[string]interface{} {
		r := httptest.NewRequest("POST", "/api/v1/workspaces/x/members/invite", nil)
		if userID != "" {
			r = r.WithContext(context.WithValue(r.Context(), ctxCurrentUser, &models.User{ID: userID}))
		}
		return planLimitDetails(r, res)
	}

	owner := as("owner-1")
	if owner["payer_is_you"] != true {
		t.Errorf("owner: payer_is_you = %v, want true", owner["payer_is_you"])
	}
	if _, ok := owner["payer_name"]; ok {
		t.Errorf("owner: payer_name present (%v); the payer is not told their own name", owner["payer_name"])
	}

	member := as("member-2")
	if member["payer_is_you"] != false {
		t.Errorf("member: payer_is_you = %v, want false", member["payer_is_you"])
	}
	if member["payer_name"] != "Dana" {
		t.Errorf("member: payer_name = %v, want Dana", member["payer_name"])
	}

	// No payer resolved: neither key, so older shapes stay byte-identical.
	bare := planLimitDetails(httptest.NewRequest("GET", "/", nil), &store.LimitResult{Feature: "workspaces", Limit: 1, Current: 1, Plan: "free"})
	if _, ok := bare["payer_is_you"]; ok {
		t.Errorf("no payer: payer_is_you present")
	}
}
