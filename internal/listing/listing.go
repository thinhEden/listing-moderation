// Package listing holds the domain types shared by the API, the worker and the
// rules, plus the two pure functions that identify a listing: the request hash
// (idempotency) and the duplicate fingerprint (R3).
package listing

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
