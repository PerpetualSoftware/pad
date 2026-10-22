package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/notify"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// Fixtures for the push dispatcher tests (TASK-3939 part B1). Every test runs
// on SQLite and, under `make test-pg`, on Postgres (needsBackends).

// fakePush is the gateway: it records every Send and answers sent for every
// message unless fn says otherwise.
type fakePush struct {
	mu    sync.Mutex
	calls int
	msgs  [][]notify.Message
	fn    func(ctx context.Context, msgs []notify.Message) ([]notify.Result, error)
}

func (p *fakePush) Send(ctx context.Context, msgs []notify.Message) ([]notify.Result, error) {
	p.mu.Lock()
	p.calls++
	p.msgs = append(p.msgs, append([]notify.Message(nil), msgs...))
	fn := p.fn
	p.mu.Unlock()
	if fn != nil {
		return fn(ctx, msgs)
	}
	return allStatus(msgs, notify.StatusSent), nil
}

func (p *fakePush) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// all returns every message sent so far, flattened.
func (p *fakePush) all() []notify.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []notify.Message
	for _, b := range p.msgs {
		out = append(out, b...)
	}
	return out
}

func allStatus(msgs []notify.Message, status string) []notify.Result {
	out := make([]notify.Result, len(msgs))
	for i, m := range msgs {
		out[i] = notify.Result{ID: m.ID, Status: status}
	}
	return out
}

var pdxSeq atomic.Int64

// pdx is a workspace with an owner (the actor), a recipient (bob) who is a
// member, assigned the item and has one live device registered long ago, a
// collection, an item, and a comment on it.
type pdx struct {
	t       *testing.T
	srv     *Server
	fake    *fakePush
	n       int64
	owner   *models.User
	bob     *models.User
	ws      *models.Workspace
	coll    *models.Collection
	item    *models.Item
	comment *models.Comment
	devID   string
	devSess string
}

