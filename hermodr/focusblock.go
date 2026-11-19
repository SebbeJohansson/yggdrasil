package hermodr

// A focus block: time you booked for focus from the week's plan (Norn's
// weave, POST /v1/weave/book), in the work calendar as "Focus" with focus
// true. It is not a meeting, but a message that asks to hold waits through
// it as through one, and GET /v1/channels says inFocusBlock until focusEndsAt.
// Unlike a focus you start yourself (focus.go), it holds only what asks to.

import "time"

// inFocusBlock is whether now is in a focus block you booked, and until when.
func (h *Hermodr) inFocusBlock(now time.Time) (int64, bool) {
	return h.inside(now, func(m meeting) bool { return m.Focus })
}
