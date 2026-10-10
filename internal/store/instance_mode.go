package store

import (
	"database/sql"
	"errors"
	"fmt"
)

// TASK-3551: which kind of instance owns this database.
//
// A non-cloud boot converts every free user to `self-hosted` (unlimited;
// BackfillSelfHostedPlans). Run once against a Pad Cloud database (a missing
// PAD_MODE, a restore tested locally, a debug boot), that conversion used to
// be permanent: a later cloud boot only fills `plan = ”`. A Cloud database now
// records that it is one, and the self-hosted conversion refuses it.
const settingInstanceMode = "instance_mode"

// Values of settingInstanceMode.
const (
	// InstanceModeCloud is written by every cloud-server boot. Sticky: no
	// non-cloud boot clears it.
	InstanceModeCloud = "cloud"
	// InstanceModeReleased is written only by `pad db release-cloud`: an
	// operator deliberately taking a Cloud database to self-host. It lets the
	// self-hosted conversion run even where the Stripe fingerprint remains. A
	// later cloud boot writes InstanceModeCloud again.
	InstanceModeReleased = "released"
)

// ErrCloudOwnedDatabase is returned by BackfillSelfHostedPlans when the
// database belongs to a Pad Cloud instance. Nothing was written.
var ErrCloudOwnedDatabase = errors.New("this database belongs to a Pad Cloud instance")

// ErrCloudFingerprintPresent is returned by ReleaseCloudOwnership without
// force while users still carry Stripe billing state.
var ErrCloudFingerprintPresent = errors.New("users in this database carry Pad Cloud billing state")

// CloudOwnership says whether the database belongs to a Pad Cloud instance,
// and why.
type CloudOwnership struct {
	Owned bool
	// Marker is the stored instance_mode ("" when none).
	Marker string
	// StripeUsers counts users carrying the Stripe fingerprint: a stripe
	// plan_source or a stored stripe_customer_id. Only the Pad Cloud billing
	// sidecar writes either, through admin routes that exist only in cloud
	// mode, so a self-hosted database has none.
	StripeUsers int64
}

// Reason renders why the database counts as cloud-owned, for a log line.
func (o CloudOwnership) Reason() string {
	switch {
	case !o.Owned:
		return ""
	case o.Marker == InstanceModeCloud:
		return "instance_mode is cloud (written by a cloud-mode boot)"
	default:
		return fmt.Sprintf("%d user(s) carry Pad Cloud billing state (a Stripe plan source or customer id)", o.StripeUsers)
	}
}

// MarkCloudOwned records that a cloud-mode server owns this database. Every
// cloud boot calls it before its own plan backfill. Idempotent.
func (s *Store) MarkCloudOwned() error {
	if err := s.SetPlatformSetting(settingInstanceMode, InstanceModeCloud); err != nil {
		return fmt.Errorf("mark database cloud-owned: %w", err)
	}
	return nil
}

// GetCloudOwnership reads the ownership marker and the Stripe fingerprint.
func (s *Store) GetCloudOwnership() (CloudOwnership, error) {
	return s.cloudOwnershipQ(s.db)
}

func (s *Store) cloudOwnershipQ(q Queryer) (CloudOwnership, error) {
	var o CloudOwnership
	marker, err := s.GetPlatformSettingQ(q, settingInstanceMode)
	if err != nil {
		return o, fmt.Errorf("read instance mode: %w", err)
	}
	o.Marker = marker
	if err := q.QueryRow(s.q(`
		SELECT COUNT(*) FROM users
		WHERE plan_source = ? OR COALESCE(stripe_customer_id, '') <> ''`), PlanSourceStripe).Scan(&o.StripeUsers); err != nil {
		return o, fmt.Errorf("count stripe users: %w", err)
	}
	switch marker {
	case InstanceModeCloud:
		o.Owned = true
	case InstanceModeReleased:
		o.Owned = false
	default:
		// No marker: a Cloud database that predates the marker is recognised
		// by its billing state until its next cloud boot writes the marker.
		o.Owned = o.StripeUsers > 0
	}
	return o, nil
}

// BackfillSelfHostedPlans is the non-cloud boot's plan conversion: free and
// empty plans become `self-hosted` (unlimited). On a cloud-owned database it
// writes NOTHING and returns ErrCloudOwnedDatabase with the ownership, so
// those users keep free-plan limits, the safe direction, instead of being
// made unlimited for good. The read and the write share one transaction.
// Returns the number of users converted.
func (s *Store) BackfillSelfHostedPlans() (int64, CloudOwnership, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, CloudOwnership{}, err
	}
	defer func() { _ = tx.Rollback() }()

	o, err := s.cloudOwnershipQ(tx)
	if err != nil {
		return 0, o, err
	}
	if o.Owned {
		return 0, o, ErrCloudOwnedDatabase
	}
	n, err := s.backfillSelfHostedQ(tx)
	if err != nil {
		return 0, o, fmt.Errorf("backfill self-hosted plans: %w", err)
	}
	return n, o, tx.Commit()
}

// backfillSelfHostedQ is the conversion itself, with no ownership check.
// Taking plan_source over as manual is deliberate (PLAN-3291 DR-6: the write
// that sets a plan owns it).
func (s *Store) backfillSelfHostedQ(ex interface {
	Exec(query string, args ...any) (sql.Result, error)
}) (int64, error) {
	res, err := ex.Exec(s.q(`UPDATE users SET plan = ?, plan_source = ?, updated_at = ? WHERE plan IN ('', 'free')`),
		"self-hosted", PlanSourceManual, now())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ReleaseCloudOwnership records that an operator is deliberately taking this
// database off Pad Cloud, so the next non-cloud boot converts free users to
// self-hosted. Without force it refuses while users carry Stripe billing
// state (ErrCloudFingerprintPresent): those are Pad Cloud customers. A later
// cloud boot marks the database cloud-owned again.
func (s *Store) ReleaseCloudOwnership(force bool) (CloudOwnership, error) {
	o, err := s.GetCloudOwnership()
	if err != nil {
		return o, err
	}
	if o.StripeUsers > 0 && !force {
		return o, ErrCloudFingerprintPresent
	}
	if err := s.SetPlatformSetting(settingInstanceMode, InstanceModeReleased); err != nil {
		return o, fmt.Errorf("release cloud ownership: %w", err)
	}
	return o, nil
}
