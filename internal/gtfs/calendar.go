package gtfs

import (
	"context"
	"time"
)

// Calendar answers which service_ids run on a given day, from calendar.txt
// and the exceptions in calendar_dates.txt.
type Calendar struct {
	services   map[string]*service
	exceptions map[string]map[string]bool // date → service → added
}

type service struct {
	days       [7]bool // indexed by time.Weekday
	start, end string  // YYYYMMDD
}

// Calendar parses the service calendar, memoised.
func (f *Feed) Calendar(ctx context.Context) (*Calendar, error) {
	if err := f.Ensure(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calendar != nil {
		return f.calendar, nil
	}
	c := &Calendar{services: map[string]*service{}, exceptions: map[string]map[string]bool{}}
	days := []string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"}
	err := f.each("calendar.txt", func(rec map[string]string) error {
		s := &service{start: rec["start_date"], end: rec["end_date"]}
		for i, d := range days {
			s.days[i] = rec[d] == "1"
		}
		c.services[rec["service_id"]] = s
		return nil
	})
	if err != nil {
		return nil, err
	}
	// calendar_dates.txt is optional.
	_ = f.each("calendar_dates.txt", func(rec map[string]string) error {
		d := rec["date"]
		if c.exceptions[d] == nil {
			c.exceptions[d] = map[string]bool{}
		}
		c.exceptions[d][rec["service_id"]] = rec["exception_type"] == "1"
		return nil
	})
	f.calendar = c
	return c, nil
}

// Active returns the services running on day (only its date matters).
func (c *Calendar) Active(day time.Time) map[string]bool {
	date := day.Format("20060102")
	out := map[string]bool{}
	for id, s := range c.services {
		if s.days[day.Weekday()] && s.start <= date && date <= s.end {
			out[id] = true
		}
	}
	for id, added := range c.exceptions[date] {
		if added {
			out[id] = true
		} else {
			delete(out, id)
		}
	}
	return out
}
