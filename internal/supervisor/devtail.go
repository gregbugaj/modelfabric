package supervisor

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gregbugaj/modelfabric/internal/devlog"
)

// The engine half of the developer log. Each running engine's own log is
// followed and every line is passed on at debug level, as it was written. The
// lines a person debugging a request actually looks for are read out and said
// again in plain words at info level: which slot the request took and
// whether that slot held its prompt, how far prompt processing has got, how
// fast it read and generated, and when a conversation was dropped from the
// RAM cache.
//
// An engine's log names a slot and its own task number, never the request
// that ModelFabric routed, so these entries carry the model and the instance
// and no trace. They sit between a request's "sent to" and "finished" entries
// by time, which is how LM Studio's developer log places them too.

// tailEvery is how often the logs are read. Prompt processing reports a line
// per batch, a second or so apart on a long prompt, so this keeps the
// progress entries close to live without holding a file open per engine.
const tailEvery = 500 * time.Millisecond

// tailFromStartBelow is the size under which a log first seen is read from
// its beginning, so a model's load messages are in the developer log. A
// larger one belongs to an engine that was running before this node started,
// and replaying it would bury what is happening now.
const tailFromStartBelow = 512 << 10

// maxTailLine bounds one line. llama.cpp logs a request's whole prompt on
// one line at some verbosity levels.
const maxTailLine = 4 << 10

type devTail struct {
	offset int64
	carry  []byte // a line whose end has not been written yet
	seen   bool
	// started is when the engine's own clock read zero, worked out from its
	// log (see when). Zero until a stamped line has been read.
	started time.Time
}

// SetDevLog attaches the node's developer log and starts following every
// running engine's log into it. Call it once, before any load.
func (s *Supervisor) SetDevLog(l *devlog.Log) {
	if l == nil {
		return
	}
	s.dev = l
	go s.tailEngines()
}

func (s *Supervisor) tailEngines() {
	tails := map[string]*devTail{}
	t := time.NewTicker(tailEvery)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
		}
		live := map[string]bool{}
		for _, inst := range s.Instances() {
			if inst.LogPath == "" {
				continue
			}
			live[inst.ID] = true
			tl := tails[inst.ID]
			if tl == nil {
				tl = &devTail{}
				tails[inst.ID] = tl
			}
			lines, written := tl.read(inst.LogPath)
			tl.calibrate(lines, written)
			if !tl.started.IsZero() {
				// Stamped the way llama.cpp stamps: its rate lines can be read.
				s.live.saw(inst.ID)
			}
			now := time.Now()
			for _, line := range lines {
				s.live.line(inst.ID, line, now)
				at := tl.when(line)
				for _, e := range engineEntries(line) {
					e.Time, e.Model, e.Engine = at, inst.Model, inst.ID
					s.dev.Add(e)
				}
			}
		}
		for id := range tails {
			if !live[id] {
				delete(tails, id)
				s.live.forget(id)
			}
		}
	}
}

// read returns the complete lines the log has gained since the last call,
// and when the log was last written, which is when the last of them was.
func (t *devTail) read(path string) (lines []string, written time.Time) {
	f, err := os.Open(path)
	if err != nil {
		return nil, time.Time{}
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, time.Time{}
	}
	written = st.ModTime()
	size := st.Size()
	if !t.seen {
		t.seen = true
		if size > tailFromStartBelow {
			t.offset = size
		}
	}
	if size < t.offset {
		// Shorter than it was: replaced. Read the new one from its start.
		t.offset, t.carry = 0, nil
	}
	if size == t.offset {
		return nil, written
	}
	// A burst larger than this is skipped to its end: a log that gained
	// megabytes in half a second is not being read by anyone line by line.
	const most = 1 << 20
	if size-t.offset > most {
		t.offset, t.carry = size-most, nil
	}
	buf := make([]byte, size-t.offset)
	n, err := f.ReadAt(buf, t.offset)
	if err != nil && err != io.EOF {
		return nil, written
	}
	t.offset += int64(n)
	data := append(t.carry, buf[:n]...)
	for {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimRight(string(data[:i]), "\r")
		data = data[i+1:]
		if strings.TrimSpace(line) == "" {
			continue
		}
		if len(line) > maxTailLine {
			line = line[:maxTailLine] + " ... (cut)"
		}
		lines = append(lines, line)
	}
	t.carry = append([]byte(nil), data...)
	if len(t.carry) > maxTailLine {
		t.carry = nil
	}
	return lines, written
}

