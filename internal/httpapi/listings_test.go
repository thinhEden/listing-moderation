package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"listing-moderation/internal/listing"
	"listing-moderation/internal/seed"
	"listing-moderation/internal/store"
)

// fakeStore records calls so tests can prove what did, and did not, reach storage.
type fakeStore struct {
	mu      sync.Mutex
	creates []store.CreateParams
	lists   []store.ListFilter
	gets    []string

	createRes store.CreateResult
	createErr error
	getRes    listing.Listing
	getErr    error
	listRes   []listing.Listing
}

func (f *fakeStore) CreateListing(_ context.Context, p store.CreateParams) (store.CreateResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creates = append(f.creates, p)
	return f.createRes, f.createErr
}

func (f *fakeStore) GetListing(_ context.Context, id string) (listing.Listing, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets = append(f.gets, id)
	return f.getRes, f.getErr
}

func (f *fakeStore) ListListings(_ context.Context, flt store.ListFilter) ([]listing.Listing, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lists = append(f.lists, flt)
	return f.listRes, nil
}

func realSeed(t *testing.T) *seed.Data {
	t.Helper()
	d, err := seed.Load(filepath.Join("..", "..", "seed"))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func newAPI(t *testing.T, fs ListingStore) http.Handler {
	t.Helper()
	return NewHandler(Deps{Log: quiet(), Seed: realSeed(t), Store: fs})
}

const okBody = `{"seller_id":"s-1002","title":"iPhone 13","description":"Like new","price_vnd":6000000,"category_id":"c-phones"}`

func do(t *testing.T, h http.Handler, method, target, body string, headers ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Add(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Error.Code == "" || e.Error.Message == "" {
		t.Fatalf("not an Error body: %q (%v)", rec.Body.String(), err)
	}
	return e.Error.Code
}

// ---- POST: nothing invalid ever reaches storage ----------------------------

func TestPostInvalidRequestsNeverTouchStorage(t *testing.T) {
	long := strings.Repeat("k", 256)
	cases := []struct {
		name    string
		body    string
		headers []string
		status  int
		code    string
	}{
		{"empty body", ``, nil, 400, "invalid_request"},
		{"malformed json", `{`, nil, 400, "invalid_request"},
		{"duplicate key", strings.Replace(okBody, `"title":"iPhone 13"`, `"title":"a","title":"b"`, 1), nil, 400, "invalid_request"},
		{"float price", strings.Replace(okBody, `6000000`, `6000000.0`, 1), nil, 400, "invalid_request"},
		{"trailing data", okBody + `{}`, nil, 400, "invalid_request"},
		{"missing field", `{"seller_id":"s-1002"}`, nil, 400, "invalid_request"},

		{"empty key header", okBody, []string{"Idempotency-Key", ""}, 400, "invalid_request"},
		{"key over 255 bytes", okBody, []string{"Idempotency-Key", long}, 400, "invalid_request"},
		{"key sent twice", okBody, []string{"Idempotency-Key", "a", "Idempotency-Key", "b"}, 400, "invalid_request"},

		{"unknown seller", strings.Replace(okBody, "s-1002", "s-9999", 1), []string{"Idempotency-Key", "k"}, 422, "unknown_seller"},
		{"unknown category", strings.Replace(okBody, "c-phones", "c-boats", 1), []string{"Idempotency-Key", "k"}, 422, "unknown_category"},

		// Validation comes before the existence checks.
		{"400 wins over 422", `{"seller_id":"s-9999","title":"","description":"","price_vnd":1,"category_id":"c-phones"}`, nil, 400, "invalid_request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := &fakeStore{}
			rec := do(t, newAPI(t, fs), http.MethodPost, "/v1/listings", tc.body, tc.headers...)
			if rec.Code != tc.status || errCode(t, rec) != tc.code {
				t.Fatalf("got %d %s, want %d %s", rec.Code, rec.Body.String(), tc.status, tc.code)
			}
			if len(fs.creates) != 0 {
				t.Fatal("an invalid request reached storage, which could claim its idempotency key")
			}
		})
	}
}

func TestPostUnknownSellerReportedBeforeUnknownCategory(t *testing.T) {
	rec := do(t, newAPI(t, &fakeStore{}), http.MethodPost, "/v1/listings",
		`{"seller_id":"nope","title":"t","description":"","price_vnd":1,"category_id":"nope"}`)
	if rec.Code != 422 || errCode(t, rec) != "unknown_seller" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestPostBodyOver1MiBIsRejected(t *testing.T) {
	big := `{"seller_id":"s-1002","title":"t","description":"","price_vnd":1,"category_id":"c-phones","x":"` +
		strings.Repeat("a", 1<<20) + `"}`
	fs := &fakeStore{}
	rec := do(t, newAPI(t, fs), http.MethodPost, "/v1/listings", big)
	if rec.Code != 400 || len(fs.creates) != 0 {
		t.Fatalf("%d, %d creates", rec.Code, len(fs.creates))
	}
}

// ---- POST: status codes -----------------------------------------------------

func TestPostCreatedIs202WithIdAndPendingStatus(t *testing.T) {
	fs := &fakeStore{createRes: store.CreateResult{ID: "l-abc", Status: listing.StatusPending, Created: true}}
	rec := do(t, newAPI(t, fs), http.MethodPost, "/v1/listings", okBody, "Idempotency-Key", "k1")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["id"] != "l-abc" || got["status"] != "pending" || len(got) != 2 {
		t.Fatalf("body %v", got)
	}
	if loc := rec.Header().Get("Location"); loc != "/v1/listings/l-abc" {
		t.Fatalf("Location %q", loc)
	}
}

func TestPostReplayIs200WithCurrentStatus(t *testing.T) {
	fs := &fakeStore{createRes: store.CreateResult{ID: "l-abc", Status: listing.StatusPublished, Created: false}}
	rec := do(t, newAPI(t, fs), http.MethodPost, "/v1/listings", okBody, "Idempotency-Key", "k1")
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["id"] != "l-abc" || got["status"] != "published" {
		t.Fatalf("body %v", got)
	}
}

func TestPostStoreErrorsMapToStatusCodes(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"key reused", store.ErrKeyConflict, 409, "idempotency_key_reused"},
		{"busy", store.ErrBusy, 503, "busy"},
		{"other", errors.New("pq: password authentication failed for user lm at 10.0.0.5"), 500, "internal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(t, newAPI(t, &fakeStore{createErr: tc.err}), http.MethodPost, "/v1/listings", okBody, "Idempotency-Key", "k1")
			if rec.Code != tc.status || errCode(t, rec) != tc.code {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
			if strings.Contains(rec.Body.String(), "10.0.0.5") {
				t.Fatal("internal error detail leaked to the client")
			}
			if tc.status == 503 && rec.Header().Get("Retry-After") == "" {
				t.Fatal("503 should say when to retry")
			}
		})
	}
}