func newPdx(t *testing.T, srv *Server) *pdx {
	t.Helper()
	f := &pdx{t: t, srv: srv, fake: &fakePush{}, n: pdxSeq.Add(1)}
	srv.SetPushSender(f.fake)
	f.owner = f.user("owner")
	f.bob = f.user("bob")
	ws, err := srv.store.CreateWorkspace(models.WorkspaceCreate{Name: fmt.Sprintf("Push %d", f.n), OwnerID: f.owner.ID})
	if err != nil {
		t.Fatal(err)
	}
	f.ws = ws
	must(t, srv.store.AddWorkspaceMember(ws.ID, f.owner.ID, "owner"))
	must(t, srv.store.AddWorkspaceMember(ws.ID, f.bob.ID, "editor"))
	f.coll, err = srv.store.CreateCollection(ws.ID, models.CollectionCreate{Name: "Tasks", Schema: nfxSchema})
	if err != nil {
		t.Fatal(err)
	}
	f.item = f.newItem("SECRET-ITEM-TITLE", &f.bob.ID)
	f.comment, err = srv.store.CreateComment(ws.ID, f.item.ID, f.owner.ID, models.CommentCreate{Body: "SECRET-COMMENT", Author: "Owner", CreatedBy: "user", Source: "web"})
	if err != nil {
		t.Fatal(err)
	}
	// The device comes last, so nothing above enqueued a row for bob.
	f.devID, f.devSess = f.device(f.bob, "tok-bob-"+fmt.Sprint(f.n), 5*time.Hour)
	f.exec(`DELETE FROM user_notifications`)
	return f
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (f *pdx) user(name string) *models.User {
	f.t.Helper()
	u, err := f.srv.store.CreateUser(models.UserCreate{
		Email: fmt.Sprintf("%s%d@push.test", name, f.n), Name: name, Username: fmt.Sprintf("%s-%d", name, f.n), Password: "pw-test-12345",
	})
	must(f.t, err)
	return u
}

func (f *pdx) newItem(title string, assignee *string) *models.Item {
	f.t.Helper()
	it, err := f.srv.store.CreateItem(f.ws.ID, f.coll.ID, models.ItemCreate{Title: title, Fields: `{"status":"open"}`, AssignedUserID: assignee})
	must(f.t, err)
	return it
}

func (f *pdx) exec(q string, args ...any) {
	f.t.Helper()
	if _, err := f.srv.store.DB().Exec(f.srv.store.D().Rebind(q), args...); err != nil {
		f.t.Fatalf("%s: %v", q, err)
	}
}

func (f *pdx) ts(ago time.Duration) string { return time.Now().UTC().Add(-ago).Format(time.RFC3339) }

func (f *pdx) pg() bool { return f.srv.store.D().Driver() == store.DriverPostgres }

// device registers a device for u on a fresh live session, created ago ago.
func (f *pdx) device(u *models.User, token string, ago time.Duration) (deviceID, sessionID string) {
	f.t.Helper()
	plain, err := f.srv.store.CreateSession(u.ID, "android", "", "", time.Hour)
	must(f.t, err)
	h := sha256.Sum256([]byte(plain))
	row := f.srv.store.DB().QueryRow(f.srv.store.D().Rebind(`SELECT id FROM sessions WHERE token_hash = ?`), hex.EncodeToString(h[:]))
	must(f.t, row.Scan(&sessionID))
	deviceID = fmt.Sprintf("dev-%d-%d", f.n, pdxSeq.Add(1))
	ts := f.ts(ago)
	regSeq := `(SELECT COALESCE(MAX(reg_seq), 0) + 1 FROM push_devices)`
	if f.srv.store.D().Driver() == store.DriverPostgres {
		regSeq = `nextval('push_devices_reg_seq')`
	}
	f.exec(`INSERT INTO push_devices (id, user_id, session_id, connection_id, platform, token, app_version, created_at, updated_at, reg_seq)
		VALUES (?, ?, ?, ?, 'android', ?, '1', ?, ?, `+regSeq+`)`, deviceID, u.ID, sessionID, "conn-"+deviceID, token, ts, ts)
	return
}

type rowOpt struct {
	user    *models.User
	item    *models.Item
	comment string // "" = none; "default" = f.comment
	age     time.Duration
	agent   bool
}

// row seeds a pending queue row for bob (or opts.user) about the fixture item.
func (f *pdx) row(kind string, opts ...func(*rowOpt)) string {
	f.t.Helper()
	o := rowOpt{user: f.bob, item: f.item, age: 2 * time.Minute}
	if kind == notify.KindComment || kind == notify.KindReply || kind == notify.KindMention {
		o.comment = "default"
	}
	for _, fn := range opts {
		fn(&o)
	}
	cid := ""
	switch o.comment {
	case "":
	case "default":
		cid = f.comment.ID
	default:
		cid = o.comment
	}
	id := fmt.Sprintf("n-%d-%d", f.n, pdxSeq.Add(1))
	f.exec(`INSERT INTO user_notifications (id, user_id, workspace_id, item_id, comment_id, kind, actor_user_id, dedupe_key, created_at)
		VALUES (?, ?, ?, ?, NULLIF(?, ''), ?, ?, ?, ?)`, id, o.user.ID, f.ws.ID, o.item.ID, cid, kind, f.owner.ID, "k-"+id, f.ts(o.age))
	if o.agent {
		lit := "1"
		if f.pg() {
			lit = "TRUE"
		}
		f.exec(`UPDATE user_notifications SET actor_is_agent = `+lit+` WHERE id = ?`, id)
	}
	return id
}

type rowState struct {
	State, Reason, Claim string
	Attempts             int
	SentAt               string
}

func (f *pdx) state(id string) rowState {
	f.t.Helper()
	var r rowState
	row := f.srv.store.DB().QueryRow(f.srv.store.D().Rebind(
		`SELECT push_state, skip_reason, claim_token, attempts, COALESCE(sent_at, '') FROM user_notifications WHERE id = ?`), id)
	must(f.t, row.Scan(&r.State, &r.Reason, &r.Claim, &r.Attempts, &r.SentAt))
	return r
}

func (f *pdx) tick() { f.srv.runPushDispatchTick(context.Background()) }

func (f *pdx) settings(mut func(*models.NotificationSettings)) {
	f.t.Helper()
	set := models.DefaultNotificationSettings()
	mut(&set)
	must(f.t, f.srv.store.PutNotificationSettings(f.bob.ID, set))
}

// pdClock is the dispatcher's clock (Server.pushDispatch.now) under a test's
// control, so lease and deadline behaviour never depends on wall-clock timing.
type pdClock struct {
	mu    sync.Mutex
	t     time.Time
	start time.Time
}

func (c *pdClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *pdClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// clock installs a fake dispatcher clock reading at.
func (f *pdx) clock(at time.Time) *pdClock {
	c := &pdClock{t: at, start: at}
	f.srv.pushDispatch.mu.Lock()
	f.srv.pushDispatch.now = c.now
	f.srv.pushDispatch.mu.Unlock()
	return c
}
