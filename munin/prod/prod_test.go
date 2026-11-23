package prod

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPoll(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bad" {
			w.WriteHeader(503)
		}
	}))
	defer srv.Close()
	src, err := New(func(k string) string { return map[string]string{"URLS": srv.URL + "/ok, " + srv.URL + "/bad"}[k] })
	if err != nil {
		t.Fatal(err)
	}
	r := src.Poll(context.Background(), "")
	if len(r.Checks) != 2 || !r.Checks[0].OK || r.Checks[1].OK || r.Checks[1].Severity != "critical" || r.Up {
		t.Fatalf("%+v", r)
	}
	if _, err := New(func(string) string { return "" }); err == nil {
		t.Fatal("want error")
	}
}
