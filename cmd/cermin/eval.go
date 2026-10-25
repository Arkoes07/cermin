package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/arkoes07/cermin/internal/cermin"
	"github.com/arkoes07/cermin/internal/claude"
	"github.com/arkoes07/cermin/internal/sqlite"
)

// minPrecision is the bar a method must pass: at least 8 in 10 of the pairs it
// keeps are wanted. Recall is reported, but isn't part of the bar for now
// (#29).
const minPrecision = 0.8

// judgeThresholds are the confidences at which J is measured, lowest first.
var judgeThresholds = []float64{0.5, 0.6, 0.7, 0.8}

// judgeFunc states how likely each shortlisted past Thought, given by its
// text, is to be strongly related to the new Thought's text, one confidence
// per shortlisted Thought, in order, as claude.Judge's JudgeShortlist does.
type judgeFunc func(ctx context.Context, text string, shortlist []string) ([]float64, error)

// evalResurfacing measures each way of deciding which shortlisted past
// Thoughts come back in a Resurfacing against your labels in the CSV at path,
// written by cermin pairs, and prints the numbers.
func evalResurfacing(ctx context.Context, path string) error {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		return errors.New("missing ANTHROPIC_API_KEY")
	}
	model := cmp.Or(os.Getenv("CERMIN_JUDGE_MODEL"), claude.DefaultJudgeModel)

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	db, err := sqlite.Open(ctx, cmp.Or(os.Getenv("CERMIN_DB_PATH"), defaultDBPath))
	if err != nil {
		return err
	}
	pairs, err := preparePairs(ctx, f, db, claude.NewJudge(apiKey, model).JudgeShortlist)
	if err := errors.Join(err, db.Close()); err != nil {
		return fmt.Errorf("evaluating %s: %w", path, err)
	}

	return renderEvaluation(os.Stdout, model, evaluate(pairs))
}

// labelledPair is a pair from the CSV, labelled wanted or not, with what each
// method decides on.
type labelledPair struct {
	score                 float64
	rank                  int
	thoughtID, pastID     int64
	thoughtText, pastText string
	isHeldOut, isWanted   bool
	// From the database: whether the two Thoughts' current Classifications
	// share a Kind.
	sharesKind bool
	// The Judge's confidence that the two are strongly related.
	confidence float64
}

// preparePairs reads the labelled pairs in the CSV from r, compares the
// current Classifications of their Thoughts in db, and asks judge about each
// Thought's shortlist.
func preparePairs(ctx context.Context, r io.Reader, db *sqlite.DB, judge judgeFunc) ([]labelledPair, error) {
	pairs, err := readLabelledPairs(r)
	if err != nil {
		return nil, err
	}
	if err := compareClassifications(ctx, db, pairs); err != nil {
		return nil, err
	}
	if err := judgePairs(ctx, judge, pairs); err != nil {
		return nil, err
	}
	return pairs, nil
}

// readLabelledPairs reads the pairs in a CSV that cermin pairs wrote, every
// one labelled y or n.
func readLabelledPairs(r io.Reader) ([]labelledPair, error) {
	rows, err := readRows(
		r,
		"score",
		"rank",
		"thought_id",
		"past_id",
		"thought_text",
		"past_text",
		"held_out",
		"wanted",
	)
	if err != nil {
		return nil, err
	}
	pairs := make([]labelledPair, 0, len(rows))
	for _, row := range rows {
		p := labelledPair{thoughtText: row["thought_text"], pastText: row["past_text"]}
		var errScore, errRank, errThought, errPast, errHeldOut, errWanted error
		p.score, errScore = strconv.ParseFloat(row["score"], 64)
		p.rank, errRank = strconv.Atoi(row["rank"])
		p.thoughtID, errThought = strconv.ParseInt(row["thought_id"], 10, 64)
		p.pastID, errPast = strconv.ParseInt(row["past_id"], 10, 64)
		p.isHeldOut, errHeldOut = parseYesNo("held_out", row["held_out"])
		p.isWanted, errWanted = parseYesNo("wanted", row["wanted"])
		if err := errors.Join(errScore, errRank, errThought, errPast, errHeldOut, errWanted); err != nil {
			return nil, fmt.Errorf("pair of thoughts %s and %s: %w", row["thought_id"], row["past_id"], err)
		}
		pairs = append(pairs, p)
	}
	return pairs, nil
}

