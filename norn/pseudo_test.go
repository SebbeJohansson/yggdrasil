package norn

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"yggdrasil/internal/contract/contracttest"
	"yggdrasil/internal/llm"
	"yggdrasil/internal/part"
)

// namesModel is a local model that finds the names given, and answers
// nothing else.
func namesModel(t *testing.T, n *Norn, names ...string) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Messages []llm.Message `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.Messages) == 0 || body.Messages[0].Content != namesSystem {
			http.Error(w, "only names here", http.StatusInternalServerError)
			return
		}
		var found []string
		for _, name := range names {
			if strings.Contains(body.Messages[len(body.Messages)-1].Content, name) {
				found = append(found, name)
			}
		}
		answer, _ := json.Marshal(map[string]any{"names": append([]string{}, found...)})
		part.JSON(w, 200, map[string]any{"choices": []map[string]any{{"message": map[string]any{"content": string(answer)}}}})
	}))
	t.Cleanup(server.Close)
	n.LLM = &llm.Client{PrimaryURL: server.URL + "/v1", OllamaURL: "http://127.0.0.1:1"}
}

func TestHandoverSendsNoNames(t *testing.T) {
	n, _ := newNorn(t)
	fake := withSindri(t, n)
	namesModel(t, n, "Kalle Ek")
	now := time.Now()
	n.Accounts.Work.Set(CalendarSnapshot{From: now.Add(-time.Hour).UnixMilli(), To: now.Add(24 * time.Hour).UnixMilli(), At: now.UnixMilli(),
		Events: []CalendarEvent{{Subject: "Sync", Start: now.Add(time.Hour).UnixMilli(), Attendees: []string{"Anna Svensson"}}}})
	_, err := n.Ask(context.Background(), AskRequest{Question: "/claude Did Kalle Ek or Anna answer anna.s@example.com about the deck?", Session: "s9"})
	if err != nil {
		t.Fatal(err)
	}
	sent := fake.called("POST /v1/work/ask")
	if sent == "" {
		t.Fatalf("nothing handed over: %v", fake.calls)
	}
	for _, name := range []string{"Kalle", "Anna", "example.com"} {
		if strings.Contains(sent, name) {
			t.Fatalf("%q went to Claude: %s", name, sent)
		}
	}
	if !strings.Contains(sent, "Person 1") || !strings.Contains(sent, "replaced with placeholders") {
		t.Fatalf("no placeholders or legend: %s", sent)
	}

	// Sindri posts the agent's answer back: the names come back with it.
	p := part.New(Name, map[string]string{contracttest.Sender: "sindri"}, nil)
	n.Mount(p)
	request := httptest.NewRequest("POST", "/v1/conversations/s9/agent-turns", strings.NewReader(`{"agent":"a1","label":"ask: Person 2","content":"Person 1, Person 2 and Person 3 all did."}`))
	request.Header.Set("Authorization", "Bearer "+contracttest.Sender)
	p.ServeHTTP(httptest.NewRecorder(), request)
	chat, _ := n.Store.Chat(context.Background(), "s9", 0, 100)
	last := chat[len(chat)-1]
	if strings.Contains(last.Content, "Person ") || !strings.Contains(last.Content, "Kalle Ek") || !strings.Contains(last.Content, "Anna Svensson") || !strings.Contains(last.Content, "anna.s@example.com") {
		t.Fatalf("names not restored: %q", last.Content)
	}
}

func TestHandoverStopsWithoutTheNamesModel(t *testing.T) {
	n, _ := newNorn(t)
	fake := withSindri(t, n)
	n.LLM = &llm.Client{OllamaURL: "http://127.0.0.1:1"}
	response, _ := n.Ask(context.Background(), AskRequest{Question: "/claude what did Kalle say?", Session: "s10"})
	if fake.called("POST /v1/work/ask") != "" || !strings.Contains(response.Answer, "Nothing was sent to Claude") {
		t.Fatalf("handed over without looking for names: %+v", response)
	}
}

func TestBriefParts(t *testing.T) {
	me := Me{ID: "me-id", Address: "me@corp.se"}
	now := time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)
	pages := map[string]string{
		"/v1.0/me/events/": `{"subject":"Sync","body":{"contentType":"html","content":"<p>Agenda: the Q4 deck</p>"},
			"organizer":{"emailAddress":{"name":"Anna Svensson","address":"Anna@corp.se"}},
			"attendees":[{"type":"required","emailAddress":{"name":"Me","address":"me@corp.se"}},
				{"type":"required","emailAddress":{"name":"Kalle Ek","address":"kalle@corp.se"}},
				{"type":"resource","emailAddress":{"name":"Room 1","address":"room@corp.se"}}]}`,
		"/v1.0/me/messages": `{"value":[{"id":"m1","subject":"Deck","from":{"emailAddress":{"name":"Kalle Ek","address":"kalle@corp.se"}},
			"receivedDateTime":"2026-11-10T10:00:00Z","bodyPreview":"Slides are late"},
			{"id":"m2","subject":"Old","receivedDateTime":"2026-09-01T10:00:00Z","bodyPreview":"too old"}]}`,
		"/v1.0/me/chats?": `{"value":[{"id":"c1","chatType":"oneOnOne","members":[{"displayName":"Me","userId":"me-id"},{"displayName":"Kalle Ek","userId":"k","email":"kalle@corp.se"}],
			"lastMessagePreview":{"createdDateTime":"2026-11-17T10:00:00Z"}},
			{"id":"c2","chatType":"oneOnOne","members":[{"displayName":"Someone Else","userId":"x"}],"lastMessagePreview":{"createdDateTime":"2026-11-17T10:00:00Z"}}]}`,
		"/v1.0/me/chats/c1/": `{"value":[{"messageType":"message","createdDateTime":"2026-11-17T10:00:00Z","from":{"user":{"displayName":"Kalle Ek"}},"body":{"content":"<p>Can we move it?</p>"}}]}`,
	}
	get := func(_ context.Context, path string, into any) error {
		for prefix, body := range pages {
			if strings.HasPrefix(path, prefix) {
				return json.Unmarshal([]byte(body), into)
			}
		}
		t.Fatalf("unexpected Graph read %s", path)
		return nil
	}
	ctx := context.Background()
	invitation, err := readInvitation(ctx, get, "e1", me)
	if err != nil || len(invitation.Invitees) != 2 || !invitation.Invitees[0].Organizer || invitation.Invitees[0].Address != "anna@corp.se" || invitation.Body != "Agenda: the Q4 deck" {
		t.Fatalf("invitation %+v, %v", invitation, err)
	}
	since := now.AddDate(0, 0, -briefDays)
	mails, err := searchMail(ctx, get, invitation.Invitees, since)
	if err != nil || len(mails) != 1 || mails[0].Preview != "Slides are late" {
		t.Fatalf("mails %+v, %v", mails, err)
	}
	chats, err := readChats(ctx, get, invitation.Invitees, me, since)
	if err != nil || len(chats) != 1 || chats[0].Lines[0].Text != "Can we move it?" {
		t.Fatalf("chats %+v, %v", chats, err)
	}
	digest := BriefDigest("Sync", now, "Anna Svensson", []string{"Kalle Ek"}, invitation.Body, mails, chats, time.UTC)
	for _, want := range []string{"The meeting: Sync, Wed 2026-11-18 09:00", "Organizer: Anna Svensson", "Agenda: the Q4 deck",
		"- 2026-11-10, from Kalle Ek: Deck — Slides are late", `Chat "Kalle Ek":`, "- 2026-11-17 Kalle Ek: Can we move it?"} {
		if !strings.Contains(digest, want) {
			t.Fatalf("digest lacks %q:\n%s", want, digest)
		}
	}
}
