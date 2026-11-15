package ratatoskr

import (
	"context"
	"testing"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/contract/contracttest"
	"yggdrasil/internal/part"
)

func TestMeetsItsContract(t *testing.T) {
	p := part.New(Name, contracttest.Tokens(), nil)
	r := New([]Target{{Name: "norn", URL: "http://127.0.0.1:1"}}, alert.NewClient("", ""), nil)
	r.Mount(p, 30e9)
	_ = r.check(context.Background())
	contracttest.Run(t, p)
}
