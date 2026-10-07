package router

import (
	"strings"
	"testing"

	"github.com/gregbugaj/modelfabric/internal/mesh"
)

// held is the shares map for a prompt one engine holds part of.
func held(target string, share float64) map[string]float64 {
	if target == "" || share <= 0 {
		return nil
	}
	return map[string]float64{target: share}
}

// The default placement, case by case against what llm-d's "tuned" profile
// does (llm-d-router v0.10.0), on the fleet of the 2026-10-06 runs. The first
// group is where the rule it replaces differed, and lost: 9 of 20 tasks at 35
// minutes to llm-d's 14, on the same machines and tasks.
func TestScheduleFollowsLLMD(t *testing.T) {
	fleet := func(x, m, h int64) []mesh.Candidate {
		return []mesh.Candidate{
			{Name: "xpredator", Local: true, Inflight: x, Slots: 2, PrefillTokS: 1600},
			{Name: "minion", Local: true, Inflight: m, Slots: 4, PrefillTokS: 600},
			{Name: "helion", Local: true, Inflight: h, Slots: 1, PrefillTokS: 250},
		}
	}
	type reading map[string]int64
	for _, c := range []struct {
		name    string
		cands   []mesh.Candidate
		target  string
		share   float64
		tokens  int64
		reading reading  // tokens each engine still has to read
		coldLRU []string // engines by last cold request, oldest first
		want    string
	}{
		// Stays home when home is full. The old rule left for the engine with
		// the fewest in flight, a different machine with nothing cached; with
		// the queue it was held at the router. Here it waits for the next slot
		// on the machine that has its cache.
		{"home has every slot busy: home all the same", fleet(0, 4, 0), "minion", 0.97, 40000, nil, nil, "minion"},
		{"home busy and a faster engine idle: still home", fleet(0, 3, 0), "minion", 0.97, 40000, nil, nil, "minion"},
		// The room filter: an engine with a request already queued is passed
		// over, home included, while another has none queued.
		{"home already has a request waiting for a slot: not home", fleet(1, 5, 0), "minion", 0.97, 40000, nil, nil, "xpredator"},
		{"every engine has one waiting: home again", fleet(3, 5, 2), "minion", 0.97, 40000, nil, nil, "minion"},
		// The load gate, in seconds at each engine's own prefill rate: 15,000
		// tokens to read on minion is 25s, more than 18s beyond an idle 5090.
		// Passing the gate only makes the other engines candidates again; the
		// token score still counts what each would have to read, and a cold
		// read of the whole prompt elsewhere is more than home's backlog.
		{"home's backlog is within 18 seconds: stays", fleet(0, 2, 0), "minion", 0.97, 40000, reading{"minion": 9000}, nil, "minion"},
		// Was "minion", when load was scored in tokens: 16,200 to read at home
		// against 40,000 on the 5090. In seconds that is 27 at home's 600 a
		// second and 25 on the 5090's 1,600, so the faster engine wins.
		{"home's backlog is past 18 seconds, and a cold read on the faster engine takes less time: leaves", fleet(0, 2, 0), "minion", 0.97, 40000, reading{"minion": 15000}, nil, "xpredator"},
		{"home's backlog is past 18 seconds, but still quicker than a cold read elsewhere: stays", fleet(0, 2, 0), "minion", 0.97, 40000, reading{"minion": 12000}, nil, "minion"},
		{"home has more to read first than the whole prompt: leaves", fleet(0, 2, 0), "minion", 0.97, 40000, reading{"minion": 60000}, nil, "xpredator"},
		// Four fifths of the prompt is the line.
		{"holds 80% of the prompt: sticky", fleet(0, 4, 0), "minion", 0.80, 40000, nil, nil, "minion"},
		{"holds 79%: every engine is a candidate, the one with least to read wins", fleet(0, 4, 0), "minion", 0.79, 40000, nil, nil, "minion"},
		{"holds 79%, but has more to read than the prompt's cached part saves", fleet(0, 4, 0), "minion", 0.79, 40000, reading{"minion": 60000}, nil, "xpredator"},
		// A prompt cached nowhere goes round the engines in turn, never-used
		// first, whatever their speed: llm-d's cold spread.
		{"cached nowhere, nothing used yet: the first engine", fleet(0, 0, 0), "", 0, 2000, nil, nil, "xpredator"},
		{"cached nowhere: the engine least recently given a cold prompt", fleet(0, 0, 0), "", 0, 2000, nil, []string{"xpredator", "helion", "minion"}, "xpredator"},
		{"cached nowhere: an engine never given one comes first", fleet(0, 0, 0), "", 0, 2000, nil, []string{"xpredator", "minion"}, "helion"},
		// A new task shares the system prompt with every other, so it is not
		// cold: load decides, in tokens, not requests.
		{"shares a short prefix only: the engine with the fewest tokens to read", fleet(1, 1, 0), "minion", 0.1, 2000, reading{"xpredator": 500, "minion": 90000, "helion": 30000}, nil, "xpredator"},
		{"one request with 90k to read outweighs three with little", fleet(1, 3, 0), "", 0, 2000, reading{"xpredator": 90000, "minion": 3000}, []string{"helion", "minion", "xpredator"}, "helion"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := NewPlacement(nil)
			for name, n := range c.reading {
				p.reading[name] = n
			}
			p.coldOrder = c.coldLRU
			got := p.schedule(c.cands, held(c.target, c.share), c.tokens, "", nil)
			if got[0].Name != c.want {
				t.Errorf("first choice %s, want %s (order %s)", got[0].Name, c.want, order(got))
			}
			if len(got) != len(c.cands) {
				t.Errorf("returned %d candidates of %d: every one must stay available for a retry", len(got), len(c.cands))
			}
		})
	}
}