// An engine's lines are read up to half a second after it wrote them, and in
// batches, so the time of reading puts a request's "read 1982 prompt tokens"
// after the router's "finished" for the same request. llama.cpp stamps each
// line with the time since it started, so the line's own time is that plus
// when it started.
//
// When it started is not recorded anywhere to the millisecond, but the log
// says: the file's modification time is when its last line was written, so
// that minus the line's stamp is the start, late by however long the engine
// held the line before writing it. The smallest such figure seen is the
// closest. Reading the clock instead of the file's time was tried first and
// put every line a tail interval late: "read 6465 prompt tokens" 270ms after
// the router's "finished" for that request.

// stampOf reads llama.cpp's "minutes.seconds.milliseconds.microseconds" from
// the front of a line.
func stampOf(line string) (time.Duration, bool) {
	m := engineClock.FindStringSubmatch(line)
	if m == nil {
		return 0, false
	}
	n := func(s string) time.Duration { v, _ := strconv.Atoi(s); return time.Duration(v) }
	return n(m[1])*time.Minute + n(m[2])*time.Second + n(m[3])*time.Millisecond + n(m[4])*time.Microsecond, true
}

// calibrate improves the estimate of when the engine started from a batch of
// lines, the last of which was written at now.
func (t *devTail) calibrate(lines []string, now time.Time) {
	if now.IsZero() {
		return
	}
	for i := len(lines) - 1; i >= 0; i-- {
		if stamp, ok := stampOf(lines[i]); ok {
			if est := now.Add(-stamp); t.started.IsZero() || est.Before(t.started) {
				t.started = est
			}
			return
		}
	}
}

// when is the time a line was written: by the engine's own stamp where it
// has one, otherwise zero, which the developer log reads as now.
func (t *devTail) when(line string) time.Time {
	if stamp, ok := stampOf(line); ok && !t.started.IsZero() {
		return t.started.Add(stamp)
	}
	return time.Time{}
}

var (
	engineClock = regexp.MustCompile(`^(\d+)\.(\d\d)\.(\d{3})\.(\d{3}) `)
	// "0.50.989.252 I slot print_timing: id  1 | task 57 | ..."
	engineStamp  = regexp.MustCompile(`^[0-9.]+ ([IWED]) `)
	slotAndTask  = regexp.MustCompile(`id\s+(\d+) \| task (-?\d+) \|`)
	progressLine = regexp.MustCompile(`prompt processing, n_tokens =\s*(\d+), progress = ([0-9.]+)`)
	promptEval   = regexp.MustCompile(`prompt eval time =\s*([0-9.]+) ms /\s*(\d+) tokens .*?([0-9.]+) tokens per second`)
	genEval      = regexp.MustCompile(`\|\s+eval time =\s*([0-9.]+) ms /\s*(\d+) tokens .*?([0-9.]+) tokens per second`)
	slotByPrefix = regexp.MustCompile(`selected slot by LCP similarity, f_sim_best = ([0-9.]+)`)
	cacheEvicted = regexp.MustCompile(`making room for prompt cache entry, removing oldest entry \(size = ([0-9.]+) MiB`)
	draftAccept  = regexp.MustCompile(`draft acceptance = ([0-9.]+) \(\s*(\d+) accepted /\s*(\d+) generated`)
)

