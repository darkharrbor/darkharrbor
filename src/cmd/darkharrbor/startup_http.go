package main

import (
	"net/http"
	"sync/atomic"
)

type startupHandlerState struct {
	handler http.Handler
}

type startupSwitchHandler struct {
	current atomic.Pointer[startupHandlerState]
}

func newStartupSwitchHandler() *startupSwitchHandler {
	h := &startupSwitchHandler{}
	h.current.Store(&startupHandlerState{handler: http.HandlerFunc(startupUnavailable)})
	return h
}

func (h *startupSwitchHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.current.Load().handler.ServeHTTP(w, r)
}

func (h *startupSwitchHandler) Ready(handler http.Handler) {
	h.current.Store(&startupHandlerState{handler: handler})
}

func startupUnavailable(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Retry-After", "5")
	http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
}
