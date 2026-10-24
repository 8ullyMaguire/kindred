package corpus

// Document frequency over the words of `works.summary` (#52).
//
// The mirror has 112,136 summaries averaging 424 characters: the only prose in it, and the
// only place a writing-style or subject signal could come from. The recommenders are
// tag-level, and tags are a human's summary of a human's summary -- "Angst" says nothing
// about whether two works read alike.
//
// The table is per-TERM document frequency, the same shape as the tag table beside it
// (`DocFreq`), because both exist for one reason: a word on every document carries no
// evidence, and a word on one carries none either. Only terms with 3 <= df <= 20% of the
// corpus are written, which is the band where a shared word means something.
//
// The build is offline (`kindred ingest`) and deterministic: vocabulary is sorted, so two
// ingests of the same corpus produce byte-identical files and a diff between mirrors means a
// diff in the data.

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
)

// SummaryDocFreq is the loaded table.
type SummaryDocFreq struct {
	// Total is the number of documents (summaries) counted.
	Total float64
	// ByHash maps a term's 64-bit hash to the number of documents carrying it.
	//
	// A HASH, not the term: the table is read for its WEIGHTS, never displayed, and storing
	// 120k strings would cost more memory than the scores are worth. The cost is that two
	// different words can collide -- at 64 bits and 120k terms the birthday estimate is a
	// 1-in-3-trillion chance, and a collision merges two weights rather than corrupting the
	// file.
	ByHash map[uint64]float64
}

// SummaryDocFreqPath is where the table lives: beside the corpus, as a side file.
func SummaryDocFreqPath(corpusPath string) string { return corpusPath + ".summarydf" }

// summaryMinDF and summaryMaxShare bound the vocabulary to the band where a shared word is
// evidence: on one or two summaries it relates at most one other work, and on more than a
// fifth of the corpus it is boilerplate ("chapter", "notes", "warnings").
const (
	summaryMinDF    = 3
	summaryMaxShare = 0.20
	summaryMinLen   = 4
	summaryMaxLen   = 24
)

// SummaryTokens splits a summary into candidate terms.
//
// Lowercase, letters only (an apostrophe or a digit ends a word: AO3 summaries carry "don't",
// "5+1" and "PWP-adjacent"), length 4-24, no stopwords. No stemming: "feels" and "feeling"
// staying apart costs recall and keeps the table honest -- a hand-rolled stemmer here would
// be a silent edit to 112k documents.
func SummaryTokens(s string) []string {
	if s == "" {
		return nil
	}
	var (
		out  []string
		buf  []rune
		emit = func() {
			if len(buf) < summaryMinLen || len(buf) > summaryMaxLen {
				buf = buf[:0]
				return
			}
			w := string(buf)
			buf = buf[:0]
			if _, stop := summaryStopwords[w]; stop {
				return
			}
			out = append(out, w)
		}
	)
	for _, r := range s {
		if unicode.IsLetter(r) {
			buf = append(buf, unicode.ToLower(r))
			continue
		}
		emit()
	}
	emit()
	return out
}

// SummaryHash is the table key for a term.
func SummaryHash(term string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(term))
	return h.Sum64()
}

// summaryStopwords is short and deliberately small: idf already discounts a common word, and
// a big list is a second, unmeasured opinion about what English means. These are the words
// that carry no subject at all, plus the AO3 boilerplate that appears in a fifth of summaries
// and would otherwise fill the band below.
var summaryStopwords = func() map[string]struct{} {
	words := strings.Fields(`
		about after again against almost along already also although always among another
		anything around because been before being below between both bring cannot could
		didnt does doesn doing done down during each else even ever every everything
		except feel feels felt find first found from further gave give given goes going
		gone good great had have having here herself himself however into itself just
		keep kept know known last later least less like likely little long look looking
		made make makes many maybe mean means might more most much must myself need
		never next none nothing often once only other others otherwise ought over perhaps
		please quite rather really right said same says seem seems seen several shall
		should since some something sometimes soon still such take taken takes than
		that their theirs them themselves then there these they thing things think
		this those though through thus together took toward towards under until upon
		used uses very want wanted wants well went were what whatever when where whether
		which while whom whose will with within without would your yours yourself
	`)
	out := make(map[string]struct{}, len(words))
	for _, w := range words {
		out[w] = struct{}{}
	}
	// AO3 summary boilerplate: these are on a large share of works for reasons that have
	// nothing to do with what the work is.
	for _, w := range strings.Fields(`
		chapter chapters summary spoilers warning warnings work story fic fics
		notes note author update updates series part complete ongoing
	`) {
		out[w] = struct{}{}
	}
	return out
}()

