package clashapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/render"
)

// UserTrafficResponse is the per-user traffic snapshot since process start.
type UserTrafficResponse struct {
	// Users maps user name (uuid) to [upload, download] bytes.
	Users map[string][2]int64 `json:"users"`
	// Online is the number of distinct users with active connections.
	Online int `json:"online"`
}

func trafficRouter(s *Server) http.Handler {
	r := chi.NewRouter()
	r.Get("/users", getUserTraffic(s))
	return r
}

func getUserTraffic(s *Server) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		render.JSON(w, r, UserTrafficResponse{
			Users:  s.trafficManager.UserTraffic(),
			Online: s.trafficManager.OnlineUsers(),
		})
	}
}
