package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"listing-moderation/internal/store"
	"listing-moderation/internal/testdb"
)

// These tests run the real handler against real Postgres over real HTTP.

func pgServer(t *testing.T) *httptest.Server {
	t.Helper()
	st, err := store.Open(context.Background(), testdb.IsolatedDSN(t), 10*time.Second, quiet())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewHandler(Deps{Log: quiet(), Seed: realSeed(t), Store: st}))
	t.Cleanup(srv.Close)
	return srv
}

type reply struct {
	code int
	body map[string]any
	raw  string
}

func send(t *testing.T, method, url, body string, headers ...string) reply {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	r := reply{code: resp.StatusCode, raw: b.String()}
	_ = json.Unmarshal([]byte(r.raw), &r.body)
	return r
}

func body(seller, title string, price int) string {
	return fmt.Sprintf(`{"seller_id":%q,"title":%q,"description":"d","price_vnd":%d,"category_id":"c-phones"}`, seller, title, price)
}

func TestE2ECreateReplayConflictAndRead(t *testing.T) {
	srv := pgServer(t)
	url := srv.URL + "/v1/listings"

	first := send(t, "POST", url, body("s-1002", "iPhone", 6_000_000), "Idempotency-Key", "k1")
	if first.code != 202 || first.body["status"] != "pending" {
		t.Fatalf("first: %d %s", first.code, first.raw)
	}
	id := first.body["id"].(string)

	// Same key, same body (spelled differently): 200 with the same id.
	reordered := `{"category_id":"c-phones","price_vnd":6000000,"description":"d","title":"iPhone","seller_id":"s-1002","ignored":{"x":[1]}}`
	replay := send(t, "POST", url, reordered, "Idempotency-Key", "k1")
	if replay.code != 200 || replay.body["id"] != id {
		t.Fatalf("replay: %d %s", replay.code, replay.raw)
	}

	// Same key, different body: 409, and the stored listing is untouched.
	for _, b := range []string{body("s-1002", "iphone", 6_000_000), body("s-1002", "iPhone", 6_000_001)} {
		if r := send(t, "POST", url, b, "Idempotency-Key", "k1"); r.code != 409 {
			t.Fatalf("conflict: %d %s", r.code, r.raw)
		}
	}

	// Another seller may use the same key.
	other := send(t, "POST", url, body("s-1003", "iPhone", 6_000_000), "Idempotency-Key", "k1")
	if other.code != 202 || other.body["id"] == id {
		t.Fatalf("other seller: %d %s", other.code, other.raw)
	}

	got := send(t, "GET", url+"/"+id, "")
	if got.code != 200 || got.body["status"] != "pending" || got.body["title"] != "iPhone" ||
		got.body["decision"] != nil || got.body["decided_at"] != nil {
		t.Fatalf("get: %d %s", got.code, got.raw)
	}
	if send(t, "GET", url+"/l-missing", "").code != 404 {
		t.Fatal("unknown id should be 404")
	}
}

// A request that is rejected as invalid must not use up its key.
func TestE2EInvalidRequestDoesNotClaimTheKey(t *testing.T) {
	srv := pgServer(t)
	url := srv.URL + "/v1/listings"

	bad := strings.Replace(body("s-1002", "iPhone", 6_000_000), `6000000`, `6000000.5`, 1)
	if r := send(t, "POST", url, bad, "Idempotency-Key", "k1"); r.code != 400 {
		t.Fatalf("%d %s", r.code, r.raw)
	}
	if r := send(t, "POST", url, body("s-9999", "iPhone", 1), "Idempotency-Key", "k1"); r.code != 422 {
		t.Fatalf("%d %s", r.code, r.raw)
	}
	// The key is still free: a valid request with a different body than the failed ones succeeds.
	if r := send(t, "POST", url, body("s-1002", "iPhone", 6_000_000), "Idempotency-Key", "k1"); r.code != 202 {
		t.Fatalf("key was consumed by a failed request: %d %s", r.code, r.raw)
	}
	if r := send(t, "GET", url, ""); len(r.body["items"].([]any)) != 1 {
		t.Fatalf("expected exactly one listing: %s", r.raw)
	}
}

