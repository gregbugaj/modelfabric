package router

// Attempt owns one dispatch's reservations. Its resident reference survives
// overlapping continuations that replace the prompt leaf. Fields are guarded
// by placement.mu; methods on a nil attempt are no-ops.
type Attempt struct {
	placement                      *Placement
	choice                         Choice
	name                           string
	conv                           *conversation
	sequence                       uint64
	reading                        int64
	prefilled, finished, forgotten bool
}

// Prefilled releases prompt load when generation starts. The request still
// occupies its engine slot and resident until Finished or Failed.
func (a *Attempt) Prefilled() {
	if a == nil {
		return
	}
	p := a.placement
	p.mu.Lock()
	defer p.mu.Unlock()
	if a.finished || a.prefilled || a.forgotten {
		return
	}
	a.releaseReading()
	a.prefilled = true
	p.aff.record(a.choice.blocks, a.name)
}

// releaseReading is idempotent so stream start and completion cannot subtract
// another request's load. placement.mu is held.
func (a *Attempt) releaseReading() {
	p := a.placement
	p.reading[a.name] -= a.reading
	a.reading = 0
}

// Finished confirms a successful response, including non-streaming engines
// that cannot signal prefill completion separately.
func (a *Attempt) Finished(promptTokens, completionTokens int64) {
	a.finish(true, promptTokens, completionTokens)
}

// Failed removes only this attempt's speculative cache hint. Previously
// confirmed cache and other requests still filling it are left intact.
func (a *Attempt) Failed() { a.finish(false, 0, 0) }

func (a *Attempt) finish(success bool, promptTokens, completionTokens int64) {
	if a == nil {
		return
	}
	p := a.placement
	p.mu.Lock()
	defer p.mu.Unlock()
	if a.finished {
		return
	}
	a.finished = true
	a.releaseReading()
	delete(p.attempts, a)
	if !a.forgotten && (success || a.prefilled) {
		p.aff.record(a.choice.blocks, a.name)
		if promptTokens > 0 {
			p.aff.setSize(a.choice.blocks[len(a.choice.blocks)-1], promptTokens+completionTokens)
		}
	}
	if v := a.conv; v != nil {
		v.inflight--
		if !a.forgotten && (success || a.prefilled) {
			// A slower earlier turn must not move the resident back to an
			// obsolete leaf after its extension has already completed.
			if a.sequence > v.confirmedSequence {
				v.confirmedEngine, v.confirmedLeaf = a.name, a.choice.blocks[len(a.choice.blocks)-1]
				v.confirmedSequence = a.sequence
			}
			v.last = p.aff.now()
		}
		if v.inflight == 0 && p.homes[v.leaf] == v {
			delete(p.homes, v.leaf)
			if v.confirmedEngine != "" {
				v.engine, v.leaf = v.confirmedEngine, v.confirmedLeaf
				p.homes[v.leaf] = v
			}
		}
	}
}
