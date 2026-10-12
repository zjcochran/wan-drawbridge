// SPDX-License-Identifier: AGPL-3.0-or-later

// Package learner decides which ISP is primary from how long each was seen.
package learner

import "time"

const (
	// PromoteShare is the 7-day share an ISP needs to become primary after learning.
	PromoteShare = 0.60
	// RelearnShare is the share over RelearnWindow a different ISP needs before it
	// is suggested as the new primary. It is never switched automatically: a long
	// primary outage looks the same as a provider change.
	RelearnShare  = 0.80
	RelearnWindow = 3 * 24 * time.Hour
	// relearnMinCoverage avoids relearning from a window with little data in it.
	relearnMinCoverage = 2 * 24 * time.Hour
)

// Share is how long one ISP was observed in a window.
type Share struct {
	ID      int
	Seconds int64
}

// Input is everything the learner needs. Ignored ISPs must already be excluded.
type Input struct {
	Pinned       int     // pinned primary ISP, 0 if none
	Current      int     // current learned primary ISP, 0 if none
	LearningDone bool    // the learning period has elapsed
	Week         []Share // last 7 days
	Recent       []Share // last RelearnWindow
}

// Decision is the primary ISP to use (0 = none yet) and why.
type Decision struct {
	Primary   int
	Reason    string
	NeedsUser bool // learning is done but no ISP is clearly primary
	Suggest   int  // a different ISP that looks like the new primary, 0 if none
}

func Decide(in Input) Decision {
	if in.Pinned != 0 {
		return Decision{Primary: in.Pinned, Reason: "pinned"}
	}
	if in.Current != 0 {
		top, share, total := leader(in.Recent)
		if top != in.Current && share > RelearnShare && total >= int64(relearnMinCoverage.Seconds()) {
			return Decision{Primary: in.Current, Reason: "learned", Suggest: top}
		}
		return Decision{Primary: in.Current, Reason: "learned"}
	}
	if !in.LearningDone {
		return Decision{Reason: "learning"}
	}
	top, share, _ := leader(in.Week)
	if top != 0 && share >= PromoteShare {
		return Decision{Primary: top, Reason: "learned"}
	}
	return Decision{Reason: "undecided", NeedsUser: true}
}

// leader returns the ISP with the most time, its share and the window total.
func leader(shares []Share) (id int, share float64, total int64) {
	var best int64
	for _, s := range shares {
		total += s.Seconds
		if s.Seconds > best {
			id, best = s.ID, s.Seconds
		}
	}
	if total == 0 {
		return 0, 0, 0
	}
	return id, float64(best) / float64(total), total
}
