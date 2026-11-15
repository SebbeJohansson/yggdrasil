// Package contracttest holds a built-in part to its contract in its tests:
// the routes it serves are the spec's, and it passes `ygg check`.
package contracttest

import (
	"context"
	"net/http/httptest"
	"testing"

	"yggdrasil/api"
	"yggdrasil/internal/contract"
	"yggdrasil/internal/part"
)

// Owner and Sender are tokens a test part can be given.
const (
	Owner  = "contract-owner-token-0123456789abcdef"
	Sender = "contract-sender-token-0123456789abcde"
)

// Tokens names Owner as the owner and Sender as the part `sender`.
func Tokens() map[string]string {
	return map[string]string{Owner: part.Owner, Sender: "sender"}
}

// Run checks p, which was made with Tokens(), against its spec.
func Run(t *testing.T, p *part.Part) {
	t.Helper()
	spec, err := contract.Load(p.Name, api.Spec(p.Name))
	if err != nil {
		t.Fatal(err)
	}
	for _, problem := range spec.CompareRoutes(p.Routes()) {
		t.Error(problem)
	}
	server := httptest.NewServer(p)
	defer server.Close()
	for _, result := range contract.Check(context.Background(), spec, server.URL, contract.Options{OwnerToken: Owner, SenderToken: Sender}) {
		switch {
		case result.Skip:
			t.Logf("skip %s: %s", result.Name, result.Detail)
		case !result.OK:
			t.Errorf("%s: %s", result.Name, result.Detail)
		}
	}
}
