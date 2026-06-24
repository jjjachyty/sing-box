package clashapi

import (
	"net/http"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing/service"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/render"
)

// UserUpdateRequest sets the full user list for a given inbound tag.
type UserUpdateRequest struct {
	Tag   string               `json:"tag"`
	Users []adapter.UserEntry `json:"users"`
}

// UserUpdateResponse is the result of updating users.
type UserUpdateResponse struct {
	Tag     string `json:"tag"`
	Count   int    `json:"count"`
	Status  string `json:"status"`
}

// UserCloseRequest closes all active connections for a specific user.
type UserCloseRequest struct {
	Tag  string `json:"tag"`
	User string `json:"user"`
}

func usersRouter(s *Server) http.Handler {
	r := chi.NewRouter()
	r.Post("/", updateUsers(s))
	r.Post("/close", closeUserConnections(s))
	return r
}

func updateUsers(s *Server) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		var req UserUpdateRequest
		if err := render.DecodeJSON(r.Body, &req); err != nil {
			render.Status(r, http.StatusBadRequest)
			render.JSON(w, r, render.M{"error": "invalid request body"})
			return
		}
		if req.Tag == "" {
			render.Status(r, http.StatusBadRequest)
			render.JSON(w, r, render.M{"error": "tag is required"})
			return
		}
		inboundManager := service.FromContext[adapter.InboundManager](s.ctx)
		if inboundManager == nil {
			render.Status(r, http.StatusServiceUnavailable)
			render.JSON(w, r, render.M{"error": "inbound manager not available"})
			return
		}
		inbound, loaded := inboundManager.Get(req.Tag)
		if !loaded {
			render.Status(r, http.StatusNotFound)
			render.JSON(w, r, render.M{"error": "inbound not found"})
			return
		}
		updatable, ok := inbound.(adapter.UserUpdatableInbound)
		if !ok {
			render.Status(r, http.StatusNotImplemented)
			render.JSON(w, r, render.M{"error": "inbound does not support runtime user updates"})
			return
		}
		if err := updatable.UpdateUsers(req.Users); err != nil {
			render.Status(r, http.StatusInternalServerError)
			render.JSON(w, r, render.M{"error": err.Error()})
			return
		}
		render.JSON(w, r, UserUpdateResponse{
			Tag:    req.Tag,
			Count:  len(req.Users),
			Status: "ok",
		})
	}
}

func closeUserConnections(s *Server) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		var req UserCloseRequest
		if err := render.DecodeJSON(r.Body, &req); err != nil {
			render.Status(r, http.StatusBadRequest)
			render.JSON(w, r, render.M{"error": "invalid request body"})
			return
		}
		if req.Tag == "" || req.User == "" {
			render.Status(r, http.StatusBadRequest)
			render.JSON(w, r, render.M{"error": "tag and user are required"})
			return
		}
		tracker := service.FromContext[adapter.ConnectionTracker](s.ctx)
		if tracker == nil {
			render.Status(r, http.StatusServiceUnavailable)
			render.JSON(w, r, render.M{"error": "connection tracker not available"})
			return
		}
		closed, err := tracker.CloseConnectionsByUser(req.User)
		if err != nil {
			render.Status(r, http.StatusInternalServerError)
			render.JSON(w, r, render.M{"error": err.Error()})
			return
		}
		render.JSON(w, r, render.M{
			"tag":    req.Tag,
			"user":   req.User,
			"closed": closed,
			"status": "ok",
		})
	}
}
