package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
)

// webBuildSourceFile is the stamp `vite build` writes into the bundle
// (web/vite.config.ts, buildSourceStamp): the commit the web source was at and
// how many entries `git status` listed under web/ when it ran.
const webBuildSourceFile = "build-source.json"

// webBuildIdentity says which web bundle this binary serves (TASK-3233).
//
// The commit a binary is stamped with says nothing about its web/build: that
// directory is generated, gitignored, and embedded as it stands, so a bundle
// left over from another commit, or built while web/ held uncommitted edits (a
// negative-control build, BUG-3129), ships under the same commit stamp.
// SHA256 is the bundle's own identity, computed from the bytes served, so it
// needs no trust in the build machine. The Source* fields come from the stamp
// and say where the bundle came from; they are absent when it has none.
type webBuildIdentity struct {
	SHA256       string  `json:"sha256"`
	Files        int     `json:"files"`
	SourceCommit *string `json:"source_commit,omitempty"`
	SourceDirty  *int    `json:"source_dirty,omitempty"`
}

type webBuildSource struct {
	Commit *string `json:"commit"`
	Dirty  *int    `json:"dirty"`
}

// identifyWebBuild digests every regular file in fsys: sha256 over, in
// WalkDir's lexical order, each file's path, a NUL, its content's sha256, and
// a newline. Paths are part of it, so a rename changes the digest too. A
// failure is logged and leaves the identity unknown (nil); it never stops the
// server, since serving works without it.
func identifyWebBuild(fsys fs.FS) *webBuildIdentity {
	if fsys == nil {
		return nil
	}
	h := sha256.New()
	files := 0
	err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		f, err := fsys.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		fh := sha256.New()
		if _, err := io.Copy(fh, f); err != nil {
			return err
		}
		io.WriteString(h, path)
		h.Write([]byte{0})
		io.WriteString(h, hex.EncodeToString(fh.Sum(nil)))
		h.Write([]byte{'\n'})
		files++
		return nil
	})
	if err != nil {
		slog.Warn("web build identity: could not digest the embedded web UI", "error", err)
		return nil
	}
	id := &webBuildIdentity{SHA256: hex.EncodeToString(h.Sum(nil)), Files: files}
	if raw, err := fs.ReadFile(fsys, webBuildSourceFile); err == nil {
		var src webBuildSource
		if err := json.Unmarshal(raw, &src); err != nil {
			slog.Warn("web build identity: unreadable "+webBuildSourceFile, "error", err)
		} else {
			id.SourceCommit, id.SourceDirty = src.Commit, src.Dirty
		}
	}
	return id
}
