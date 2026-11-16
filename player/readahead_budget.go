package main

import (
	"bufio"
	"math"
	"os"
	"path"
	"runtime/debug"
	"strconv"
	"strings"
)

// readAheadBytes is the shared download budget of the native engine; 0 disables
// read-ahead. Set from -read-ahead-mib before the backend starts.
var readAheadBytes int64

const readAheadMaxAuto = 1 << 30

// autoReadAhead is half of the tighter of physical memory and every cgroup v2
// memory.max above this process, capped at 1 GiB. A full original rarely needs
// more than tens of MiB; the cap is a guard against one huge file.
func autoReadAhead() int64 {
	limit := memTotal()
	if l := cgroupMemoryMax(); l > 0 && (limit == 0 || l < limit) {
		limit = l
	}
	if limit == 0 {
		return 32 << 20
	}
	return min(limit/2, readAheadMaxAuto)
}

func memTotal() int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if fields := strings.Fields(scanner.Text()); len(fields) >= 2 && fields[0] == "MemTotal:" {
			kib, _ := strconv.ParseInt(fields[1], 10, 64)
			return kib << 10
		}
	}
	return 0
}

func cgroupMemoryMax() int64 {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return 0
	}
	var group string
	for line := range strings.SplitSeq(strings.TrimSpace(string(b)), "\n") {
		if rest, ok := strings.CutPrefix(line, "0::"); ok {
			group = rest
		}
	}
	if group == "" {
		return 0
	}
	var limit int64
	for dir := path.Clean(group); ; dir = path.Dir(dir) {
		if v, err := os.ReadFile(path.Join("/sys/fs/cgroup", dir, "memory.max")); err == nil {
			if n, err := strconv.ParseInt(strings.TrimSpace(string(v)), 10, 64); err == nil && (limit == 0 || n < limit) {
				limit = n
			}
		}
		if dir == "/" {
			return limit
		}
	}
}

// raiseMemoryLimit lifts a configured GOMEMLIMIT by the read-ahead budget, so that
// held downloads do not make the collector run continuously.
func raiseMemoryLimit(extra int64) (before, after int64) {
	before = debug.SetMemoryLimit(-1)
	if before == math.MaxInt64 || extra <= 0 {
		return before, before
	}
	debug.SetMemoryLimit(before + extra) // Returns the previous limit.
	return before, before + extra
}
