package sdk

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandler(t *testing.T) {
	writeJSON := func(w http.ResponseWriter, status int, v any) {
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(v)
	}
	h := Handler(writeJSON)
	get := func(p string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		return rec
	}
	var idx Index
	if err := json.Unmarshal(get("/sdk/node/").Body.Bytes(), &idx); err != nil || idx.Latest == "" || len(idx.Packages) == 0 {
		t.Fatalf("index: %+v %v", idx, err)
	}
	if r := get("/sdk/node/" + idx.Packages[0].File); r.Code != 200 || r.Header().Get("Content-Type") != "application/gzip" || r.Body.Len() < 1000 {
		t.Fatalf("tarball: %d %q %d", r.Code, r.Header().Get("Content-Type"), r.Body.Len())
	}
	if r := get("/sdk/node/README.md"); r.Code != 200 {
		t.Fatalf("readme: %d", r.Code)
	}
	for _, p := range []string{"/sdk/node/../sdk.go", "/sdk/node/nope.tgz", "/sdk/node/files/README.md"} {
		if r := get(p); r.Code != 404 {
			t.Errorf("%s: %d", p, r.Code)
		}
	}
}

func TestLessVersion(t *testing.T) {
	if !lessVersion("0.9.1", "0.10.0") || lessVersion("1.0.0", "1.0.0") || !lessVersion("1.0", "1.0.1") {
		t.Fatal("version order")
	}
}
