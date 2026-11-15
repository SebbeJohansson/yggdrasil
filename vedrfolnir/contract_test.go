package vedrfolnir

import (
	"path/filepath"
	"testing"

	"yggdrasil/internal/alert"
	"yggdrasil/internal/contract/contracttest"
	"yggdrasil/internal/part"
)

func TestMeetsItsContract(t *testing.T) {
	machine := NewMachine(filepath.Join(t.TempDir(), "history.json"))
	machine.Sample()
	machine.Sample()
	p := part.New(Name, contracttest.Tokens(), nil)
	New(machine, t.TempDir(), []Probe{{Name: "nothing", URL: "http://127.0.0.1:1"}}, alert.NewClient("", "")).Mount(p)
	contracttest.Run(t, p)
}