// ---- POST: what is handed to storage ----------------------------------------

func TestPostPassesKeyAndHashesToStorage(t *testing.T) {
	fs := &fakeStore{createRes: store.CreateResult{ID: "l-1", Status: listing.StatusPending, Created: true}}
	h := newAPI(t, fs)

	do(t, h, http.MethodPost, "/v1/listings", okBody)
	do(t, h, http.MethodPost, "/v1/listings", okBody, "Idempotency-Key", "abc-123")
	if fs.creates[0].Key != nil {
		t.Fatal("no header must mean no key")
	}
	if k := fs.creates[1].Key; k == nil || *k != "abc-123" {
		t.Fatalf("key %v", k)
	}
	want := listing.Fields{SellerID: "s-1002", Title: "iPhone 13", Description: "Like new", PriceVND: 6_000_000, CategoryID: "c-phones"}
	if fs.creates[1].Fields != want {
		t.Fatalf("fields %+v", fs.creates[1].Fields)
	}
	if !bytes.Equal(fs.creates[1].RequestHash, listing.RequestHash(want)) ||
		!bytes.Equal(fs.creates[1].DupFingerprint, listing.DupFingerprint(want)) {
		t.Fatal("hashes do not match the parsed fields")
	}
}

// What the client wrote around the five fields must not change the idempotency hash...
func TestPostHashIgnoresFieldOrderWhitespaceEscapesAndUnknownFields(t *testing.T) {
	fs := &fakeStore{}
	h := newAPI(t, fs)
	variants := []string{
		okBody,
		"{\n  \"category_id\":\"c-phones\",\n  \"price_vnd\": 6000000,\n  \"description\":\"Like new\",\n  \"title\":\"iPhone 13\",\n  \"seller_id\":\"s-1002\"\n}",
		strings.Replace(okBody, `"c-phones"`, `"c-phones"`, 1),
		strings.Replace(okBody, `{`, `{"unknown":{"nested":[1,2,{"a":"b"}]},`, 1),
		strings.Replace(okBody, `{`, `{"unknown":"something else",`, 1),
	}
	for _, v := range variants {
		if rec := do(t, h, http.MethodPost, "/v1/listings", v, "Idempotency-Key", "k"); rec.Code == 400 || rec.Code == 422 {
			t.Fatalf("variant rejected: %d %s\n%s", rec.Code, rec.Body.String(), v)
		}
	}
	for i := 1; i < len(fs.creates); i++ {
		if !bytes.Equal(fs.creates[0].RequestHash, fs.creates[i].RequestHash) {
			t.Fatalf("variant %d changed the request hash", i)
		}
	}
}

