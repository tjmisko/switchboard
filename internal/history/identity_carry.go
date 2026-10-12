package history

import "time"

// CarrySessionIdentities finds the last route and response evidence recorded
// before a window for sessions that have usage inside it. It reads earlier days
// only until every requested session has a base identity, preserving the
// original timestamps so a newer identity inside the window wins.
func CarrySessionIdentities(dir string, before time.Time, window []Event) ([]Event, error) {
	need := make(map[string]bool)
	for _, ev := range window {
		if ev.SessionID != "" && (ev.Type == EventUsageSample || ev.Type == EventUsageCutover) {
			need[ev.Agent+"\x00"+ev.SessionID] = true
		}
	}
	if len(need) == 0 {
		return nil, nil
	}
	days, err := Days(dir)
	if err != nil {
		return nil, err
	}
	base := make(map[string]Event)
	evidence := make(map[string]Event)
	for d := len(days) - 1; d >= 0 && len(base) < len(need); d-- {
		dayStart, err := time.ParseInLocation("2006-01-02", days[d], time.Local)
		if err != nil || !dayStart.Before(before) {
			continue
		}
		events, err := ReadDay(dir, days[d])
		if err != nil {
			return nil, err
		}
		for i := len(events) - 1; i >= 0; i-- {
			ev := events[i]
			if ev.Type != EventSessionIdentity || !ev.Ts.Before(before) {
				continue
			}
			key := ev.Agent + "\x00" + ev.SessionID
			if !need[key] {
				continue
			}
			if ev.RateLimitsPresent != nil {
				if _, ok := evidence[key]; !ok {
					evidence[key] = ev
				}
			} else if _, ok := base[key]; !ok {
				base[key] = ev
			}
		}
	}
	carried := make([]Event, 0, len(base)+len(evidence))
	for key, ev := range base {
		carried = append(carried, ev)
		if seen, ok := evidence[key]; ok {
			carried = append(carried, seen)
		}
	}
	return carried, nil
}
