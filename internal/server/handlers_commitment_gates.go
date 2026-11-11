package server

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

type commitmentGateCreateBody struct {
	Blocker *string `json:"blocker"`
}

func (s *Server) handleCreateCommitmentGate(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	item := s.resolveCommitmentGraphItem(w, r, ws, chi.URLParam(r, "itemSlug"))
	if item == nil {
		return
	}
	body, err := strictScheduleBody[commitmentGateCreateBody](r)
	if err != nil {
		writeScheduleBodyError(w)
		return
	}
	if s.writeGraphError(w, r, graphRequired("blocker", body.Blocker)) {
		return
	}
	blocker := s.resolveCommitmentGraphItem(w, r, ws, *body.Blocker)
	if blocker == nil {
		return
	}
	loader := s.graphAccessLoader(r, ws, commitmentAccessLive, item.ID, blocker.ID)
	access, err := loader(s.store.Q())
	if s.writeGraphError(w, r, err) {
		return
	}
	gate, err := s.store.CreateScheduleGate(ws, item.ID, blocker.ID, access, loader)
	if s.writeGraphError(w, r, err) {
		return
	}
	writeJSON(w, 201, gate)
}
func (s *Server) handleDeleteCommitmentGate(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	id := chi.URLParam(r, "gateID")
	a, b, ok := s.graphResourceEnds(w, r, ws, id, true)
	if !ok {
		return
	}
	loader := s.graphAccessLoader(r, ws, commitmentAccessRemove, a, b)
	access, err := loader(s.store.Q())
	if s.writeGraphError(w, r, err) {
		return
	}
	if s.writeGraphError(w, r, s.store.DeleteScheduleGate(ws, id, access, loader)) {
		return
	}
	w.WriteHeader(204)
}
func (s *Server) handleGetCommitmentGates(w http.ResponseWriter, r *http.Request) {
	ws, ok := s.getWorkspaceID(w, r)
	if !ok {
		return
	}
	item := s.resolveCommitmentGraphItem(w, r, ws, chi.URLParam(r, "itemSlug"))
	if item == nil {
		return
	}
	access, err := s.buildCommitmentAccess(r, s.store.Q(), ws, item.ID, commitmentAccessLive)
	if s.writeGraphError(w, r, err) {
		return
	}
	view, err := s.store.ScheduleGates(ws, item.ID, access)
	if s.writeGraphError(w, r, err) {
		return
	}
	writeJSON(w, 200, view)
}
