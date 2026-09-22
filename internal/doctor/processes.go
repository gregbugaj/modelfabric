package doctor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gregbugaj/modelfabric/internal/osproc"
)

// The processes ModelFabric started on this machine, and whether it can still stop
// them.
//
// Other sections say whether things work. This one answers a different
// question — "what is running because of ModelFabric, and does ModelFabric still own it?"
// — which is the question you have when something needs stopping and the
// obvious command says there is nothing to stop.
//
// That is not hypothetical. A node was running as an orphaned `mfsh serve`
// (parent pid 1), so `mfsh down` reported "no node is running" while the node
// answered every request put to it, and `mfsh up` refused because the port was
// held. Nothing in the report said so: the node section reported it up, healthy
// and on the current build, which was all true and none of it useful. The
// process had to be found with `ss -ltnp` and stopped by hand.

// daemonRecord mirrors what `mfsh up` writes to <state>/node.json. Declared
// here rather than imported because it belongs to the CLI, and doctor reads it
// as a file on disk — the same way it reads everything else it reports on.
type daemonRecord struct {
	PID     int    `json:"pid"`
	BirthID string `json:"birth_id"`
	Listen  string `json:"listen"`
	Started string `json:"started"`
}

func (d *doctor) checkProcesses() {
	const s = "Processes"

	owned := 0
	if d.nodeUp && d.nodePID > 0 {
		owned++
		d.checkNodeOwnership(s)
	}

	// Engines, one line each. The Engines section reports on the ones ModelFabric
	// does *not* own, because those are the surprising ones; these are listed
	// because "which of these processes are mine" is exactly what you want to
	// know before killing anything.
	gpuMem := gpuMemoryByPID()
	byPID := make([]doctorInstance, len(d.instances))
	copy(byPID, d.instances)
	sort.Slice(byPID, func(i, j int) bool { return byPID[i].PID < byPID[j].PID })
	for _, i := range byPID {
		if i.PID == 0 {
			// A recorded instance with no pid is one ModelFabric is fronting but did
			// not launch: a `base_url` engine from the config, say. Saying so
			// is the point of this section.
			d.add(s, "engine", StatusInfo,
				fmt.Sprintf("%s on port %d: ModelFabric routes to it but did not start it, so it cannot stop it", i.Model, i.Port), "")
			continue
		}
		owned++
		detail := fmt.Sprintf("pid %d, port %d, %s", i.PID, i.Port, i.Model)
		if i.Runtime != "" {
			detail += " (" + i.Runtime + ")"
		}
		if mb, ok := gpuMem[i.PID]; ok {
			detail += fmt.Sprintf(", %s VRAM", humanBytes(int64(mb)<<20))
		}
		// Alive, and still the process ModelFabric started: a pid on its own can be
		// reused, and reporting a stranger's process as ours is worse than
		// reporting nothing.
		if !processAlive(i.PID) {
			d.add(s, "engine", StatusWarn, detail+" — recorded, but that process is gone",
				"mfsh unload "+i.ID+" (then load again)")
			continue
		}
		d.add(s, "engine", StatusOK, detail, "")
	}

	// llm-d's two processes write their own pid files; Status does not carry
	// them, and reading the files is how `mfsh llmd disable` finds them too.
	for _, name := range []string{"epp", "envoy"} {
		p := filepath.Join(d.opts.Home, "llmd", name+".pid")
		pid, err := readPIDFile(p)
		if err != nil {
			continue
		}
		if !processAlive(pid) {
			d.add(s, "llm-d "+name, StatusWarn,
				fmt.Sprintf("pid %d recorded in %s, but that process is gone", pid, p),
				"mfsh llmd disable && mfsh llmd enable <model>")
			continue
		}
		owned++
		d.add(s, "llm-d "+name, StatusOK, fmt.Sprintf("pid %d", pid), "")
	}

	d.add(s, "owned", StatusInfo, fmt.Sprintf("ModelFabric started and still owns %s on this machine",
		plural(owned, "process")), "")
}

// checkNodeOwnership answers whether `mfsh down` can stop the running node.
//
// `down` works from the record `mfsh up` writes, matched on the process's birth
// id so a reused pid cannot be mistaken for the node. A node started any other
// way — by hand, by a service manager, or by an `up` whose record was later
// removed — answers normally and ignores `down`, which then says "no node is
// running" about a node that is plainly running.
func (d *doctor) checkNodeOwnership(section string) {
	// Only meaningful for the node on this machine: the record is local, and
	// -addr may point somewhere else. When doctor runs *inside* a node (the
	// dashboard asking a peer) the addr is that node's own listener, so this
	// still compares the right pair.
	if !isLoopbackAddr(d.addr) {
		d.add(section, "node", StatusInfo,
			fmt.Sprintf("pid %d on %s; ownership is not checked for a node on another machine", d.nodePID, d.addr), "")
		return
	}
	up := fmt.Sprintf("pid %d, up %s", d.nodePID, time.Since(d.nodeStarted).Round(time.Second))

	rec, err := readDaemonRecord(d.opts.StateDir)
	switch {
	case err != nil:
		d.add(section, "node", StatusWarn,
			up+" — no record in "+filepath.Join(d.opts.StateDir, "node.json")+
				", so it was not started by `mfsh up` and `mfsh down` will not stop it",
			fmt.Sprintf("kill -TERM %d && mfsh up", d.nodePID))
	case rec.PID != d.nodePID:
		d.add(section, "node", StatusWarn,
			fmt.Sprintf("%s — but the record names pid %d, so `mfsh down` would signal the wrong process or none", up, rec.PID),
			fmt.Sprintf("kill -TERM %d && mfsh up", d.nodePID))
	case !birthMatches(rec):
		// The record's pid is right but the process behind it is not the one
		// that was recorded, which means the pid was reused.
		d.add(section, "node", StatusWarn,
			up+" — the record matches this pid but not this process; the pid was reused since it was written",
			fmt.Sprintf("kill -TERM %d && mfsh up", d.nodePID))
	default:
		d.add(section, "node", StatusOK, up+", started by `mfsh up` and stoppable with `mfsh down`", "")
	}
}

func readDaemonRecord(stateDir string) (*daemonRecord, error) {
	if stateDir == "" {
		return nil, os.ErrNotExist
	}
	b, err := os.ReadFile(filepath.Join(stateDir, "node.json"))
	if err != nil {
		return nil, err
	}
	var r daemonRecord
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	if r.PID <= 0 {
		return nil, os.ErrNotExist
	}
	return &r, nil
}

func birthMatches(r *daemonRecord) bool {
	// An empty recorded birth id is from a build that did not write one; the
	// pid match is all there is, so do not call it a mismatch.
	if r.BirthID == "" {
		return true
	}
	birth, err := osproc.BirthID(r.PID)
	return err == nil && birth == r.BirthID
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if _, err := osproc.BirthID(pid); err != nil {
		return false
	}
	return !osproc.Zombie(pid)
}

func readPIDFile(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("%s: not a pid", path)
	}
	return pid, nil
}

// isLoopbackAddr reports whether an address names this machine.
func isLoopbackAddr(addr string) bool {
	h := strings.TrimPrefix(strings.TrimPrefix(addr, "http://"), "https://")
	if i := strings.IndexByte(h, '/'); i >= 0 {
		h = h[:i]
	}
	if i := strings.LastIndexByte(h, ':'); i >= 0 {
		h = h[:i]
	}
	return isLoopbackHost(h)
}
