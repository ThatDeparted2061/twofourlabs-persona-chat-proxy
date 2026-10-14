package handlers

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"

	"github.com/go-chi/chi/v5"

	"github.com/ThatDeparted2061/twofourlabs-persona-chat-proxy/internal/persona"
)

var personaID = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// Personas serves PUT/GET /v1/personas/{id}, behind the same API keys as chat.
type Personas struct {
	Store   *persona.Store
	APIKeys map[string]bool
}

func (h *Personas) authorized(w http.ResponseWriter, r *http.Request) bool {
	if len(h.APIKeys) > 0 && !h.APIKeys[apiKey(r)] {
		writeError(w, http.StatusUnauthorized, "invalid_api_key", "invalid or missing API key")
		return false
	}
	return true
}

func (h *Personas) Put(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(w, r) {
		return
	}
	id := chi.URLParam(r, "id")
	var body struct{ Name, Prompt string }
	if !personaID.MatchString(id) {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "persona id must match "+personaID.String())
		return
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10)).Decode(&body); err != nil || body.Prompt == "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", `body must be {"name": "...", "prompt": "..."}`)
		return
	}
	p, err := h.Store.Put(r.Context(), id, body.Name, body.Prompt)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": p.ID, "name": p.Name, "hash": p.Hash, "prompt_bytes": len(p.Prompt)})
}

func (h *Personas) Get(w http.ResponseWriter, r *http.Request) {
	if !h.authorized(w, r) {
		return
	}
	p, err := h.Store.Get(r.Context(), chi.URLParam(r, "id"))
	switch {
	case errors.Is(err, persona.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, "server_error", err.Error())
	default:
		writeJSON(w, http.StatusOK, p)
	}
}