// ...but any change to a value, however small, must.
func TestPostHashChangesWithAnyValue(t *testing.T) {
	fs := &fakeStore{}
	h := newAPI(t, fs)
	variants := []string{
		okBody,
		strings.Replace(okBody, "iPhone 13", "iphone 13", 1),  // case
		strings.Replace(okBody, "iPhone 13", "iPhone  13", 1), // inner whitespace
		strings.Replace(okBody, "6000000", "6000001", 1),      // price
		strings.Replace(okBody, "Like new", "Like new ", 1),   // trailing space
		strings.Replace(okBody, "iPhone 13", "Hàng", 1),       // NFC
		strings.Replace(okBody, "iPhone 13", "Hàng", 1),      // NFD of the same text
		strings.Replace(okBody, "c-phones", "c-laptops", 1),   // category
		strings.Replace(okBody, "s-1002", "s-1003", 1),        // seller
	}
	for _, v := range variants {
		if rec := do(t, h, http.MethodPost, "/v1/listings", v); rec.Code == 400 || rec.Code == 422 {
			t.Fatalf("variant rejected: %d %s", rec.Code, rec.Body.String())
		}
	}
	seen := map[string]int{}
	for i, c := range fs.creates {
		if j, dup := seen[string(c.RequestHash)]; dup {
			t.Fatalf("variants %d and %d share a request hash", j, i)
		}
		seen[string(c.RequestHash)] = i
	}
}

// ---- GET by id --------------------------------------------------------------

func TestGetListing(t *testing.T) {
	l := listing.Listing{ID: "l-1", SellerID: "s-1002", Title: "t", Description: "d", PriceVND: 5, CategoryID: "c-phones", Status: listing.StatusPending}
	rec := do(t, newAPI(t, &fakeStore{getRes: l}), http.MethodGet, "/v1/listings/l-1", "")
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	for _, k := range []string{"id", "seller_id", "title", "description", "price_vnd", "category_id", "status", "created_at"} {
		if _, ok := got[k]; !ok {
			t.Errorf("missing %q in %v", k, got)
		}
	}
	if got["decision"] != nil || got["decided_at"] != nil {
		t.Fatalf("pending listing should have null decision and decided_at: %v", got)
	}
}

