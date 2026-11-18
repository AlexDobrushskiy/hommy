package claude

import (
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
)

// TreeGroups is process group pgid plus the groups of every descendant of a
// process in it: Claude Code runs each Bash-tool command in a group of its
// own, which a signal to claude's group alone misses. The caller's own group
// is never included.
func TreeGroups(pgid int) []int {
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,pgid=").Output()
	if err != nil {
		return []int{pgid}
	}
	return treeGroups(string(out), pgid, syscall.Getpgrp())
}

// SignalGroups sends sig to each process group.
func SignalGroups(groups []int, sig syscall.Signal) {
	for _, g := range groups {
		if g > 1 {
			syscall.Kill(-g, sig)
		}
	}
}

// treeGroups reads `ps -axo pid=,ppid=,pgid=` output.
func treeGroups(ps string, pgid, never int) []int {
	type proc struct{ pid, pgid int }
	kids := map[int][]proc{}
	var roots []int
	for _, line := range strings.Split(ps, "\n") {
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		g, err3 := strconv.Atoi(f[2])
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		kids[ppid] = append(kids[ppid], proc{pid, g})
		if g == pgid {
			roots = append(roots, pid)
		}
	}
	groups := map[int]bool{pgid: true}
	seen := map[int]bool{}
	var walk func(int)
	walk = func(pid int) {
		if seen[pid] {
			return
		}
		seen[pid] = true
		for _, k := range kids[pid] {
			groups[k.pgid] = true
			walk(k.pid)
		}
	}
	for _, r := range roots {
		walk(r)
	}
	delete(groups, never)
	out := make([]int, 0, len(groups))
	for g := range groups {
		out = append(out, g)
	}
	slices.Sort(out)
	return out
}

func union(a, b []int) []int {
	out := slices.Clone(a)
	for _, g := range b {
		if !slices.Contains(out, g) {
			out = append(out, g)
		}
	}
	return out
}
