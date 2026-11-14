// Package listing holds the domain types shared by the API, the worker and the
// rules, plus the two pure functions that identify a listing: the request hash
// (idempotency) and the duplicate fingerprint (R3).
package listing

import (
	"encoding/json"
	"time"
)

// Status is a listing's lifecycle state. The values are the API's.
type Status string

const (
	StatusPending      Status = "pending"
	StatusAutoApproved Status = "auto_approved" // part of the enum but never stored: approval and publish are one step
	StatusAutoRejected Status = "auto_rejected"
	StatusNeedsHuman   Status = "needs_human"
	StatusPublished    Status = "published"
	StatusRejected     Status = "rejected"
	StatusExpired      Status = "expired"
)

// AllStatuses lists the API's status enum, for validating ?status=.
var AllStatuses = []Status{
	StatusPending, StatusAutoApproved, StatusAutoRejected, StatusNeedsHuman,
	StatusPublished, StatusRejected, StatusExpired,
}

// Valid reports whether s is one of the API's statuses.
func (s Status) Valid() bool {
	for _, v := range AllStatuses {
		if s == v {
			return true
		}
	}
	return false
}

// Fields are the five business fields of a submission, exactly as decoded from
// the request. Nothing here is normalised: what the client sent is what is
// stored and returned.
type Fields struct {
	SellerID    string
	Title       string
	Description string
	PriceVND    int64
	CategoryID  string
}

// Listing is a stored listing as the API returns it (openapi Listing schema).
type Listing struct {
	ID          string     `json:"id"`
	SellerID    string     `json:"seller_id"`
	Title       string     `json:"title"`
	Description string     `json:"description"`
	PriceVND    int64      `json:"price_vnd"`
	CategoryID  string     `json:"category_id"`
	Status      Status     `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	DecidedAt   *time.Time `json:"decided_at"`
	// Decision is {"outcome": ..., "reasons": [...]} once the listing has left
	// pending, and JSON null before that. It is kept as stored.
	Decision json.RawMessage `json:"decision"`
}
