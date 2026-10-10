package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

// The responses the pad-cloud sidecar DECODES, captured from the real handlers
// as golden shapes. pad-cloud keeps a copy (testdata/pad_contract/) and decodes
// each through its own client, so a key renamed or dropped on one side of the
// repo boundary fails a test instead of shipping. The class this closes:
// user-by-customer answered "user_id" while pad-cloud read "id", so every
// subscription webhook synced user "" for months.
//
// A changed shape is a contract change: review the diff of
// testdata/cloud_contract/, then copy the files to pad-cloud's
// testdata/pad_contract/. Regenerate with
//
//	PAD_UPDATE_CLOUD_CONTRACT=1 go test ./internal/server -run TestCloudContract
//
// Values are replaced by placeholders of their JSON type ("<string>", 0,
// booleans as they are), so the files hold the SHAPE, not one test run's ids.
// The exception is a "code" value: an error code is vocabulary the sidecar
// branches on (stripe_customer_conflict, TASK-3549), so it is the contract
// and is kept verbatim.

const cloudContractSecret = "cloud-contract-secret"

func contractShape(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, vv := range x {
			if code, isString := vv.(string); k == "code" && isString {
				out[k] = code
				continue
			}
			out[k] = contractShape(vv)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, vv := range x {
			out[i] = contractShape(vv)
		}
		return out
	case string:
		if x == "" {
			return ""
		}
		return "<string>"
	case float64:
		return 0
	default:
		return x
	}
}

func TestCloudContract(t *testing.T) {
	srv := testServer(t)
	bootstrapFirstUser(t, srv, "admin@example.com", "Admin")
	srv.SetCloudMode(cloudContractSecret)
	u, err := srv.store.CreateUser(models.UserCreate{Email: "buyer@example.com", Name: "Buyer", Password: "pw-contract-x"})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.store.SetUserStripeCustomerID(u.ID, "cus_contract"); err != nil {
		t.Fatal(err)
	}
	hdr := map[string]string{"X-Cloud-Secret": cloudContractSecret}
	callCode := func(code int, method, path string, body any) map[string]any {
		t.Helper()
		req := cloudAdminReq(t, method, path, body, hdr)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		if rr.Code != code {
			t.Fatalf("%s %s: %d %s", method, path, rr.Code, rr.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		return out
	}
	call := func(method, path string, body any) map[string]any {
		t.Helper()
		return callCode(http.StatusOK, method, path, body)
	}

	shapes := map[string]map[string]any{
		// Checkout reads the stored customer by user before it writes one, and
		// the write is compare-and-set: the same id answers 200, a different
		// one 409 naming the stored id, which pad-cloud then uses (TASK-3549).
		"admin_user_by_id": call("GET", "/api/v1/admin/user-by-id?user_id="+u.ID, nil),
		"admin_stripe_customer_id": call("POST", "/api/v1/admin/stripe-customer-id", map[string]any{
			"user_id": u.ID, "customer_id": "cus_contract", "cloud_secret": cloudContractSecret,
		}),
		"admin_stripe_customer_id_conflict": callCode(http.StatusConflict, "POST", "/api/v1/admin/stripe-customer-id", map[string]any{
			"user_id": u.ID, "customer_id": "cus_contract_other", "cloud_secret": cloudContractSecret,
		}),
		"admin_user_by_customer": call("GET", "/api/v1/admin/user-by-customer?customer_id=cus_contract", nil),
		"admin_plan": call("POST", "/api/v1/admin/plan", map[string]any{
			"user_id": u.ID, "plan": "pro", "expires_at": "2099-01-01T00:00:00Z", "source": "stripe",
			"revision": 1, "subscription_id": "sub_contract", "cloud_secret": cloudContractSecret,
		}),
		"admin_stripe_event_processed": call("POST", "/api/v1/admin/stripe-event-processed", map[string]any{
			"event_id": "evt_contract", "cloud_secret": cloudContractSecret,
		}),
		"admin_payment_failed": call("POST", "/api/v1/admin/payment-failed", map[string]any{
			"stripe_customer_id": "cus_contract", "amount_display": "$12.00", "next_retry_display": "in 3 days",
			"cloud_secret": cloudContractSecret,
		}),
		"auth_oauth_login": call("POST", "/api/v1/auth/oauth-login", map[string]any{
			"provider": "github", "email": "oauth@example.com", "name": "OAuth", "email_verified": true,
			"subject": "gh-contract", "cloud_secret": cloudContractSecret,
		}),
	}
	processed := shapes["admin_stripe_event_processed"]
	unmarkAt, _ := processed["processed_at"].(string)
	shapes["admin_stripe_event_unmark"] = call("POST", "/api/v1/admin/stripe-event-unmark", map[string]any{
		"event_id": "evt_contract", "processed_at": unmarkAt, "cloud_secret": cloudContractSecret,
	})

	// /auth/me is how pad-cloud validates a browser session. Its checkout
	// reads stripe_customer_id from it, which pad sends only when set.
	token, _ := shapes["auth_oauth_login"]["token"].(string)
	oauthUser, err := srv.store.GetUserByEmail("oauth@example.com")
	if err != nil || oauthUser == nil {
		t.Fatalf("oauth user: %v", err)
	}
	if err := srv.store.SetUserStripeCustomerID(oauthUser.ID, "cus_contract_me"); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/api/v1/auth/me", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName(srv.secureCookies), Value: token})
	req.RemoteAddr = "192.0.2.1:1"
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("auth/me: %d %s", rr.Code, rr.Body.String())
	}
	var me map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &me)
	shapes["auth_me"] = me

	names := make([]string, 0, len(shapes))
	for n := range shapes {
		names = append(names, n)
	}
	sort.Strings(names)
	dir := filepath.Join("testdata", "cloud_contract")
	update := os.Getenv("PAD_UPDATE_CLOUD_CONTRACT") == "1"
	if update {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range names {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if err := enc.Encode(contractShape(shapes[n])); err != nil {
			t.Fatal(err)
		}
		got := buf.Bytes()
		path := filepath.Join(dir, n+".json")
		if update {
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v (regenerate with PAD_UPDATE_CLOUD_CONTRACT=1)", path, err)
		}
		if string(want) != string(got) {
			t.Errorf("%s changed: a response pad-cloud decodes has a new shape. Review it, regenerate, and copy it to pad-cloud's testdata/pad_contract/.\n got:\n%s\nwant:\n%s", path, got, want)
		}
	}
}
