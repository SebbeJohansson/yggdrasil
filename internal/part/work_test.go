package part

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthSaysWorkOnAWorkMachine(t *testing.T) {
	t.Setenv("YGG_WORK", "on")
	p := New("test", map[string]string{"t": Owner}, nil)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if !strings.Contains(rec.Body.String(), `"work":true`) {
		t.Fatalf("health without work: %s", rec.Body.String())
	}
	t.Setenv("YGG_WORK", "")
	rec = httptest.NewRecorder()
	New("test", nil, nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if strings.Contains(rec.Body.String(), `"work"`) {
		t.Fatalf("health with work off: %s", rec.Body.String())
	}
}