// BuildSummaryDocFreq scans the corpus and writes the table.
//
// It returns the number of terms kept and the number of summaries counted. Progress is
// silent: it runs inside `kindred ingest`, which prints its own line.
func BuildSummaryDocFreq(ctx context.Context, db *sql.DB, corpusPath string) (terms int, docs int, err error) {
	if db == nil {
		return 0, 0, fmt.Errorf("summary docfreq: no corpus database")
	}
	rows, err := db.QueryContext(ctx,
		`SELECT summary FROM works WHERE summary IS NOT NULL AND summary <> ''`)
	if err != nil {
		return 0, 0, fmt.Errorf("summary docfreq: %w", err)
	}
	defer rows.Close()

	counts := make(map[string]int, 1<<20)
	docs = 0
	for rows.Next() {
		var summary string
		if err := rows.Scan(&summary); err != nil {
			return 0, 0, fmt.Errorf("summary docfreq: scan: %w", err)
		}
		docs++
		seen := make(map[string]struct{}, 64)
		for _, tok := range SummaryTokens(summary) {
			// A term counts ONCE per document, however often the summary repeats it: this is
			// DOCUMENT frequency, and counting occurrences would make a long rambling summary
			// look like a common word.
			if _, dup := seen[tok]; dup {
				continue
			}
			seen[tok] = struct{}{}
			counts[tok]++
		}
	}
	if err := rows.Err(); err != nil {
		return 0, 0, fmt.Errorf("summary docfreq: rows: %w", err)
	}
	if docs == 0 {
		return 0, 0, fmt.Errorf("summary docfreq: no summaries in this corpus")
	}

	maxDF := int(float64(docs) * summaryMaxShare)
	kept := make([]string, 0, len(counts)/4)
	for term, n := range counts {
		if n < summaryMinDF || n > maxDF {
			continue
		}
		kept = append(kept, term)
	}
	sort.Strings(kept)

	terms = len(kept)
	tmp := SummaryDocFreqPath(corpusPath) + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, 0, fmt.Errorf("summary docfreq: create: %w", err)
	}
	w := bufio.NewWriterSize(f, 1<<20)
	write := func(v any) error { return binary.Write(w, binary.LittleEndian, v) }
	if err := write(uint32(0x534d4446)); err != nil { // "SMDF"
		f.Close()
		return 0, 0, fmt.Errorf("summary docfreq: write: %w", err)
	}
	if err := write(float64(docs)); err != nil {
		f.Close()
		return 0, 0, fmt.Errorf("summary docfreq: write: %w", err)
	}
	if err := write(uint32(terms)); err != nil {
		f.Close()
		return 0, 0, fmt.Errorf("summary docfreq: write: %w", err)
	}
	for _, term := range kept {
		if err := write(SummaryHash(term)); err != nil {
			f.Close()
			return 0, 0, fmt.Errorf("summary docfreq: write: %w", err)
		}
		if err := write(float64(counts[term])); err != nil {
			f.Close()
			return 0, 0, fmt.Errorf("summary docfreq: write: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return 0, 0, fmt.Errorf("summary docfreq: flush: %w", err)
	}
	if err := f.Close(); err != nil {
		return 0, 0, fmt.Errorf("summary docfreq: close: %w", err)
	}
	if err := os.Rename(tmp, SummaryDocFreqPath(corpusPath)); err != nil {
		return 0, 0, fmt.Errorf("summary docfreq: rename: %w", err)
	}
	return terms, docs, nil
}

// LoadSummaryDocFreq reads the table. os.ErrNotExist means this mirror has never been
// ingested; the caller reports that as a degraded signal rather than an error.
func LoadSummaryDocFreq(corpusPath string) (*SummaryDocFreq, error) {
	f, err := os.Open(SummaryDocFreqPath(corpusPath))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var magic uint32
	if err := binary.Read(f, binary.LittleEndian, &magic); err != nil {
		return nil, fmt.Errorf("summary docfreq: %s: header: %w", filepath.Base(corpusPath), err)
	}
	if magic != 0x534d4446 {
		return nil, fmt.Errorf("summary docfreq: %s is not a summary table (magic %08x)",
			SummaryDocFreqPath(corpusPath), magic)
	}
	var total float64
	var n uint32
	if err := binary.Read(f, binary.LittleEndian, &total); err != nil {
		return nil, fmt.Errorf("summary docfreq: total: %w", err)
	}
	if err := binary.Read(f, binary.LittleEndian, &n); err != nil {
		return nil, fmt.Errorf("summary docfreq: count: %w", err)
	}
	out := &SummaryDocFreq{Total: total, ByHash: make(map[uint64]float64, n)}
	var key uint64
	var df float64
	for i := uint32(0); i < n; i++ {
		if err := binary.Read(f, binary.LittleEndian, &key); err != nil {
			return nil, fmt.Errorf("summary docfreq: key %d: %w", i, err)
		}
		if err := binary.Read(f, binary.LittleEndian, &df); err != nil {
			return nil, fmt.Errorf("summary docfreq: df %d: %w", i, err)
		}
		out.ByHash[key] = df
	}
	return out, nil
}

// IDF is the weight of a term, or 0 when the term is outside the measured band.
//
// A term absent from the table is either on fewer than three summaries (it relates almost
// nothing) or on more than a fifth (it is boilerplate). Both contribute nothing, and 0 says
// so without a second rule.
func (d *SummaryDocFreq) IDF(term string) float64 {
	if d == nil || d.Total <= 1 {
		return 0
	}
	df, ok := d.ByHash[SummaryHash(term)]
	if !ok || df <= 0 {
		return 0
	}
	return math.Log(d.Total / df)
}
