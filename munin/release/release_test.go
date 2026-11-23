package release

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func settings(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestCheck(t *testing.T) {
	if c := Check("a/b", &Run{Name: "ci", Conclusion: "failure", HTMLURL: "http://run"}, nil); c.OK || c.Key != "a/b" {
		t.Fatalf("failed run should fail: %+v", c)
	}
	if c := Check("a/b", &Run{Conclusion: "cancelled"}, nil); !c.OK {
		t.Fatal("cancelled is not a failure")
	}
	if c := Check("a/b", &Run{Conclusion: "success"}, &Deployment{Environment: "prod", State: "error"}); c.OK {
		t.Fatal("failed deploy should fail")
	}
	if c := Check("a/b", nil, nil); !c.OK {
		t.Fatal("nothing seen is ok")
	}
}

func TestNewNeedsRepos(t *testing.T) {
	if _, err := New(settings(nil)); err == nil {
		t.Fatal("want error without repos")
	}
	if _, err := New(settings(map[string]string{"WATCH": "nope"})); err == nil {
		t.Fatal("want error for bad repo")
	}
}

func TestPoll(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/a/b":
			w.Write([]byte(`{"default_branch":"main"}`))
		case "/repos/a/b/actions/runs":
			if r.URL.Query().Get("branch") != "main" {
				t.Error("branch not asked for")
			}
			w.Write([]byte(`{"workflow_runs":[{"name":"ci","conclusion":"failure","html_url":"http://run"}]}`))
		case "/repos/a/b/deployments":
			w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	src, err := New(settings(map[string]string{"WATCH": "a/b,a/missing", "API": srv.URL}))
	if err != nil {
		t.Fatal(err)
	}
	report := src.Poll(context.Background(), "")
	if len(report.Checks) != 2 || report.Checks[0].OK || !report.Checks[1].Skip {
		t.Fatalf("checks: %+v", report.Checks)
	}
	if report.Up {
		t.Fatal("should not be up")
	}
}