// parseYesNo parses the value of column, y or n in any case, as true or false.
func parseYesNo(column, value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "y":
		return true, nil
	case "n":
		return false, nil
	}
	return false, fmt.Errorf("%s is %q, not y or n", column, value)
}

// compareClassifications fills in whether the two Thoughts of each pair share
// a Kind, by their current Classifications in db. It fails for a
// Thought db doesn't have, as when CERMIN_DB_PATH isn't the copy the CSV came
// from.
func compareClassifications(ctx context.Context, db *sqlite.DB, pairs []labelledPair) error {
	current := map[int64]cermin.Classification{}
	for i := range pairs {
		for _, id := range []int64{pairs[i].thoughtID, pairs[i].pastID} {
			if _, isRead := current[id]; isRead {
				continue
			}
			_, exists, err := db.GetThoughtByID(ctx, id)
			if err != nil {
				return err
			}
			if !exists {
				return fmt.Errorf("thought %d is not in the database", id)
			}
			// The zero Classification if it has none.
			if current[id], _, err = db.GetCurrentClassificationByThoughtID(ctx, id); err != nil {
				return err
			}
		}

		thought, past := current[pairs[i].thoughtID], current[pairs[i].pastID]
		pairs[i].sharesKind = thought.Kind != "" && thought.Kind == past.Kind
	}
	return nil
}

// judgePairs fills in the Judge's confidence for each pair, with one judge
// call per Thought, for its whole shortlist in rank order.
func judgePairs(ctx context.Context, judge judgeFunc, pairs []labelledPair) error {
	shortlists := map[int64][]*labelledPair{}
	thoughtIDs := []int64{} // in the order of the CSV
	for i := range pairs {
		id := pairs[i].thoughtID
		if _, isListed := shortlists[id]; !isListed {
			thoughtIDs = append(thoughtIDs, id)
		}
		shortlists[id] = append(shortlists[id], &pairs[i])
	}

	for _, id := range thoughtIDs {
		shortlist := shortlists[id]
		slices.SortStableFunc(shortlist, func(a, b *labelledPair) int { return cmp.Compare(a.rank, b.rank) })
		texts := make([]string, 0, len(shortlist))
		for _, p := range shortlist {
			texts = append(texts, p.pastText)
		}
		confidences, err := judge(ctx, shortlist[0].thoughtText, texts)
		if err != nil {
			return fmt.Errorf("judging the shortlist of thought %d: %w", id, err)
		}
		for i, p := range shortlist {
			p.confidence = confidences[i]
		}
	}
	return nil
}

// evaluation is how each method did on the labelled pairs.
type evaluation struct {
	lines                  []line // in the order they print
	kindsAll, kindsHeldOut kindOverlap
}

// line is how one method did on one set of pairs: all of them or the held-out
// ones.
type line struct {
	method, rows string
	tally
}

// tally is how one method did on a set of pairs.
type tally struct {
	kept       int     // pairs kept
	wantedKept int     // wanted pairs kept
	precision  float64 // the share of the pairs kept that are wanted; 0 if none is kept
	recall     float64 // the share of the wanted pairs that are kept; 0 if none is wanted
	thoughts   int     // Thoughts that would get a Resurfacing
}

// passes reports whether t passes the bar.
func (t tally) passes() bool {
	return t.precision >= minPrecision
}

// kindOverlap counts the pairs whose two Thoughts share a current Kind, among
// the wanted pairs and among the others.
type kindOverlap struct {
	wantedShared, wanted, unwantedShared, unwanted int
}

// evaluate measures C and J on pairs, all of them and the held-out ones. J is
// judged on all pairs, since its prompt was written blind.
func evaluate(pairs []labelledPair) evaluation {
	heldOut := slices.DeleteFunc(slices.Clone(pairs), func(p labelledPair) bool { return !p.isHeldOut })
	var ev evaluation
	ev.addCosine("all", pairs)
	ev.addCosine("held-out", heldOut)
	for _, threshold := range judgeThresholds {
		isKept := func(p labelledPair) bool { return p.confidence >= threshold }
		ev.addMethod(fmt.Sprintf("J at %.1f", threshold), pairs, heldOut, isKept)
	}
	ev.kindsAll, ev.kindsHeldOut = countKindOverlap(pairs), countKindOverlap(heldOut)
	return ev
}

