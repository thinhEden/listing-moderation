package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"

	"listing-moderation/internal/listing"
	"listing-moderation/internal/seed"
	"listing-moderation/internal/store"
)

const (
	// maxBodyBytes bounds a POST body. The largest valid listing is a few tens
	// of KB; the rest of the allowance is for ignored unknown fields.
	maxBodyBytes = 1 << 20

	// maxIdempotencyKeyBytes is the longest accepted Idempotency-Key.
	maxIdempotencyKeyBytes = 255

	idempotencyHeader = "Idempotency-Key"

	defaultListLimit = 20
	maxListLimit     = 100
)

// ListingStore is what the listing endpoints need from storage.
type ListingStore interface {
	CreateListing(ctx context.Context, p store.CreateParams) (store.CreateResult, error)
	GetListing(ctx context.Context, id string) (listing.Listing, error)
	ListListings(ctx context.Context, f store.ListFilter) ([]listing.Listing, error)
}

type listingAPI struct {
	log   *slog.Logger
	seed  *seed.Data
	store ListingStore
}

// POST /v1/listings
//
// Order matters and is part of the contract:
//
//  1. validate the Idempotency-Key header and the body            -> 400
//  2. check the seller and category exist in the seed             -> 422
//  3. only then touch storage, where a key may conflict            -> 409
//
// So a request that is invalid never stores anything, never claims its key,
// and gets the same 400/422 whether or not the key was used before.
//
// The listing is only stored as pending; moderation happens in the worker.
// 202 is sent only after the insert has committed.
func (a *listingAPI) create(w http.ResponseWriter, r *http.Request) {
	key, err := idempotencyKey(r.Header)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", "could not read request body (is it larger than 1 MiB?)")
		return
	}
	fields, err := listing.ParseSubmission(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}

	if _, ok := a.seed.Sellers[fields.SellerID]; !ok {
		writeError(w, http.StatusUnprocessableEntity, "unknown_seller", "seller_id does not refer to a known seller")
		return
	}
	if _, ok := a.seed.Categories[fields.CategoryID]; !ok {
		writeError(w, http.StatusUnprocessableEntity, "unknown_category", "category_id does not refer to a known category")
		return
	}

	res, err := a.store.CreateListing(r.Context(), store.CreateParams{
		Fields:         fields,
		Key:            key,
		RequestHash:    listing.RequestHash(fields),
		DupFingerprint: listing.DupFingerprint(fields),
	})
	switch {
	case errors.Is(err, store.ErrKeyConflict):
		writeError(w, http.StatusConflict, "idempotency_key_reused",
			"this Idempotency-Key was already used with a different request body")
		return
	case errors.Is(err, store.ErrBusy):
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "busy",
			"a request with this Idempotency-Key is still in progress; retry shortly")
		return
	case err != nil:
		a.log.Error("create listing failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}

	status := http.StatusOK // replay of an earlier submission
	if res.Created {
		status = http.StatusAccepted
	}
	w.Header().Set("Location", "/v1/listings/"+res.ID)
	writeJSON(w, status, map[string]any{"id": res.ID, "status": res.Status})
}

// idempotencyKey reads the optional Idempotency-Key header. A missing header
// means "no key" (nil). A header that is present must be usable: sent twice,
// empty or longer than 255 bytes is an error rather than silently "no key",
// because a client that sent a key expects retry protection.
func idempotencyKey(h http.Header) (*string, error) {
	values := h.Values(idempotencyHeader)
	switch {
	case len(values) == 0:
		return nil, nil
	case len(values) > 1:
		return nil, errors.New("Idempotency-Key must be sent at most once")
	case values[0] == "":
		return nil, errors.New("Idempotency-Key must not be empty")
	case len(values[0]) > maxIdempotencyKeyBytes:
		return nil, errors.New("Idempotency-Key must be at most 255 bytes")
	}
	return &values[0], nil
}

// GET /v1/listings/{id}
func (a *listingAPI) get(w http.ResponseWriter, r *http.Request) {
	l, err := a.store.GetListing(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "no such listing")
		return
	case err != nil:
		a.log.Error("get listing failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	writeJSON(w, http.StatusOK, l)
}

// GET /v1/listings?seller_id=&status=&limit=
func (a *listingAPI) list(w http.ResponseWriter, r *http.Request) {
	f, err := parseListQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	items, err := a.store.ListListings(r.Context(), f)
	if err != nil {
		a.log.Error("list listings failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
		return
	}
	if items == nil {
		items = []listing.Listing{} // the schema wants an array, never null
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

// parseListQuery is strict: a parameter that is present must be valid, so a
// typo like limit=abc or status=draft is a 400, not silently ignored.
func parseListQuery(r *http.Request) (store.ListFilter, error) {
	q := r.URL.Query()
	f := store.ListFilter{Limit: defaultListLimit}

	for _, name := range []string{"seller_id", "status", "limit"} {
		if len(q[name]) > 1 {
			return f, errors.New(name + " must be given at most once")
		}
	}
	if q.Has("seller_id") {
		v := q.Get("seller_id")
		if v == "" {
			return f, errors.New("seller_id must not be empty")
		}
		f.SellerID = &v
	}
	if q.Has("status") {
		s := listing.Status(q.Get("status"))
		if !s.Valid() {
			return f, errors.New("status must be one of pending, auto_approved, auto_rejected, needs_human, published, rejected, expired")
		}
		f.Status = &s
	}
	if q.Has("limit") {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n < 1 || n > maxListLimit {
			return f, errors.New("limit must be an integer between 1 and 100")
		}
		f.Limit = n
	}
	return f, nil
}
