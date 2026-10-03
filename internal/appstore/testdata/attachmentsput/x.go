// Package attachmentsput reaches the ordinary content-addressed Put, whose
// dedup hit skips the write: rule attachments-put.
package attachmentsput

import (
	"context"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/attachments"
	"github.com/PerpetualSoftware/pad/internal/store"
)

var _ *store.Store

func Bad(fs *attachments.FSStore) error {
	_, err := fs.Put(context.Background(), strings.Repeat("0", 64), "", strings.NewReader("x"))
	return err
}