// What an engine has to read goes up when a request is placed and down when
// it ends, by the same amount; and a conversation's size, once the engine has
// reported it, replaces the guess made from the length of its request.
func TestPlacementTracksWhatEachEngineHasToRead(t *testing.T) {
	p := NewPlacement(nil)
	cands := []mesh.Candidate{
		{Name: "a", Local: true, Slots: 2, PrefillTokS: 1000},
		{Name: "b", Local: true, Slots: 2, PrefillTokS: 1000},
	}
	turn := func(text string) []byte {
		return []byte(`{"model":"m","messages":[{"role":"user","content":"` + text + `"}]}`)
	}
	first := turn(strings.Repeat("the quick brown fox ", 400)) // about 8KB
	c1 := p.Order(cands, "m", first, "")
	home := c1.Candidates[0].Name
	a1 := p.Placed(c1, home)
	if got, want := p.reading[home], int64(len(first)/4); got != want {
		t.Fatalf("after placing a new prompt, %s has %d to read, want all %d of it", home, got, want)
	}
	a1.Finished(2100, 300)
	if got := p.reading[home]; got != 0 {
		t.Fatalf("after it finished, %s still has %d to read", home, got)
	}

	// The next turn of the same conversation: nearly all of it is held at
	// home, so home it goes, and little is left to read.
	second := turn(strings.Repeat("the quick brown fox ", 400) + strings.Repeat(" and a tool's output", 60)) // about 1.2KB more
	c2 := p.Order(cands, "m", second, "")
	if c2.Target != home || c2.Candidates[0].Name != home {
		t.Fatalf("second turn: target %q first %q, want %s", c2.Target, c2.Candidates[0].Name, home)
	}
	// 2,100 prompt tokens and 300 of answer were reported for the first turn.
	// Scaled up by the part of the new prompt that was not there before.
	if c2.tokens < 2400 || c2.tokens > 3200 {
		t.Errorf("second turn sized at %d tokens, want a little over the 2,400 the engine reported", c2.tokens)
	}
	a2 := p.Placed(c2, home)
	if got := p.reading[home]; got <= 0 || got > c2.tokens/4 {
		t.Errorf("home has %d of %d tokens to read for a prompt it mostly holds", got, c2.tokens)
	}
	// Sent elsewhere it would all have to be read.
	other := "b"
	if home == "b" {
		other = "a"
	}
	if got := c2.cost(other); got != c2.tokens {
		t.Errorf("on %s the whole prompt has to be read: got %d of %d", other, got, c2.tokens)
	}
	a2.Finished(2500, 100)
	if got := p.reading[home]; got != 0 {
		t.Errorf("after both finished, %s still has %d to read", home, got)
	}
}

// The earlier rule is still there to be chosen.
func TestHomeSlotPlacementIsSelectable(t *testing.T) {
	cands := []mesh.Candidate{
		{Name: "xpredator", Local: true, Inflight: 0, Slots: 2, PrefillTokS: 1600},
		{Name: "minion", Local: true, Inflight: 4, Slots: 4, PrefillTokS: 600},
	}
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("a conversation ", 600) + `"}]}`)
	for _, c := range []struct {
		name     string
		homeSlot bool
		want     string
	}{
		{"default: home though every slot there is busy", false, "minion"},
		{"home-slot: home is full, so the fewest in flight", true, "xpredator"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := NewPlacement(nil)
			p.HomeSlot = c.homeSlot
			p.Placed(p.Order(cands, "m", body, ""), "minion")
			if got := p.Order(cands, "m", body, "").Candidates[0].Name; got != c.want {
				t.Errorf("first choice %s, want %s", got, c.want)
			}
		})
	}
}

