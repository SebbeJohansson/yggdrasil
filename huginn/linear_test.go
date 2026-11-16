package huginn

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/call"
	"yggdrasil/internal/tell"
)

func TestATodoTicketWithNoGoalIsSuggestedOnceWhenNornSaysItIsDevelopment(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	var told []tell.Message
	goal := int64(7)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/v1/goals/candidates":
			if r.URL.Query().Get("from") != "linear" || r.URL.Query().Get("assignee") != "mine" {
				t.Errorf("asked Sindri for %s", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"from": "linear", "candidates": []Candidate{
				{Source: "linear:AI-43", Title: "Skapa ett dedikerat formulär", State: "Todo", StateType: "unstarted"},
				{Source: "linear:AI-102", Title: "Undersök Copilot för copywriters", State: "Todo", StateType: "unstarted"},
				{Source: "linear:AI-50", Title: "Already going", State: "In Progress", StateType: "started"},
				{Source: "linear:AI-60", Title: "Has a goal", State: "Todo", StateType: "unstarted", Goal: &goal},
			}})
		case "/v1/ask":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			asked = append(asked, body["question"])
			answer := "No."
			if strings.Contains(body["question"], "AI-43") {
				answer = "Yes"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"answer": answer})
		case "/v1/messages":
			var m tell.Message
			_ = json.NewDecoder(r.Body).Decode(&m)
			told = append(told, m)
			w.WriteHeader(201)
		}
	}))
	defer server.Close()
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	h, err := New(context.Background(), store, call.New("norn", server.URL, "t", time.Second), call.New("sindri", server.URL, "t", time.Second),
		alert.NewClient("", ""), tell.NewHermodr(server.URL, "t"), time.UTC, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	h.DashboardURL = "http://localhost:17420"

	for range 2 {
		if err := h.watchLinear(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(asked) != 2 || !strings.HasPrefix(asked[0], "/local Is this Linear ticket software development work") {
		t.Fatalf("asked Norn %d times: %q", len(asked), asked)
	}
	if len(told) != 1 || told[0].Title != "AI-43 Skapa ett dedikerat formulär" ||
		told[0].Text != "AI-43 is in Todo and no goal has it: Skapa ett dedikerat formulär. Add it as a goal?" ||
		told[0].Link != "http://localhost:17420/dashboard/goals?import=linear&ticket=AI-43" || !told[0].Hold {
		t.Fatalf("told %+v", told)
	}
}

func TestATicketNornCannotJudgeNeitherBlocksTheOthersNorFailsForGood(t *testing.T) {
	var mu sync.Mutex
	asked := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/v1/goals/candidates":
			_ = json.NewEncoder(w).Encode(map[string]any{"candidates": []Candidate{
				{Source: "linear:AI-58", Title: "Prepare the planning meeting", State: "Todo", StateType: "unstarted"},
				{Source: "linear:AI-59", Title: "Fix the importer", State: "Todo", StateType: "unstarted"},
			}})
		case "/v1/ask":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if strings.Contains(body["question"], "AI-58") {
				asked["AI-58"]++
				http.Error(w, `{"error":"the model did not answer"}`, http.StatusBadGateway)
				return
			}
			asked["AI-59"]++
			_ = json.NewEncoder(w).Encode(map[string]any{"answer": "No"})
		}
	}))
	defer server.Close()
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	h, err := New(context.Background(), store, call.New("norn", server.URL, "t", time.Second), call.New("sindri", server.URL, "t", time.Second),
		alert.NewClient("", ""), tell.NewHermodr(server.URL, "t"), time.UTC, t.Logf)
	if err != nil {
		t.Fatal(err)
	}

	// The first pass: AI-59 is judged although AI-58 failed before it, and the pass is not a failure.
	if err := h.watchLinear(context.Background()); err != nil {
		t.Fatalf("pass 1: %v", err)
	}
	if _, judged, _ := store.Judged(context.Background(), "AI-59"); !judged {
		t.Fatal("AI-59 was not judged")
	}
	// Now AI-58 is the only one asked: those passes fail, until the third sets it aside.
	if err := h.watchLinear(context.Background()); err == nil {
		t.Fatal("pass 2: want the failure, as every ask failed")
	}
	if err := h.watchLinear(context.Background()); err != nil {
		t.Fatalf("pass 3 should set AI-58 aside: %v", err)
	}
	if dev, judged, _ := store.Judged(context.Background(), "AI-58"); !judged || dev {
		t.Fatalf("AI-58: judged %v, development %v", judged, dev)
	}
	if err := h.watchLinear(context.Background()); err != nil || asked["AI-58"] != LinearTries || asked["AI-59"] != 1 {
		t.Fatalf("pass 4: %v, asked %v", err, asked)
	}
}

func TestIsYes(t *testing.T) {
	for answer, want := range map[string]bool{"Yes": true, "yes, it is code": true, `"Yes."`: true, "Ja": true, "No": false, "Probably": false, "": false} {
		if IsYes(answer) != want {
			t.Errorf("%q", answer)
		}
	}
}
