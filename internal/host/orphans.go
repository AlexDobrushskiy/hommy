package host

import (
	"log"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/AlexDobrushskiy/hommy/internal/claude"
)

// killOrphanGroup kills process group pgid, with its tool process groups, if
// it is still a run whose host died: it runs a claude and began no later than
// recorded (when the host wrote the pgid down). Anything else holding that id
// is someone else's by now.
func killOrphanGroup(pgid int, recorded time.Time) {
	out, err := exec.Command("ps", "-axo", "pgid=,etime=,command=").Output()
	if err != nil {
		log.Printf("orphan group %d: ps: %v", pgid, err)
		return
	}
	started, ok := orphanGroupStarted(string(out), pgid, time.Now())
	if !ok {
		return
	}
	if started.After(recorded.Add(2 * time.Second)) { // etime has 1 s resolution
		log.Printf("orphan group %d: a claude started %s, after the run was recorded: not ours", pgid, started.Format(time.RFC3339))
		return
	}
	log.Printf("killing orphaned claude process group %d", pgid)
	claude.SignalGroups(claude.TreeGroups(pgid), syscall.SIGKILL)
}

// orphanGroupStarted reads `ps -o pgid=,etime=,command=` output: when the
// earliest claude process of group pgid started (false: none in it).
func orphanGroupStarted(ps string, pgid int, now time.Time) (time.Time, bool) {
	var earliest time.Time
	found := false
	for _, line := range strings.Split(ps, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		g, err := strconv.Atoi(f[0])
		if err != nil || g != pgid || !strings.Contains(strings.Join(f[2:], " "), "claude") {
			continue
		}
		age, ok := parseEtime(f[1])
		if !ok {
			continue
		}
		if s := now.Add(-age); !found || s.Before(earliest) {
			earliest, found = s, true
		}
	}
	return earliest, found
}

// parseEtime reads ps's elapsed time: [[dd-]hh:]mm:ss.
func parseEtime(s string) (time.Duration, bool) {
	var days int
	if d, rest, ok := strings.Cut(s, "-"); ok {
		n, err := strconv.Atoi(d)
		if err != nil {
			return 0, false
		}
		days, s = n, rest
	}
	parts := strings.Split(s, ":")
	if len(parts) > 3 {
		return 0, false
	}
	var secs int
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return 0, false
		}
		secs = secs*60 + n
	}
	return time.Duration(days)*24*time.Hour + time.Duration(secs)*time.Second, true
}
