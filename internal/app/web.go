package app

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"time"
)

//go:embed static/*
var assets embed.FS

func WebHandler(pool *Pool, store *Store, tester *LoadTester) http.Handler {
	mux := http.NewServeMux()
	root, _ := fs.Sub(assets, "static")
	mux.Handle("GET /", http.FileServer(http.FS(root)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := store.db.PingContext(ctx); err != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		writeJSON(w, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if !pool.Ready() {
			http.Error(w, "no eligible verified IP is ready", 503)
			return
		}
		writeJSON(w, map[string]string{"status": "ready"})
	})
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		s, err := pool.Snapshot(r.Context())
		if err != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		writeJSON(w, struct {
			Snapshot
			Test TestSnapshot `json:"test"`
		}{s, tester.Snapshot()})
	})
	mux.HandleFunc("GET /api/test", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, tester.Snapshot()) })
	mux.HandleFunc("POST /api/test/start", func(w http.ResponseWriter, r *http.Request) {
		var options TestOptions
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&options); err != nil {
			http.Error(w, "invalid test options", 400)
			return
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			http.Error(w, "expected one JSON object", 400)
			return
		}
		if err := tester.Start(options); err != nil {
			code := http.StatusBadRequest
			if errors.Is(err, errTestActive) {
				code = http.StatusConflict
			}
			http.Error(w, err.Error(), code)
			return
		}
		writeJSON(w, tester.Snapshot())
	})
	mux.HandleFunc("POST /api/test/stop", func(w http.ResponseWriter, r *http.Request) {
		tester.Stop()
		writeJSON(w, tester.Snapshot())
	})
	mux.HandleFunc("GET /api/ips", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		page, size := 1, 50
		var err error
		if q.Has("page") {
			page, err = strconv.Atoi(q.Get("page"))
			if err != nil {
				http.Error(w, "invalid page", 400)
				return
			}
		}
		if q.Has("page_size") {
			size, err = strconv.Atoi(q.Get("page_size"))
			if err != nil {
				http.Error(w, "invalid page_size", 400)
				return
			}
		}
		if page < 1 || page > 100000000 || size < 1 || size > 100 || len(q.Get("search")) > 64 {
			http.Error(w, "invalid pagination or search", 400)
			return
		}
		if v := q.Get("used"); v != "" && v != "true" && v != "false" {
			http.Error(w, "used must be true or false", 400)
			return
		}
		p, err := store.List(r.Context(), q.Get("search"), q.Get("used"), page, size)
		if err != nil {
			http.Error(w, "database unavailable", 503)
			return
		}
		writeJSON(w, p)
	})
	subscribers := make(chan struct{}, 32)
	mux.HandleFunc("GET /api/events", func(w http.ResponseWriter, r *http.Request) {
		select {
		case subscribers <- struct{}{}:
			defer func() { <-subscribers }()
		default:
			http.Error(w, "too many live viewers", 503)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		controller := http.NewResponseController(w)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			snapshot, err := pool.Snapshot(r.Context())
			if err != nil {
				return
			}
			data, err := json.Marshal(struct {
				Snapshot
				Test TestSnapshot `json:"test"`
			}{snapshot, tester.Snapshot()})
			if err != nil {
				return
			}
			_ = controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if _, err := fmt.Fprintf(w, "event: status\ndata: %s\n\n", data); err != nil {
				return
			}
			if err := controller.Flush(); err != nil {
				return
			}
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
			}
		}
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		mux.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(value)
}