func TestGetListingNotFoundIsAnErrorBody(t *testing.T) {
	rec := do(t, newAPI(t, &fakeStore{getErr: store.ErrNotFound}), http.MethodGet, "/v1/listings/l-x", "")
	if rec.Code != 404 || errCode(t, rec) != "not_found" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestGetListingStoreFailureIs500(t *testing.T) {
	rec := do(t, newAPI(t, &fakeStore{getErr: errors.New("boom")}), http.MethodGet, "/v1/listings/l-x", "")
	if rec.Code != 500 || strings.Contains(rec.Body.String(), "boom") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

// ---- GET list ----------------------------------------------------------------

func TestListQueryValidation(t *testing.T) {
	bad := []string{
		"limit=0", "limit=101", "limit=-1", "limit=abc", "limit=", "limit=1.5", "limit=1&limit=2",
		"status=draft", "status=", "status=PUBLISHED", "status=pending&status=published",
		"seller_id=", "seller_id=a&seller_id=b",
	}
	for _, q := range bad {
		t.Run(q, func(t *testing.T) {
			fs := &fakeStore{}
			rec := do(t, newAPI(t, fs), http.MethodGet, "/v1/listings?"+q, "")
			if rec.Code != 400 || errCode(t, rec) != "invalid_request" {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
			if len(fs.lists) != 0 {
				t.Fatal("invalid query reached storage")
			}
		})
	}
}

func TestListQueryPassesFiltersToStorage(t *testing.T) {
	fs := &fakeStore{}
	h := newAPI(t, fs)
	do(t, h, http.MethodGet, "/v1/listings", "")
	do(t, h, http.MethodGet, "/v1/listings?seller_id=s-1002&status=needs_human&limit=100", "")
	do(t, h, http.MethodGet, "/v1/listings?limit=1", "")

	if f := fs.lists[0]; f.Limit != 20 || f.SellerID != nil || f.Status != nil {
		t.Fatalf("defaults: %+v", f)
	}
	f := fs.lists[1]
	if f.Limit != 100 || f.SellerID == nil || *f.SellerID != "s-1002" || f.Status == nil || *f.Status != listing.StatusNeedsHuman {
		t.Fatalf("filters: %+v", f)
	}
	if fs.lists[2].Limit != 1 {
		t.Fatalf("limit: %+v", fs.lists[2])
	}
}

func TestListWithNoResultsIsAnEmptyArrayNotNull(t *testing.T) {
	rec := do(t, newAPI(t, &fakeStore{}), http.MethodGet, "/v1/listings", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"items":[]`) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestListingRoutesAbsentWithoutStore(t *testing.T) {
	rec := do(t, NewHandler(Deps{Log: quiet()}), http.MethodGet, "/v1/listings", "")
	if rec.Code != 404 {
		t.Fatalf("%d", rec.Code)
	}
}

// -0 is the integer 0, so it is valid and hashes like 0: retrying an
// idempotent request with the price spelled the other way is a replay.
func TestPostMinusZeroPriceIsZero(t *testing.T) {
	fs := &fakeStore{createRes: store.CreateResult{ID: "l-1", Status: listing.StatusPending, Created: true}}
	h := newAPI(t, fs)
	for _, price := range []string{"0", "-0"} {
		b := strings.Replace(okBody, "6000000", price, 1)
		if rec := do(t, h, http.MethodPost, "/v1/listings", b, "Idempotency-Key", "k"); rec.Code != http.StatusAccepted {
			t.Fatalf("price %s: %d %s", price, rec.Code, rec.Body.String())
		}
	}
	if len(fs.creates) != 2 || fs.creates[0].Fields.PriceVND != 0 || fs.creates[1].Fields.PriceVND != 0 {
		t.Fatalf("creates: %+v", fs.creates)
	}
	if !bytes.Equal(fs.creates[0].RequestHash, fs.creates[1].RequestHash) {
		t.Fatal("-0 and 0 produced different request hashes")
	}
	for _, price := range []string{"-0.0", "-1", "1e3", "1.0"} {
		b := strings.Replace(okBody, "6000000", price, 1)
		if rec := do(t, h, http.MethodPost, "/v1/listings", b); rec.Code != http.StatusBadRequest {
			t.Fatalf("price %s should be rejected, got %d", price, rec.Code)
		}
	}
}

// A title is checked for length only: whitespace is allowed and kept as sent.
func TestPostBlankTitleIsAcceptedAndStoredVerbatim(t *testing.T) {
	fs := &fakeStore{createRes: store.CreateResult{ID: "l-1", Status: listing.StatusPending, Created: true}}
	b := strings.Replace(okBody, "iPhone 13", "   ", 1)
	if rec := do(t, newAPI(t, fs), http.MethodPost, "/v1/listings", b); rec.Code != http.StatusAccepted {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if got := fs.creates[0].Fields.Title; got != "   " {
		t.Fatalf("title stored as %q", got)
	}
}
