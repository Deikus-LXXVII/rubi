package home

import (
	"errors"
	"sync"
	"time"
)

// The folder browser (fs.list) is asked for by the Rubi panel through core, and core holds the pairing token. A core
// that has been taken over could ask for the same listing in a loop and read the names of the user's folders, so Home
// limits how fast it answers and counts what it was asked: 30 calls a minute, 300 an hour, two at once. A person
// walking through folders uses a handful of calls. The count of the last hour is in `hello`, for the panel to show.
const (
	fsListPerMinute = 30
	fsListPerHour   = 300
	fsListAtOnce    = 2
)

var errFsListBusy = errors.New("Rubi Home is already listing folders; try again in a moment")
var errFsListRate = errors.New("Rubi Home lists folders at most 30 times a minute and 300 times an hour; try again in a minute")

// callLimiter is a sliding window of the times of the calls it let through, a window of every call it was asked for (for
// the counter), and the number of calls in progress.
type callLimiter struct {
	mu      sync.Mutex
	allowed []time.Time // calls let through, last hour
	asked   []time.Time // every call, last hour
	busy    int
	now     func() time.Time // time.Now when nil
}

func (l *callLimiter) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

func keepSince(ts []time.Time, since time.Time) []time.Time {
	i := 0
	for i < len(ts) && !ts[i].After(since) {
		i++
	}
	return ts[i:]
}

// begin lets a call through or refuses it. The returned function ends the call.
func (l *callLimiter) begin() (func(), error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock()
	l.asked = append(keepSince(l.asked, now.Add(-time.Hour)), now)
	l.allowed = keepSince(l.allowed, now.Add(-time.Hour))
	if l.busy >= fsListAtOnce {
		return nil, errFsListBusy
	}
	recent := 0
	for _, t := range l.allowed {
		if t.After(now.Add(-time.Minute)) {
			recent++
		}
	}
	if recent >= fsListPerMinute || len(l.allowed) >= fsListPerHour {
		return nil, errFsListRate
	}
	l.allowed = append(l.allowed, now)
	l.busy++
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			l.busy--
			l.mu.Unlock()
		})
	}, nil
}

// lastHour is how many calls for a listing there were in the last hour, those refused included.
func (l *callLimiter) lastHour() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.asked = keepSince(l.asked, l.clock().Add(-time.Hour))
	return len(l.asked)
}
