package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/PerpetualSoftware/pad/internal/models"
)

const copyAttachmentLimitMessage = "This item references more than 1,000 attachments, which is too many to copy. Remove some references, then copy it again."

func TestCopyPreflightAttachmentLimit(t *testing.T) {
	for _, n := range []int{1000, 1001} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			f := newCopyPreflightFixture(t)
			var content strings.Builder
			tx, err := f.srv.store.DB().Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			for i := 0; i < n; i++ {
				a := &models.Attachment{WorkspaceID: f.wsA.ID, UploadedBy: f.owner.ID, ItemID: &f.source.ID, StorageKey: "fs:fixture", ContentHash: "fixture", MimeType: "image/png", SizeBytes: 1, Filename: "file.png"}
				if err := f.srv.store.CreateAttachmentTx(tx, a); err != nil {
					t.Fatal(err)
				}
				fmt.Fprintf(&content, "![file](pad-attachment:%s)\n", a.ID)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			body := content.String() + "[[Other]]"
			if _, err := f.srv.store.UpdateItem(f.source.ID, models.ItemUpdate{Content: &body}); err != nil {
				t.Fatal(err)
			}
			before := f.snapshot()
			versions, links := f.countRows("SELECT COUNT(*) FROM item_versions"), f.countRows("SELECT COUNT(*) FROM item_wiki_links")
			request := f.resolvableBody()
			pre := f.call(f.owner, reqOpts{}, request)
			if n == 1000 {
				if pre.Code != http.StatusOK {
					t.Fatalf("preflight = %d: %s", pre.Code, pre.Body.String())
				}
				var result ItemCopyPreflight
				if err := json.Unmarshal(pre.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if !result.Valid || result.Warnings.AttachmentCount != 1000 {
					t.Fatalf("preflight valid=%t count=%d", result.Valid, result.Warnings.AttachmentCount)
				}
				return
			}
			for _, archive := range []bool{false, true} {
				request["archive_source"] = archive
				copy := f.callCopy(f.owner, reqOpts{}, request)
				for name, rr := range map[string]struct {
					status int
					body   string
				}{"preflight": {pre.Code, pre.Body.String()}, "copy": {copy.Code, copy.Body.String()}} {
					if rr.status != http.StatusUnprocessableEntity {
						t.Errorf("%s status = %d, want 422", name, rr.status)
					}
					var env struct {
						Error struct{ Code, Message string }
					}
					if err := json.Unmarshal([]byte(rr.body), &env); err != nil {
						t.Fatal(err)
					}
					if env.Error.Code != "too_many_attachments" || env.Error.Message != copyAttachmentLimitMessage {
						t.Errorf("%s refusal = %+v", name, env.Error)
					}
				}
				if pre.Body.String() != copy.Body.String() {
					t.Error("preflight and copy refusals differ")
				}
				if after := f.snapshot(); !reflect.DeepEqual(after, before) {
					t.Errorf("refusal wrote rows: before %+v, after %+v", before, after)
				}
				if f.countRows("SELECT COUNT(*) FROM item_versions") != versions || f.countRows("SELECT COUNT(*) FROM item_wiki_links") != links {
					t.Error("refusal created version or wiki-link rows")
				}
				if source, err := f.srv.store.GetItem(f.source.ID); err != nil || source == nil {
					t.Fatalf("source archived on refusal: %v", err)
				}
			}
		})
	}
}
