package gjallar

import (
	"testing"

	"yggdrasil/internal/contract/contracttest"
	"yggdrasil/internal/part"
)

func TestMeetsItsContract(t *testing.T) {
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	p := part.New(Name, contracttest.Tokens(), nil)
	New(store, nil).Mount(p)
	contracttest.Run(t, p)
}