// engineEntries turns one line of an engine's log into developer-log entries:
// the line itself at debug level, and where it says something a person looks
// for, that in words at info level or above. Lines from an engine that does
// not write llama.cpp's format come through as the debug entry alone.
func engineEntries(line string) []devlog.Entry {
	raw := devlog.Entry{Level: devlog.Debug, Source: devlog.Engine, Msg: line}
	if m := engineStamp.FindStringSubmatch(line); m != nil && m[1] == "E" {
		// The engine's own errors are not detail: they are usually the line
		// above an exit.
		raw.Level = devlog.Error
	}
	out := []devlog.Entry{raw}

	fields := map[string]any{}
	if m := slotAndTask.FindStringSubmatch(line); m != nil {
		fields["slot"], _ = strconv.Atoi(m[1])
		if task, _ := strconv.Atoi(m[2]); task >= 0 {
			fields["task"] = task
		}
	}
	slot := ""
	if v, ok := fields["slot"]; ok {
		slot = fmt.Sprintf("slot %d: ", v)
	}
	said := func(level, msg string) {
		out = append(out, devlog.Entry{Level: level, Source: devlog.Engine, Msg: msg, Fields: fields})
	}
	switch {
	case progressLine.MatchString(line):
		m := progressLine.FindStringSubmatch(line)
		tokens, _ := strconv.Atoi(m[1])
		p, _ := strconv.ParseFloat(m[2], 64)
		fields["tokens"], fields["progress"] = tokens, p
		said(devlog.Info, fmt.Sprintf("%sprompt processing %d%% (%d tokens read)", slot, int(p*100+0.5), tokens))
	case promptEval.MatchString(line):
		m := promptEval.FindStringSubmatch(line)
		ms, _ := strconv.ParseFloat(m[1], 64)
		tokens, _ := strconv.Atoi(m[2])
		rate, _ := strconv.ParseFloat(m[3], 64)
		fields["tokens"], fields["ms"], fields["tok_s"] = tokens, ms, rate
		said(devlog.Info, fmt.Sprintf("%sread %d prompt tokens in %s (%.0f tok/s)", slot, tokens, seconds(ms), rate))
	case genEval.MatchString(line):
		m := genEval.FindStringSubmatch(line)
		ms, _ := strconv.ParseFloat(m[1], 64)
		tokens, _ := strconv.Atoi(m[2])
		rate, _ := strconv.ParseFloat(m[3], 64)
		fields["tokens"], fields["ms"], fields["tok_s"] = tokens, ms, rate
		said(devlog.Info, fmt.Sprintf("%sgenerated %d tokens in %s (%.1f tok/s)", slot, tokens, seconds(ms), rate))
	case slotByPrefix.MatchString(line):
		m := slotByPrefix.FindStringSubmatch(line)
		sim, _ := strconv.ParseFloat(m[1], 64)
		fields["similarity"] = sim
		said(devlog.Info, fmt.Sprintf("%schosen because it holds %d%% of this prompt", slot, int(sim*100+0.5)))
	case strings.Contains(line, "selected slot by LRU"):
		said(devlog.Info, slot+"chosen as the least recently used; no slot holds this prompt")
	case cacheEvicted.MatchString(line):
		m := cacheEvicted.FindStringSubmatch(line)
		mib, _ := strconv.ParseFloat(m[1], 64)
		fields["mib"] = mib
		said(devlog.Warn, fmt.Sprintf("RAM cache full: dropped the oldest saved conversation (%.0f MiB). Its next request reads its whole prompt again", mib))
	case strings.Contains(line, "exceeds cache size limit"):
		said(devlog.Warn, "a conversation was too large to save in the RAM cache. Raise -cache-ram if the machine has the memory")
	case draftAccept.MatchString(line):
		m := draftAccept.FindStringSubmatch(line)
		rate, _ := strconv.ParseFloat(m[1], 64)
		accepted, _ := strconv.Atoi(m[2])
		drafted, _ := strconv.Atoi(m[3])
		if drafted > 0 {
			fields["accepted"], fields["drafted"] = accepted, drafted
			said(devlog.Info, fmt.Sprintf("%sspeculative decoding: %d of %d drafted tokens accepted (%d%%)", slot, accepted, drafted, int(rate*100+0.5)))
		}
	}
	return out
}

// seconds writes a duration given in milliseconds the way a person says it.
func seconds(ms float64) string {
	if ms < 1000 {
		return fmt.Sprintf("%.0f ms", ms)
	}
	return fmt.Sprintf("%.1f s", ms/1000)
}

// said writes a lifecycle entry for an instance. s.dev may be nil: a nil
// *devlog.Log drops what it is given, so there is no check here.
func (s *Supervisor) said(level, model, instance, msg string, fields map[string]any) {
	s.dev.Add(devlog.Entry{Level: level, Source: devlog.Node, Model: model, Engine: instance, Msg: msg, Fields: fields})
}
