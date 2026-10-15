package cli

import (
	"database/sql"
	"flag"
	"fmt"
	"time"

	"github.com/ynsuy/mongo-parser/internal/store"
	"github.com/ynsuy/mongo-parser/internal/timeparse"
)

// windowFlags are how every command takes the period it is asked about. They read the same way
// everywhere: two ends, a moment with a width around it, or a stretch counted back from the end of
// what the bundle holds.
type windowFlags struct {
	from, to, around, window string
	last                     time.Duration
}

func addWindowFlags(fs *flag.FlagSet) *windowFlags {
	w := &windowFlags{}
	fs.StringVar(&w.from, "from", "", "start time (UTC)")
	fs.StringVar(&w.to, "to", "", "end time (UTC)")
	fs.StringVar(&w.around, "around", "", "a moment to centre the window on (UTC)")
	fs.StringVar(&w.window, "window", "30m", "width of the --around window")
	fs.DurationVar(&w.last, "last", 0, "window ending at the last thing the bundle recorded, e.g. 2h")
	return w
}

// resolve settles the window against what the bundle actually covers. ftdcLast is the last FTDC
// sample, which is what a relative window counts back from when the metrics reach further than the
// logs do; zero when there is no FTDC.
func (w *windowFlags) resolve(st *store.Store, ftdcLast time.Time) (start, end time.Time, err error) {
	first, last := logSpan(st)
	if ftdcLast.After(last) {
		last = ftdcLast
	}
	if w.last > 0 {
		if w.from != "" || w.to != "" || w.around != "" {
			return time.Time{}, time.Time{}, fmt.Errorf("--last cannot be combined with --from, --to or --around")
		}
		if last.IsZero() {
			return time.Time{}, time.Time{}, fmt.Errorf("--last needs something recorded to count back from, and this bundle has nothing with a time on it")
		}
		return last.Add(-w.last), last, nil
	}
	if w.from == "" && w.to == "" && w.around == "" {
		return time.Time{}, time.Time{}, nil
	}
	around := ""
	if w.around != "" {
		around = w.window
	}
	return timeparse.Window(w.from, w.to, w.around, around, first, last)
}

// logSpan is the period the stored log lines cover.
func logSpan(st *store.Store) (first, last time.Time) {
	var a, b sql.NullInt64
	_ = st.DB.QueryRow(`SELECT min(first_ts), max(last_ts) FROM block`).Scan(&a, &b)
	if !a.Valid {
		return time.Time{}, time.Time{}
	}
	return time.UnixMilli(a.Int64).UTC(), time.UnixMilli(b.Int64).UTC()
}