func TestE2ENoKeyCreatesEveryTime(t *testing.T) {
	srv := pgServer(t)
	url := srv.URL + "/v1/listings"
	a := send(t, "POST", url, body("s-1002", "iPhone", 6_000_000))
	b := send(t, "POST", url, body("s-1002", "iPhone", 6_000_000))
	if a.code != 202 || b.code != 202 || a.body["id"] == b.body["id"] {
		t.Fatalf("%s / %s", a.raw, b.raw)
	}
}

// Many clients retrying the same submission at once, over real HTTP.
func TestE2EConcurrentRetriesCreateOneListing(t *testing.T) {
	srv := pgServer(t)
	url := srv.URL + "/v1/listings"
	const n = 25

	replies := make([]reply, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			replies[i] = send(t, "POST", url, body("s-1002", "iPhone", 6_000_000), "Idempotency-Key", "same")
		}()
	}
	close(start)
	wg.Wait()

	created, ids := 0, map[any]bool{}
	for _, r := range replies {
		switch r.code {
		case 202:
			created++
		case 200:
		default:
			t.Fatalf("unexpected reply: %d %s", r.code, r.raw)
		}
		ids[r.body["id"]] = true
	}
	if created != 1 || len(ids) != 1 {
		t.Fatalf("%d created, %d distinct ids", created, len(ids))
	}
	if r := send(t, "GET", url, ""); len(r.body["items"].([]any)) != 1 {
		t.Fatalf("expected exactly one listing: %s", r.raw)
	}
}

// Same key, two different bodies, at once: one body wins, the other is a 409.
func TestE2EConcurrentDifferentBodiesOneWins(t *testing.T) {
	srv := pgServer(t)
	url := srv.URL + "/v1/listings"
	const n = 20

	replies := make([]reply, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			title := "A"
			if i%2 == 1 {
				title = "B"
			}
			replies[i] = send(t, "POST", url, body("s-1002", title, 6_000_000), "Idempotency-Key", "same")
		}()
	}
	close(start)
	wg.Wait()

	var winner string
	created := 0
	for i, r := range replies {
		if r.code == 202 {
			created++
			winner = []string{"A", "B"}[i%2]
		}
	}
	if created != 1 {
		t.Fatalf("%d requests created a listing", created)
	}
	for i, r := range replies {
		title := []string{"A", "B"}[i%2]
		switch {
		case r.code == 202:
		case title == winner && r.code != 200:
			t.Fatalf("request %d has the winning body but got %d %s", i, r.code, r.raw)
		case title != winner && r.code != 409:
			t.Fatalf("request %d has the losing body but got %d %s", i, r.code, r.raw)
		}
	}
}

func TestE2EList(t *testing.T) {
	srv := pgServer(t)
	url := srv.URL + "/v1/listings"

	var ids []string
	for _, c := range []struct{ seller, title string }{{"s-1002", "a"}, {"s-1003", "b"}, {"s-1002", "c"}} {
		r := send(t, "POST", url, body(c.seller, c.title, 6_000_000))
		ids = append(ids, r.body["id"].(string))
	}
	list := func(q string) []string {
		r := send(t, "GET", url+q, "")
		if r.code != 200 {
			t.Fatalf("%s: %d %s", q, r.code, r.raw)
		}
		var out []string
		for _, it := range r.body["items"].([]any) {
			out = append(out, it.(map[string]any)["id"].(string))
		}
		return out
	}
	eq := func(q string, want ...string) {
		t.Helper()
		got := list(q)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("%s: got %v, want %v", q, got, want)
		}
	}
	eq("", ids[2], ids[1], ids[0]) // newest first
	eq("?limit=2", ids[2], ids[1])
	eq("?seller_id=s-1002", ids[2], ids[0])
	eq("?status=pending&limit=1", ids[2])
	eq("?status=published")   // none yet
	eq("?seller_id=s-nobody") // unknown seller: empty, not an error
	if r := send(t, "GET", url+"?limit=0", ""); r.code != 400 {
		t.Fatalf("%d", r.code)
	}
}
