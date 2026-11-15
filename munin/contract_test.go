package munin

import (
	"context"
	"testing"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/contract/contracttest"
	"yggdrasil/internal/part"
)

func TestMeetsItsContract(t *testing.T) {
	p := part.New(Name, contracttest.Tokens(), nil)
	m := New([]Target{{Name: "ollama", Kind: Ollama, URL: "http://127.0.0.1:1/api/tags", Local: true}}, nil, "", alert.NewClient("", ""))
	m.Mount(p)
	_ = m.round(context.Background(), true)
	contracttest.Run(t, p)
}