// llm-d's rule as written deals new conversations round the engines in turn
// and keeps each where it started. Run live on 2026-10-06 that put three on
// the two-slot 5090: twelve cold reads of 45,000 to 70,000 tokens in eight
// minutes there, each conversation taking the next one's slot, beside a GPU
// with a slot nobody lived in. An engine is full by who lives on it.
func TestScheduleKeepsConversationsWithinAnEnginesSlots(t *testing.T) {
	fleet := func(x, m, h int64) []mesh.Candidate {
		return []mesh.Candidate{
			{Name: "xpredator", Local: true, Inflight: x, Slots: 2, PrefillTokS: 1600},
			{Name: "minion", Local: true, Inflight: m, Slots: 4, PrefillTokS: 600},
			{Name: "helion", Local: true, Inflight: h, Slots: 1, PrefillTokS: 250},
		}
	}
	for _, c := range []struct {
		name string
		// living is how many other conversations live on each engine.
		living map[string]int
		cands  []mesh.Candidate
		target string
		share  float64
		want   string
	}{
		// A slot free this instant is not room: both of the 5090's
		// conversations are between calls, and both will be back.
		{"a new conversation is kept off an engine whose slots are all lived in, though none is busy",
			map[string]int{"xpredator": 2, "minion": 2}, fleet(0, 1, 0), "", 0, "minion"},
		{"a one-slot engine with someone living on it does not get a second",
			map[string]int{"xpredator": 2, "minion": 3, "helion": 1}, fleet(1, 2, 0), "", 0, "minion"},
		{"every engine is lived in to its slots: llm-d's rule decides, there is nowhere better",
			map[string]int{"xpredator": 2, "minion": 4, "helion": 1}, fleet(1, 2, 0), "", 0, "xpredator"},
		// The run's case, from the side of a conversation already there.
		{"home has one more living there than it has slots, and another engine has room: leaves",
			map[string]int{"xpredator": 2, "minion": 2}, fleet(1, 1, 0), "xpredator", 0.97, "minion"},
		{"home is lived in exactly to its slots, counting this one: stays",
			map[string]int{"xpredator": 1, "minion": 2}, fleet(1, 1, 0), "xpredator", 0.97, "xpredator"},
		{"home is crowded, and so is everywhere else: stays",
			map[string]int{"xpredator": 2, "minion": 4, "helion": 1}, fleet(1, 2, 1), "xpredator", 0.97, "xpredator"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := NewPlacement(nil)
			id := byte(0)
			for engine, n := range c.living {
				for range n {
					id++
					p.homes[[32]byte{id}] = &conversation{leaf: [32]byte{id}, engine: engine, last: p.aff.now()}
				}
			}
			self := &conversation{leaf: [32]byte{200}, engine: c.target, last: p.aff.now()}
			if c.target != "" {
				p.homes[self.leaf] = self
			}
			got := p.schedule(c.cands, held(c.target, c.share), 40000, "", self)
			if got[0].Name != c.want {
				t.Errorf("first choice %s, want %s (order %s)", got[0].Name, c.want, order(got))
			}
		})
	}
	// A conversation that has gone quiet stops counting.
	t.Run("a conversation not heard from for a while no longer lives there", func(t *testing.T) {
		p := NewPlacement(nil)
		old := p.aff.now().Add(-2 * residentFor)
		p.homes[[32]byte{1}] = &conversation{engine: "xpredator", last: old}
		p.homes[[32]byte{2}] = &conversation{engine: "xpredator", last: old}
		if got := p.schedule(fleet(0, 0, 0), nil, 2000, "", nil); got[0].Name != "xpredator" {
			t.Errorf("first choice %s, want xpredator: both of its conversations ended long ago", got[0].Name)
		}
	})
}

