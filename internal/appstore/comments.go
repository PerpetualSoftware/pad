package appstore

import (
	"context"
	"strings"

	"github.com/PerpetualSoftware/pad/internal/models"
	"github.com/PerpetualSoftware/pad/internal/store"
)

// AppCommentCreate is the only input an app comment create takes.
type AppCommentCreate struct {
	Body     string
	ParentID string
	// Author is the display name recorded on the comment; the server sets it
	// from the actor (U6), never from the app's request.
	Author string
}

// A comment body may carry a pad-attachment: reference, unlike an item's
// content, but only to an attachment already bound to the comment's item
// (DOC-3371 §4). The store checks that under the fence
// (store.ErrAppAttachmentUnavailable); nothing here inspects the reference.

func refuseBlankBody(body string) error {
	if strings.TrimSpace(body) == "" {
		return refuse("body is required")
	}
	return nil
}

// CommentWrite is a committed app comment write with the item it is on and
// the actor's display name, both read in the same transaction: what the
// write publishes describes the item as the write found it.
type CommentWrite struct {
	Comment      *models.Comment
	Item         *models.Item
	ActorDisplay string
}

// CreateComment writes a comment on a companion item, in one FencedTx.
func (a *Store) CreateComment(ctx context.Context, spec store.FenceSpec, itemID string, in AppCommentCreate, actor store.FencedActor) (*models.Comment, error) {
	w, err := a.CreateCommentWrite(ctx, spec, itemID, in, actor)
	if err != nil {
		return nil, err
	}
	return w.Comment, nil
}

// CreateCommentWrite is CreateComment with the item and actor display. An
// empty Author is filled with the actor's display name, read in the
// transaction.
func (a *Store) CreateCommentWrite(ctx context.Context, spec store.FenceSpec, itemID string, in AppCommentCreate, actor store.FencedActor) (*CommentWrite, error) {
	if err := refuseBlankBody(in.Body); err != nil {
		return nil, err
	}
	ftx, err := a.s.BeginFenced(ctx, spec)
	if err != nil {
		return nil, err
	}
	defer func() { _ = ftx.Rollback() }()

	who, err := ftx.ActorDisplay(actor)
	if err != nil {
		return nil, err
	}
	author := in.Author
	if author == "" {
		author = who
	}
	c, err := ftx.CreateComment(store.FencedCommentCreate{
		ItemID: itemID, ParentID: in.ParentID, Body: in.Body, Author: author, Actor: actor,
	})
	if err != nil {
		return nil, err
	}
	item, err := ftx.Item(itemID)
	if err != nil {
		return nil, err
	}
	if err := ftx.Commit(); err != nil {
		return nil, err
	}
	return &CommentWrite{Comment: c, Item: item, ActorDisplay: who}, nil
}

// UpdateComment replaces the body of a comment this install wrote as this
// actor, addressed through its item, in one FencedTx.
func (a *Store) UpdateComment(ctx context.Context, spec store.FenceSpec, itemID, commentID, body string, actor store.FencedActor) (*models.Comment, error) {
	w, err := a.UpdateCommentWrite(ctx, spec, itemID, commentID, body, actor)
	if err != nil {
		return nil, err
	}
	return w.Comment, nil
}

// UpdateCommentWrite is UpdateComment with the item and actor display.
func (a *Store) UpdateCommentWrite(ctx context.Context, spec store.FenceSpec, itemID, commentID, body string, actor store.FencedActor) (*CommentWrite, error) {
	if err := refuseBlankBody(body); err != nil {
		return nil, err
	}
	ftx, err := a.s.BeginFenced(ctx, spec)
	if err != nil {
		return nil, err
	}
	defer func() { _ = ftx.Rollback() }()

	c, err := ftx.UpdateComment(itemID, commentID, body, actor)
	if err != nil {
		return nil, err
	}
	item, err := ftx.Item(itemID)
	if err != nil {
		return nil, err
	}
	who, err := ftx.ActorDisplay(actor)
	if err != nil {
		return nil, err
	}
	if err := ftx.Commit(); err != nil {
		return nil, err
	}
	return &CommentWrite{Comment: c, Item: item, ActorDisplay: who}, nil
}

// DeleteComment deletes (or tombstones) a comment this install wrote as this
// actor, addressed through its item, in one FencedTx.
func (a *Store) DeleteComment(ctx context.Context, spec store.FenceSpec, itemID, commentID string, actor store.FencedActor) error {
	ftx, err := a.s.BeginFenced(ctx, spec)
	if err != nil {
		return err
	}
	defer func() { _ = ftx.Rollback() }()

	if err := ftx.DeleteComment(itemID, commentID, actor); err != nil {
		return err
	}
	return ftx.Commit()
}
