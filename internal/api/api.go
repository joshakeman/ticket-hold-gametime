// Package api is the HTTP layer: routing, validation and error mapping.
package api

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"

	"github.com/google/uuid"
	"github.com/josh/ticket-hold-gametime/internal/store"
)

const (
	maxSeatsPerHold = 8
	maxBodyBytes    = 1 << 20
	maxKeyLen       = 255
)

type API struct {
	store *store.Store
}

func New(s *store.Store) http.Handler {
	a := &API{store: s}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /events/{id}/seats", a.listSeats)
	mux.HandleFunc("POST /events/{id}/holds", a.createHold)
	mux.HandleFunc("GET /holds/{id}", a.getHold)
	mux.HandleFunc("DELETE /holds/{id}", a.releaseHold)
	mux.HandleFunc("POST /holds/{id}/purchase", a.purchase)
	return mux
}

func (a *API) listSeats(w http.ResponseWriter, r *http.Request) {
	eventID, ok := pathInt(w, r, "id")
	if !ok {
		return
	}
	seats, err := a.store.ListSeats(r.Context(), eventID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"seats": seats})
}

func (a *API) createHold(w http.ResponseWriter, r *http.Request) {
	eventID, ok := pathInt(w, r, "id")
	if !ok {
		return
	}
	buyer, ok := buyerID(w, r)
	if !ok {
		return
	}

	var req struct {
		SeatIDs []int64 `json:"seat_ids"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if n := len(req.SeatIDs); n == 0 || n > maxSeatsPerHold {
		writeError(w, http.StatusBadRequest, "seat_ids must contain between 1 and 8 seats")
		return
	}

	hold, err := a.store.CreateHold(r.Context(), eventID, buyer, req.SeatIDs)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, hold)
}

func (a *API) getHold(w http.ResponseWriter, r *http.Request) {
	holdID, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	buyer, ok := buyerID(w, r)
	if !ok {
		return
	}
	hold, err := a.store.GetHold(r.Context(), holdID, buyer)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, hold)
}

func (a *API) releaseHold(w http.ResponseWriter, r *http.Request) {
	holdID, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	buyer, ok := buyerID(w, r)
	if !ok {
		return
	}
	if err := a.store.Release(r.Context(), holdID, buyer); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) purchase(w http.ResponseWriter, r *http.Request) {
	holdID, ok := pathUUID(w, r, "id")
	if !ok {
		return
	}
	buyer, ok := buyerID(w, r)
	if !ok {
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" || len(key) > maxKeyLen {
		writeError(w, http.StatusBadRequest, "Idempotency-Key header is required (max 255 chars)")
		return
	}

	order, replayed, err := a.store.Purchase(r.Context(), holdID, buyer, key)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	status := http.StatusCreated
	if replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, order)
}

func pathInt(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	v, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || v <= 0 {
		writeError(w, http.StatusBadRequest, "invalid "+name)
		return 0, false
	}
	return v, true
}

func pathUUID(w http.ResponseWriter, r *http.Request, name string) (string, bool) {
	id, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid "+name)
		return "", false
	}
	return id.String(), true
}

func buyerID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.Header.Get("X-Buyer-ID")
	if id == "" {
		writeError(w, http.StatusUnauthorized, "X-Buyer-ID header is required")
		return "", false
	}
	return id, true
}

func writeStoreError(w http.ResponseWriter, err error) {
	var unavailable *store.UnavailableError
	var unknown *store.UnknownSeatsError
	switch {
	case errors.As(err, &unavailable):
		writeJSON(w, http.StatusConflict, map[string]any{"error": "some seats are unavailable", "unavailable": unavailable.SeatIDs})
	case errors.As(err, &unknown):
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "some seats do not exist in this event", "unknown": unknown.SeatIDs})
	case errors.Is(err, store.ErrEventNotFound), errors.Is(err, store.ErrHoldNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, store.ErrForbidden):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, store.ErrHoldExpired):
		writeError(w, http.StatusGone, err.Error())
	case errors.Is(err, store.ErrAlreadyPurchased), errors.Is(err, store.ErrSeatsLost), errors.Is(err, store.ErrIdempotencyKeyReuse):
		writeError(w, http.StatusConflict, err.Error())
	default:
		log.Printf("internal error: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write response: %v", err)
	}
}