// Every SWE-bench task starts with the same system prompt. With one engine
// remembered as holding it, each new task was scored as partly cached there
// and nowhere else, and followed the last request anywhere: replaying llm-d's
// own run through this placement, 13 of 20 new tasks went to the one-slot Mac
// where llm-d had sent 2. A block many conversations share is held by every
// engine that has served any of them, and then it decides nothing.
func TestASharedSystemPromptDoesNotPullNewTasksToOneEngine(t *testing.T) {
	p := NewPlacement(nil)
	p.NoRoomRule = true
	cands := []mesh.Candidate{
		{Name: "xpredator", Local: true, Slots: 2, PrefillTokS: 1600},
		{Name: "minion", Local: true, Slots: 4, PrefillTokS: 600},
		{Name: "helion", Local: true, Slots: 1, PrefillTokS: 250},
	}
	system := strings.Repeat("You are a helpful assistant with tools. ", 60) // about 2.4KB, shared
	task := func(n string) []byte {
		return []byte(`{"model":"m","messages":[{"role":"system","content":"` + system + `"},{"role":"user","content":"task ` + n + ` ` + strings.Repeat("describe the bug in detail ", 400) + `"}]}`)
	}
	// One task has run on each engine, the Mac last.
	for i, name := range []string{"xpredator", "minion", "helion"} {
		c := p.Order(cands, "m", task(string(rune('a'+i))), "")
		attempt := p.Placed(c, name)
		attempt.Finished(0, 0)
	}
	c := p.Order(cands, "m", task("new"), "")
	if len(c.shares) != 3 {
		t.Fatalf("the shared prefix is held by %d engines in placement's view, want all 3: %v", len(c.shares), c.shares)
	}
	if got := c.Candidates[0].Name; got != "xpredator" {
		t.Errorf("a new task went to %s, want xpredator: nothing is in flight, the shared prefix is everywhere, so the mesh's order decides", got)
	}
	// And a conversation still goes back to the one engine that has all of it.
	first := p.Order(cands, "m", task("mine"), "")
	attempt := p.Placed(first, "minion")
	attempt.Finished(3000, 200)
	again := p.Order(cands, "m", task("mine"), "")
	if again.Candidates[0].Name != "minion" || again.Target != "minion" {
		t.Errorf("a continuing conversation went to %s (target %q), want minion", again.Candidates[0].Name, again.Target)
	}
}

// An engine that reloads has nothing cached, whatever was sent to it before.
// The replay of 2026-10-06 started 49 seconds after an earlier run of the same
// conversations, every engine reloaded in between, and placement followed the
// earlier run: 53 moves where runs from an empty memory made 13.
func TestPlacementForgetsAnEngineThatReloaded(t *testing.T) {
	cands := []mesh.Candidate{
		{Name: "xpredator", Local: true, Slots: 2, PrefillTokS: 1600},
		{Name: "minion", Local: true, Slots: 4, PrefillTokS: 600},
	}
	shared := strings.Repeat("You are a helpful assistant with tools. ", 60)
	body := func(task string) []byte {
		return []byte(`{"model":"m","messages":[{"role":"system","content":"` + shared + `"},{"role":"user","content":"` + task + ` ` + strings.Repeat("a long conversation ", 600) + `"}]}`)
	}
	place := func(p *Placement, b []byte, name string) {
		c := p.Order(cands, "m", b, "")
		attempt := p.Placed(c, name)
		attempt.Finished(0, 0)
	}
	for _, c := range []struct {
		name   string
		forget string
		want   map[string]bool // engines still holding any of conversation A
	}{
		{"nothing reloaded: A is on minion, and the shared prompt on both", "", map[string]bool{"minion": true, "xpredator": true}},
		{"minion reloaded: only what xpredator holds is left", "minion", map[string]bool{"xpredator": true}},
		{"xpredator reloaded: A is still minion's", "xpredator", map[string]bool{"minion": true}},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := NewPlacement(nil)
			place(p, body("task A"), "minion")
			place(p, body("task B"), "xpredator")
			if c.forget != "" {
				p.Forget(c.forget)
			}
			got := p.Order(cands, "m", body("task A"), "")
			for _, e := range []string{"minion", "xpredator"} {
				if held := got.shares[e] > 0; held != c.want[e] {
					t.Errorf("%s holding some of A: %v, want %v (shares %v)", e, held, c.want[e], got.shares)
				}
			}
			if c.forget == "minion" {
				if got.shares["xpredator"] >= stickyShare {
					t.Errorf("xpredator holds only the shared prompt, not A: share %.2f", got.shares["xpredator"])
				}
				if got.conv != nil {
					t.Errorf("A no longer lives anywhere, got a home on %s", got.conv.engine)
				}
			}
		})
	}
}
