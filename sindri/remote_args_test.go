package sindri

import (
	"slices"
	"testing"
)

func TestWithRemoteControlComesFirst(t *testing.T) {
	for _, c := range []struct{ in, want []string }{
		{nil, []string{"--remote-control"}},
		{[]string{"--", "-fix the bug"}, []string{"--remote-control", "--", "-fix the bug"}},
		{[]string{"--resume", "abc"}, []string{"--remote-control", "--resume", "abc"}},
	} {
		if got := WithRemoteControl(c.in); !slices.Equal(got, c.want) {
			t.Errorf("WithRemoteControl(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
