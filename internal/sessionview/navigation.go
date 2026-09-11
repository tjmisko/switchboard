package sessionview

import "github.com/tjmisko/switchboard/internal/state"

// Navigable is independent of presentation. Informational headless or
// unreachable rows remain in the ordered list but have no local focus target.
func Navigable(session state.Session) bool {
	return !session.Headless && (session.Hostname == "" || session.Navigable)
}

// Cycle follows snapshot order, wrapping at either end. An empty navigable
// list is a valid no-op; hiding or changing a display never changes the ring.
func Cycle(sessions []state.Session, direction string) (state.Session, bool) {
	ring := make([]state.Session, 0, len(sessions))
	for _, s := range sessions {
		if Navigable(s) {
			ring = append(ring, s)
		}
	}
	if len(ring) == 0 {
		return state.Session{}, false
	}
	index := -1
	for i, s := range ring {
		if s.Focused {
			index = i
			break
		}
	}
	if direction == "next" || direction == "up" {
		if index < 0 {
			return ring[0], true
		}
		return ring[(index+1)%len(ring)], true
	}
	if index < 0 {
		return ring[len(ring)-1], true
	}
	return ring[(index-1+len(ring))%len(ring)], true
}
