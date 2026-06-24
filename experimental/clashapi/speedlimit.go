package clashapi

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/render"
)

// SpeedLimitRequest 设置/更新限速请求体
type SpeedLimitRequest struct {
	Name        string `json:"name"`          // 用户标签，如 "5M" / "20M"
	BytesPerSec int64  `json:"bytes_per_sec"` // 字节/秒，0 表示取消限速
}

// SpeedLimitResponse 限速列表响应
type SpeedLimitResponse struct {
	Limits map[string]int64 `json:"limits"`
}

func speedLimitRouter(s *Server) http.Handler {
	r := chi.NewRouter()
	r.Get("/", getSpeedLimits(s))
	r.Post("/", setSpeedLimit(s))
	r.Delete("/{name}", deleteSpeedLimit(s))
	return r
}

func getSpeedLimits(s *Server) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.rateLimiter == nil {
			render.JSON(w, r, SpeedLimitResponse{Limits: map[string]int64{}})
			return
		}
		render.JSON(w, r, SpeedLimitResponse{Limits: s.rateLimiter.GetLimits()})
	}
}

func setSpeedLimit(s *Server) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.rateLimiter == nil {
			render.Status(r, http.StatusServiceUnavailable)
			render.JSON(w, r, render.M{"error": "rate limiter not available"})
			return
		}
		var req SpeedLimitRequest
		if err := render.DecodeJSON(r.Body, &req); err != nil {
			render.Status(r, http.StatusBadRequest)
			render.JSON(w, r, render.M{"error": "invalid request body"})
			return
		}
		if req.Name == "" {
			render.Status(r, http.StatusBadRequest)
			render.JSON(w, r, render.M{"error": "name is required"})
			return
		}
		s.rateLimiter.SetLimit(req.Name, req.BytesPerSec)
		render.JSON(w, r, render.M{
			"name":          req.Name,
			"bytes_per_sec": req.BytesPerSec,
			"status":        "ok",
		})
	}
}

func deleteSpeedLimit(s *Server) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.rateLimiter == nil {
			render.Status(r, http.StatusServiceUnavailable)
			render.JSON(w, r, render.M{"error": "rate limiter not available"})
			return
		}
		name := chi.URLParam(r, "name")
		name, _ = strconv.Unquote(strings.TrimSpace(name))
		if name == "" {
			render.Status(r, http.StatusBadRequest)
			render.JSON(w, r, render.M{"error": "name is required"})
			return
		}
		s.rateLimiter.SetLimit(name, 0)
		render.JSON(w, r, render.M{"name": name, "status": "deleted"})
	}
}
