package server

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/burkostya/regate/internal/storage"
)

var errEffectHistoryUnavailable = errors.New("effect history unavailable")

func (s *Service) getEffectHistory(ctx context.Context, id string) (storage.EffectHistory, error) {
	history, err := s.Store.EffectHistory(ctx, id)
	if err != nil && !errors.Is(err, storage.ErrInvalidEffectHistoryID) && !errors.Is(err, storage.ErrEffectHistoryNotFound) {
		return storage.EffectHistory{}, errEffectHistoryUnavailable
	}
	return history, err
}

func (s *Service) effectHistory(w http.ResponseWriter, r *http.Request) {
	if len(r.URL.RawQuery) > 512 {
		errorJSON(w, http.StatusBadRequest, storage.ErrInvalidEffectHistoryID)
		return
	}
	params, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(params) != 1 || len(params["effect_id"]) != 1 {
		errorJSON(w, http.StatusBadRequest, storage.ErrInvalidEffectHistoryID)
		return
	}
	history, err := s.getEffectHistory(r.Context(), params.Get("effect_id"))
	switch {
	case err == nil:
		reply(w, http.StatusOK, history)
	case errors.Is(err, storage.ErrInvalidEffectHistoryID):
		errorJSON(w, http.StatusBadRequest, err)
	case errors.Is(err, storage.ErrEffectHistoryNotFound):
		errorJSON(w, http.StatusNotFound, err)
	default:
		errorJSON(w, http.StatusServiceUnavailable, err)
	}
}