// addMethod adds the lines of method, which keeps the pairs isKept reports, on
// all pairs and on the held-out ones.
func (ev *evaluation) addMethod(method string, all, heldOut []labelledPair, isKept func(labelledPair) bool) {
	ev.lines = append(ev.lines, line{method: method, rows: "all", tally: tallyPairs(all, isKept)})
	ev.lines = append(ev.lines, line{method: method, rows: "held-out", tally: tallyPairs(heldOut, isKept)})
}

// addCosine adds the line of C on pairs, the set named rows: at the lowest
// cut-off that passes the bar, as J is read at the lowest threshold that
// passes, or if none passes, at the one with the highest precision, the
// lowest on a tie. Every pair's score is a cut-off to try.
func (ev *evaluation) addCosine(rows string, pairs []labelledPair) {
	best := line{method: "C (no pairs)", rows: rows}
	var bestCutoff float64
	for i, cutoff := range pairs {
		t := tallyPairs(pairs, func(p labelledPair) bool { return p.score >= cutoff.score })
		isLower := cutoff.score < bestCutoff
		isBetter := t.passes() && (!best.passes() || isLower)
		if !t.passes() && !best.passes() {
			isBetter = t.precision > best.precision || (t.precision == best.precision && isLower)
		}
		if i == 0 || isBetter {
			best.tally, bestCutoff = t, cutoff.score
			best.method = fmt.Sprintf("C at %.4f", cutoff.score)
		}
	}
	ev.lines = append(ev.lines, best)
}

// tallyPairs counts how the method that keeps the pairs isKept reports does
// on pairs.
func tallyPairs(pairs []labelledPair, isKept func(labelledPair) bool) tally {
	var t tally
	wanted := 0
	thoughts := map[int64]bool{}
	for _, p := range pairs {
		if p.isWanted {
			wanted++
		}
		if !isKept(p) {
			continue
		}
		t.kept++
		thoughts[p.thoughtID] = true
		if p.isWanted {
			t.wantedKept++
		}
	}

	t.thoughts = len(thoughts)
	if t.kept > 0 {
		t.precision = float64(t.wantedKept) / float64(t.kept)
	}
	if wanted > 0 {
		t.recall = float64(t.wantedKept) / float64(wanted)
	}
	return t
}

// countKindOverlap counts the pairs in pairs whose two Thoughts share a
// current Kind.
func countKindOverlap(pairs []labelledPair) kindOverlap {
	var k kindOverlap
	for _, p := range pairs {
		if p.isWanted {
			k.wanted++
			if p.sharesKind {
				k.wantedShared++
			}
			continue
		}
		k.unwanted++
		if p.sharesKind {
			k.unwantedShared++
		}
	}
	return k
}

// renderEvaluation writes ev to w as a table, numbers only, with J asking
// judgeModel.
func renderEvaluation(w io.Writer, judgeModel string, ev evaluation) error {
	fmt.Fprintf(w, "Judge: %s, prompt %s\n\n", judgeModel, claude.JudgePromptVersion)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "method\trows\tkept\twanted kept\tprecision\trecall\tThoughts\tpasses")
	for _, l := range ev.lines {
		passes := "no"
		if l.passes() {
			passes = "yes"
		}
		fmt.Fprintf(
			tw,
			"%s\t%s\t%d\t%d\t%.2f\t%.2f\t%d\t%s\n",
			l.method,
			l.rows,
			l.kept,
			l.wantedKept,
			l.precision,
			l.recall,
			l.thoughts,
			passes,
		)
	}
	if err := tw.Flush(); err != nil {
		return err
	}

	for _, set := range []struct {
		rows  string
		kinds kindOverlap
	}{{rows: "all", kinds: ev.kindsAll}, {rows: "held-out", kinds: ev.kindsHeldOut}} {
		fmt.Fprintf(
			w,
			"\nSharing a Kind, %s pairs: %d of %d wanted, %d of %d unwanted",
			set.rows,
			set.kinds.wantedShared,
			set.kinds.wanted,
			set.kinds.unwantedShared,
			set.kinds.unwanted,
		)
	}
	_, err := fmt.Fprintln(w)
	return err
}
