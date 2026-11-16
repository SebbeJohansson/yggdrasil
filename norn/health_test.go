package norn

import (
	"context"
	"net/http"
	"testing"
	"time"

	"yggdrasil/internal/llm"
)

func TestFailureKind(t *testing.T) {
	for text, want := range map[string]string{
		`/completions answered 502: {"error": {"type": "structured_output_failed"}}`:  failJSON,
		`Post "http://127.0.0.1:8095/v1/chat/completions": context deadline exceeded`: failTimeout,
		`The operation was aborted due to timeout`:                                    failTimeout,
		`fetch failed`:                       failUnreachable,
		`dial tcp: connection refused`:       failUnreachable,
		`ollama answered 500: out of memory`: failOther,
	} {
		if got := FailureKind(text); got != want {
			t.Errorf("FailureKind(%q) = %s, want %s", text, got, want)
		}
	}
}

func TestModelHealthCountsByHourAndPurposeWithTheWorstFirst(t *testing.T) {
	now := time.Date(2026, 11, 16, 14, 30, 0, 0, time.UTC)
	at := func(ago time.Duration) int64 { return now.Add(-ago).UnixMilli() }
	calls := []HealthCall{
		{At: at(30 * time.Hour), Purpose: "answer", Backend: "xestrata", Error: "too old"},
		{At: at(3 * time.Hour), Purpose: "summary", Backend: "xestrata", TotalMS: 100},
		{At: at(2 * time.Hour), Purpose: "summary", Backend: "ollama", TotalMS: 300},
		{At: at(time.Hour), Purpose: "answer", Backend: "xestrata", Error: "structured_output_failed"},
		{At: at(10 * time.Minute), Purpose: "answer", Backend: "xestrata", Error: "context deadline exceeded"},
		{At: at(5 * time.Minute), Purpose: "answer", Backend: "xestrata", TotalMS: 900},
	}
	health := SummarizeHealth(calls, now, 24)
	if len(health.Hours) != 24 || health.Hours[23].At != now.Truncate(time.Hour).UnixMilli() {
		t.Fatalf("hours: %d, last at %d", len(health.Hours), health.Hours[23].At)
	}
	if health.Total.Calls != 5 || health.Total.Failed != 2 || health.Total.Ollama != 1 {
		t.Fatalf("total = %+v", health.Total)
	}
	if last := health.Hours[23]; last.Calls != 2 || last.Failed != 1 || last.Kinds[failTimeout] != 1 {
		t.Fatalf("this hour = %+v", last)
	}
	if first := health.Purposes[0]; first.Purpose != "answer" || first.Failed != 2 || first.MedianMS != 900 {
		t.Fatalf("worst purpose = %+v", first)
	}
	if health.Purposes[1].MedianMS != 300 {
		t.Fatalf("summary median = %d", health.Purposes[1].MedianMS)
	}
	if len(health.Recent) != 2 || health.Recent[0].Kind != failTimeout {
		t.Fatalf("recent = %+v", health.Recent)
	}
}

func TestModelHealthRoute(t *testing.T) {
	n, p := newNorn(t)
	if err := n.Store.AddCall(context.Background(), llm.Call{At: time.Now().UnixMilli(), Purpose: "answer", Backend: "xestrata", Model: "m", Error: "structured_output_failed"}); err != nil {
		t.Fatal(err)
	}
	code, body := call(t, p, http.MethodGet, "/v1/models/health?hours=2", nil)
	if code != http.StatusOK {
		t.Fatalf("status %d: %v", code, body)
	}
	if total := body.(map[string]any)["total"].(map[string]any); total["failed"] != 1.0 {
		t.Fatalf("total = %v", total)
	}
	if code, _ := call(t, p, http.MethodGet, "/v1/models/health?hours=0", nil); code != http.StatusBadRequest {
		t.Fatalf("hours=0 gave %d", code)
	}
}

func TestAJSONFailureARetryAnsweredIsNotCountedAgainstTheModel(t *testing.T) {
	failedJSON := HealthCall{Purpose: "answer", Error: "structured_output_failed"}
	retried := HealthCall{Purpose: retryPurpose}
	calls := []HealthCall{failedJSON, retried, failedJSON, retried, failedJSON, retried, failedJSON, retried, failedJSON, retried, {Purpose: "summary"}}
	if failed, total := Unrecovered(calls); failed != 0 || total != 6 {
		t.Fatalf("recovered: %d of %d failed", failed, total)
	}
	if failing, _, _ := Failing(calls); failing {
		t.Fatal("recovered answers counted as failing")
	}
	broken := []HealthCall{failedJSON, failedJSON, failedJSON, {Purpose: "summary", Error: "context deadline exceeded"}, {Purpose: "summary", Error: "fetch failed"}, {Purpose: "summary"}}
	if failing, failed, total := Failing(broken); !failing || failed != 5 || total != 6 {
		t.Fatalf("broken: failing %v, %d of %d", failing, failed, total)
	}
}
