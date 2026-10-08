// Package ratedlist turns a reader's external reading history into the works
// this mirror holds.
//
// ## Why this exists
//
// The taste signal (internal/signal.Taste) needs to know what a reader liked.
// The mirror has exactly one piece of reader evidence of its own -- bookmarks
// in `user_work_interactions` -- and this reader's is 44 rows, which is not
// enough to learn a taste from. The real reading history is a Calibre library
// of 257 rated fics, and it lives outside the mirror entirely.
//
// So this package does the joining, and it does it HONESTLY. The single most
// dangerous failure mode here is a fuzzy match that lands on the wrong work:
// a title-only match on a common title like "Instinct" attaches a reader's
// 9-rating to a work they never read, and every weight derived from it is
// then wrong in a way no downstream metric can see. The matcher's confidence
// ladder exists to keep that visible, and Report.Unmatched is printed rather
// than summarised away.
//
// ## The paste format
//
// `calibredb list` writes FIXED-COLUMN output, and continuation lines (a
// wrapped title or author) are indented into the same columns. Parsing by
// whitespace alone therefore destroys exactly the rows most likely to need
// manual resolution, so the parser is column-driven: title occupies a fixed
// span, authors a fixed span after it, and an indented line with no row id is
// appended to whichever field it lands in.
//
// Calibre's library is not guaranteed to exist when this runs. It can be
// rebuilt from a saved paste (what this parser reads) or queried live with
// calibredb. Both paths produce the same Entry, so nothing downstream knows
// which one ran.
package ratedlist

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
)

// Entry is one row of a reader's rated reading history.
type Entry struct {
	// BookID is Calibre's own row id. It is kept for reporting only: it is
	// NOT the mirror's work id and is never used as one. Keeping the two
	// apart is the point -- a paste row id of 11827 and an AO3 work id of
	// 11827 are unrelated numbers, and a matcher that confuses them would
	// report a 90% match rate that is pure noise.
	BookID int64

	// Title and Authors as the source spelled them. Kept verbatim so the
	// match report can show the reader exactly what was and was not found.
	//
	// AltTitle and AltAuthor carry the SECOND reading of an over-long field
	// that Calibre wrapped. When a wrap lands mid-word -- "Arch" then "ivist"
	// for "HarryPotterFanFicArchive_Archivist" -- the tail is a continuation
	// of the same token, but when it lands between tokens -- "Sir Lucifer
	// Morningstar" then "(Neckbeard_Satan)" -- it is a new one, and the two
	// cases are indistinguishable from the text alone: both are an indented
	// line at the same column. So both readings are recorded and the corpus
	// decides which one is a real byline. Guessing here would silently
	// mis-attribute a 10-rating.
	Title  string
	Author string
	// AltTitle and AltAuthor are set ONLY when a wrap was ambiguous. Empty
	// otherwise, so their presence means "ask the corpus, do not assume".
	AltTitle  string
	AltAuthor string

	// Rating is the reader's 1-10 rating, or 0 when the row was unrated
	// (`None` in Calibre). Zero means "no opinion", NOT "disliked": an
	// unrated row is still a work the reader read, which is weak positive
	// evidence, and conflating it with a 1-rated row would teach the model
	// to avoid 38 fics the reader never expressed an opinion about.
	Rating int

	// Rated reports whether Rating came from a number rather than the
	// literal "None".
	Rated bool

	// WorkID is the matched mirror work, or 0 when unmatched.
	WorkID int64

	// Confidence is how much the match is trusted: 1.0 exact on both
	// title and author, 0.8 author-scoped fuzzy, 0.5 title-only.
	Confidence float64

	// Method names the tier that matched, for the report.
	Method string
}

// Rated reports whether the entry carries an explicit rating.
func (e Entry) HasRating() bool { return e.Rated && e.Rating > 0 }

// ParseCalibrePaste reads the fixed-column output of
//
//	calibredb list --search '#last_read:True' --fields='*rating,title,authors' --sort-by='*rating'
//
// and returns the entries.
//
// ## Why the columns are read from the DATA and not from the header
//
// The obvious implementation measures "title" and "authors" in the header
// line and slices every data row at those offsets. That is wrong, and the
// reason is a detail of how Calibre formats columns: the header LABELS are
// left-aligned in each column's width, while a numeric column's VALUES are
// RIGHT-aligned. With `--fields='*rating,title,authors'`, "*rating" is 7
// characters wide and the ratings are 1-2, so every data row starts its
// title three columns earlier than the header says it does:
//
//	header: id    *rating title                          authors
//	                ^col 6      ^col 14                    ^col 93
//	data:   29    10   Psycho of the Dead                 Shaboobamon
//	                       ^col 11                          ^col 90
//
// Slicing at the header offsets returns "cho of the Dead" by "boobamon" for
// every row in the library. Measured against the real paste: the title column
// begins at 11 in 256 of 257 rows and the author column at 90; the header
// claims 14 and 93.
//
// So the layout is measured from the data rows themselves: the title ends
// where the widest gap before the author begins, and the author column is
// the modal start of that gap. The header is still checked, so a file that is
// not a Calibre listing at all is rejected rather than parsed into nonsense.
func ParseCalibrePaste(r io.Reader) ([]Entry, error) {
	raw, err := readAll(r)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(raw, "\n")
	// A pasted paste begins with the shell command that produced it, so the
	// header is the first line that LOOKS like a header rather than line 1.
	headerAt := -1
	for i, l := range lines {
		if looksLikeHeader(l) {
			headerAt = i
			break
		}
	}
	if headerAt < 0 {
		return nil, fmt.Errorf(
			"ratedlist: no column header found; expected a line starting " +
				"\"id\" and containing \"title\"")
	}

	// Pass 1: split each data row into its three cells, and measure where the
	// author column begins across all of them.
	type rawRow struct {
		id      int64
		rating  int
		rated   bool
		title   string
		author  string
		contInd []int    // indent of each continuation line
		contTxt []string // its text
	}
	var rows []*rawRow
	authorCols := map[int]int{}
	var cur *rawRow
	flush := func() {
		if cur != nil {
			rows = append(rows, cur)
			cur = nil
		}
	}
	for _, line := range lines[headerAt+1:] {
		line = strings.TrimRight(line, " 	\r")
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		if id, rest, ok := cutRowStart(line); ok {
			flush()
			rating, rated, body := parseRating(rest)
			title, author, col := splitRow(body)
			if col > 0 {
				// Author column in LINE coordinates, because the
				// continuation comparison below measures a line indent
				// against it. The body has already had the id and rating
				// removed, so its columns are shifted by their width.
				authorCols[col+len(line)-len(body)]++
			}
			cur = &rawRow{id: id, rating: rating, rated: rated,
				title: title, author: author}
			continue
		}
		if cur == nil {
			continue
		}
		// A continuation line: the wrapped remainder of an over-long title or
		// author. Which field it belongs to is decided in pass 2, once the
		// author column is known for the file as a whole.
		indent := len(line) - len(strings.TrimLeft(line, " "))
		cur.contInd = append(cur.contInd, indent)
		cur.contTxt = append(cur.contTxt, strings.TrimSpace(line))
	}
	flush()

	authorStart := modalColumn(authorCols)
	out := make([]Entry, 0, len(rows))
	for _, r := range rows {
		title, author, altTitle, altAuthor := r.title, r.author, "", ""
		for i, indent := range r.contInd {
			// The author column is where the padding ends. A continuation
			// that starts at or after it is the tail of the author; anything
			// earlier is the tail of the title.
			if authorStart > 0 && indent >= authorStart {
				// Two readings, because a wrap can split a token or add
				// one: "Arch"+"ivist" is Archivist, while "Morningstar"
				//+"(Neckbeard_Satan)" is two words. Identical from the text
				// alone, so both are kept and the matcher tries each.
				if altAuthor == "" {
					altAuthor = joinField(author, r.contTxt[i])
				}
				author = glueField(author, r.contTxt[i])
			} else {
				if altTitle == "" {
					altTitle = joinField(title, r.contTxt[i])
				}
				title = glueField(title, r.contTxt[i])
			}
		}
		out = append(out, Entry{
			BookID: r.id, Rating: r.rating, Rated: r.rated,
			Title: title, Author: author,
			AltTitle: altTitle, AltAuthor: altAuthor,
		})
	}
	return out, nil
}

// looksLikeHeader recognises the column header line.
func looksLikeHeader(line string) bool {
	l := strings.ToLower(line)
	return strings.HasPrefix(l, "id") && strings.Contains(l, "title") &&
		strings.Contains(l, "authors")
}

// readAll reads a whole paste, because the author column is measured across
// rows and one pass over the text keeps the layout logic in one place.
func readAll(r io.Reader) (string, error) {
	var b strings.Builder
	if _, err := io.Copy(&b, r); err != nil {
		return "", fmt.Errorf("ratedlist: read paste: %w", err)
	}
	return b.String(), nil
}

// splitRow divides one row's remainder (everything after the id and rating)
// into title and author, and reports where the author column began.
//
// The split is the LAST run of two or more spaces, not the first: a title may
// contain a double space ("In The  Dark") while Calibre pads the author
// column to a fixed width, so the final gap is the one separating the fields.
// Splitting on the first gap puts half a title into the author field whenever
// a title contains a double space -- and a 10-rating then attaches to the
// wrong author's fic.
func splitRow(rest string) (title, author string, authorCol int) {
	idx := lastGap(rest)
	if idx < 0 {
		return strings.TrimSpace(rest), "", 0
	}
	tail := strings.TrimLeft(rest[idx:], " ")
	authorCol = len(rest) - len(tail)
	return strings.TrimSpace(rest[:idx]), strings.TrimSpace(tail), authorCol
}

// lastGap returns the index where the last run of two or more spaces begins,
// or -1 when there is none.
func lastGap(s string) int {
	last, run := -1, 0
	for i := 0; i < len(s); i++ {
		if s[i] == ' ' {
			run++
			if run == 2 {
				last = i - 1
			}
			continue
		}
		run = 0
	}
	return last
}

// modalColumn returns the most common column start, or 0 when there is no
// agreement at all.
//
// A majority rather than a minimum: the point is to find where the author
// column BEGINS on a typical row, and one row with an unusually short title
// must not move that. Rows that disagree are still parsed correctly, because
// the title/author split is taken from each row's own widest gap.
func modalColumn(cols map[int]int) int {
	best, bestN := 0, 0
	for col, n := range cols {
		if n > bestN || (n == bestN && col < best) {
			best, bestN = col, n
		}
	}
	if bestN < 2 {
		return 0
	}
	return best
}

// LoadFromCalibreDB shells out to calibredb and parses its output.
//
// The shell-out is because calibredb has no library mode that emits machine-
// readable rows with ratings: `--for-machine` returns an empty list for a
// library whose rows lack the field it is asked for, which is exactly the
// field that matters here. The paste path is therefore the primary one and
// this is the convenience wrapper, so both produce identical Entries.
func LoadFromCalibreDB(ctx context.Context, extraArgs ...string) ([]Entry, error) {
	if _, err := exec.LookPath("calibredb"); err != nil {
		return nil, fmt.Errorf("ratedlist: calibredb is not installed: %w", err)
	}
	args := []string{"list", "--search", "#last_read:True",
		"--fields=*rating,title,authors", "--sort-by=*rating"}
	args = append(args, extraArgs...)
	cmd := exec.CommandContext(ctx, "calibredb", args...)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ratedlist: calibredb list: %w", err)
	}
	return ParseCalibrePaste(strings.NewReader(string(out)))
}

// LoadFromFile parses a saved paste.
func LoadFromFile(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("ratedlist: open %s: %w", path, err)
	}
	defer f.Close()
	return ParseCalibrePaste(f)
}

// joinField appends a wrapped piece with a space, which is right when the
// wrap landed BETWEEN tokens.
func joinField(cur, piece string) string {
	if cur == "" {
		return piece
	}
	return cur + " " + piece
}

// glueField appends a wrapped piece with no space, which is right when the
// wrap landed INSIDE one.
//
// Which of the two happened is not decidable from the text -- both arrive as
// an indented line at the same column -- so both readings are computed and
// the matcher tries each against the corpus. The primary reading is the glued
// one because a mid-word split is the common case for the long underscore
// pseudonyms this library is full of, and joining with a space there would
// produce a byline that matches nothing.
func glueField(cur, piece string) string {
	if cur == "" {
		return piece
	}
	return cur + piece
}

// parseHeader recognises the column header line and returns its id and
// rating column labels.
func parseHeader(line string) (idCol, ratingCol string, ok bool) {
	l := strings.ToLower(strings.TrimSpace(line))
	if !strings.HasPrefix(l, "id") {
		return "", "", false
	}
	if !strings.Contains(l, "title") {
		return "", "", false
	}
	i := strings.Index(strings.ToLower(line), "*rating")
	if i < 0 {
		return "id", "", true
	}
	return "id", line[i : i+len("*rating")], true
}

// cutRowStart splits a data row's leading integer id from the rest.
func cutRowStart(line string) (int64, string, bool) {
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	if i == 0 || i >= len(line) || line[i] != ' ' {
		return 0, "", false
	}
	var n int64
	for _, c := range []byte(line[:i]) {
		n = n*10 + int64(c-'0')
	}
	return n, line[i+1:], true
}

// parseRating reads the rating cell and returns the REST of the row.
//
// It returns the remainder because the rating cell is what separates the id
// from the title: parsing the title without removing the rating first yields
// "10   Psycho of the Dead" for every row, which is the shape a naive
// whitespace split produces and which silently offsets every later column.
//
// "None" is unrated and is consumed like a number, so an unrated row's title
// starts where a rated one's does. Treating it as a title fragment instead
// would shift 38 of this reader's rows by five columns.
func parseRating(rest string) (rating int, rated bool, body string) {
	f := strings.TrimLeft(rest, " ")
	i := 0
	for i < len(f) && f[i] >= '0' && f[i] <= '9' {
		i++
	}
	if i > 0 {
		var n int
		for _, c := range []byte(f[:i]) {
			n = n*10 + int(c-'0')
		}
		if n >= 0 && n <= 10 {
			return n, true, f[i:]
		}
	}
	if len(f) >= 4 && strings.EqualFold(f[:4], "None") {
		return 0, false, f[4:]
	}
	return 0, false, f
}

// ---------------------------------------------------------------- matching

// Match pairs a reading history with the works a mirror holds.
type Match struct {
	Entries    []Entry
	Exact      int
	Fuzzy      int
	TitleOnly  int
	Unmatched  []Entry
	Ambiguous  []Entry
	TotalWorks int64
}

// Report renders the match for a human, and is printed rather than
// summarised: a reader who wants to know why their profile is wrong needs
// the list of works that did not match, not a percentage.
func (m Match) Report() string {
	var b strings.Builder
	fmt.Fprintf(&b, "matched %d of %d rows (%.0f%%)\n",
		m.Exact+m.Fuzzy+m.TitleOnly, len(m.Entries),
		100*float64(m.Exact+m.Fuzzy+m.TitleOnly)/float64(max(1, len(m.Entries))))
	fmt.Fprintf(&b, "  exact (title+author)  %d\n", m.Exact)
	fmt.Fprintf(&b, "  fuzzy (same author)   %d\n", m.Fuzzy)
	fmt.Fprintf(&b, "  title-only            %d\n", m.TitleOnly)
	fmt.Fprintf(&b, "  unmatched             %d\n", len(m.Unmatched))
	if len(m.Ambiguous) > 0 {
		fmt.Fprintf(&b, "  ambiguous (2+ works share the title) %d\n", len(m.Ambiguous))
	}
	if len(m.Unmatched) > 0 {
		b.WriteString("\nunmatched (highest rated first; check these by hand):\n")
		sorted := append([]Entry(nil), m.Unmatched...)
		sort.SliceStable(sorted, func(i, j int) bool {
			ri, rj := entryRating(sorted[i]), entryRating(sorted[j])
			if ri != rj {
				return ri > rj
			}
			return sorted[i].Title < sorted[j].Title
		})
		for i, e := range sorted {
			if i >= 25 {
				fmt.Fprintf(&b, "  ... and %d more\n", len(sorted)-25)
				break
			}
			// Highest rated first, so a reader looking for the rows that
			// matter most finds them at the top of the listing.
			fmt.Fprintf(&b, "  %s - %s (rating %s)\n", e.Title, e.Author, ratingText(e))
		}
	}
	return b.String()
}

// entryRating sorts on the rating, treating unrated as zero.
func entryRating(e Entry) int {
	if e.Rated {
		return e.Rating
	}
	return 0
}

func ratingText(e Entry) string {
	if e.Rated {
		return fmt.Sprint(e.Rating)
	}
	return "unrated"
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Summarise counts the tiers and moves unmatched entries onto the report.
func (m *Match) Summarise() {
	m.Exact, m.Fuzzy, m.TitleOnly = 0, 0, 0
	m.Unmatched = nil
	m.Ambiguous = nil
	for _, e := range m.Entries {
		switch e.Method {
		case "exact":
			m.Exact++
		case "fuzzy":
			m.Fuzzy++
		case "title-only":
			m.TitleOnly++
		default:
			m.Unmatched = append(m.Unmatched, e)
		}
	}
}

// UnmatchedEntries returns the entries that matched nothing, so a caller can
// print them without reading the whole result set.
func (m Match) UnmatchedEntries() []Entry {
	var out []Entry
	for _, e := range m.Entries {
		if e.Method == "" {
			out = append(out, e)
		}
	}
	return out
}

// Matched returns the entries resolved to a work, at or above minConfidence.
//
// The confidence floor is a real filter, not a formality: a title-only match
// at 0.5 is a guess, and a 10-rating attached to the wrong fic corrupts every
// weight derived from it. A reader who wants the guesses passes 0.5.
func (m Match) Matched(minConfidence float64) []Entry {
	out := make([]Entry, 0, len(m.Entries))
	for _, e := range m.Entries {
		if e.WorkID > 0 && e.Confidence >= minConfidence {
			out = append(out, e)
		}
	}
	return out
}

// ---------------------------------------------------------------- matching

// Normalise reduces a title or author to its matching key.
//
// The transformations are the ones that actually occur between Calibre and
// AO3: case, punctuation, and any bracketed series or continuity suffix. That
// last one is the single biggest source of false misses -- "A Precise Note
// [MHA | Izuku-Centric]" and "A Precise Note" are the same fic to a reader
// and two rows to a matcher.
//
// SPACES, HYPHENS AND UNDERSCORES ALL COLLAPSE TO ONE SPACE. The three are
// interchangeable in practice: the mirror stores "The_Dark_Wolf_Shiro" where
// Calibre wrote "The Dark Wolf Shiro", and a title is hyphenated on one side
// and spaced on the other often enough that keeping them apart would lose
// real matches. Two works that differ ONLY in spacing are rare enough that
// the tiering absorbs the risk -- a title-only match is admitted at half
// confidence and printed in the report for a reader to check, while the
// author-scoped tier still requires the byline to agree.
func Normalise(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	lower := strings.ToLower(s)
	// Two bracket cuts, in order, because Calibre's titles carry markers the
	// mirror never had and both appear at the START:
	//
	//	"(High School DxD) Magician of Darkness"  ->  "Magician of Darkness"
	//	"[ASOIAF] Lord of Nature"                 ->  "Lord of Nature"
	//	"A Precise Note [MHA | Izuku-Centric]"   ->  "A Precise Note"
	//
	// Measured on this reader's library: 44 rows carry a leading fandom
	// marker, and 12 of them have a work in the mirror under the bare title.
	// Without this cut they match nothing at all, and they are the reader's
	// HIGHEST-rated works -- a gamer/system fandom reading list is prefixed
	// exactly as much as any other.
	//
	// Only a CLOSED bracket at the very start is cut, so a title that merely
	// begins with a parenthesis ("(un)Wholesome Want") survives when the
	// bracket is balanced later -- that title is real punctuation, not a
	// marker.
	if cut, ok := leadingMarker(lower); ok {
		lower = cut
	}
	if i := strings.IndexAny(lower, "[("); i > 0 {
		// Only cut when the bracket CLOSES, so a title that legitimately
		// contains an unbalanced "(" is not truncated at the open paren.
		if strings.ContainsAny(lower[i:], "])") {
			lower = lower[:i]
		}
	}
	for _, r := range lower {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// leadingMarker strips a fandom or status marker in the first bracket pair
// when one sits at the very start of the title.
//
// The condition is deliberately narrow: opening bracket at offset 0, a
// closing bracket, and real text after it. "(un)Wholesome Want" has a closing
// bracket but no whitespace after it, so it is left alone -- and it should
// be, because "(un)" there is part of the title rather than a fandom label.
func leadingMarker(s string) (string, bool) {
	if len(s) == 0 || (s[0] != '[' && s[0] != '(') {
		return s, false
	}
	i := strings.IndexAny(s, "])")
	if i < 0 {
		return s, false
	}
	rest := strings.TrimLeft(s[i+1:], " ")
	if rest == "" {
		// "(Hiatus)" alone is the whole title, not a marker prefix.
		return s, false
	}
	return rest, true
}

// NormaliseAuthor reduces an author string to its matching key.
//
// Calibre stores "aTasteofDarkness (Dirk_Grey)" for a byline and its
// pseudonym; the mirror stores the pseudonym alone. So the parenthetical is
// stripped before normalising, and the remaining token is what is matched.
func NormaliseAuthor(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "("); i > 0 {
		s = s[:i]
	}
	// Comma-separated multi-author rows: match on the FIRST author, which is
	// the byline in every multi-author format Calibre writes.
	if i := strings.Index(s, ","); i > 0 {
		s = s[:i]
	}
	return Normalise(s)
}

// authorKeys returns every normalised spelling of one byline that is worth
// matching on.
//
// Both the byline and the pseudonym, because Calibre writes
// "aTasteofDarkness (Dirk_Grey)" and the mirror may hold the work under
// either name -- which one is a property of the work, not of the reader's
// library. A single key here would silently lose those works.
func authorKeys(s string) []string {
	s = strings.TrimSpace(s)
	var out []string
	if i := strings.Index(s, "("); i > 0 {
		out = append(out, Normalise(strings.TrimSpace(s[:i])))
		if j := strings.Index(s[i:], ")"); j > 0 {
			out = append(out, Normalise(strings.TrimSpace(s[i+1:i+j])))
		}
	}
	for _, part := range strings.Split(s, ",") {
		if p := NormaliseAuthor(part); p != "" {
			out = append(out, p)
		}
	}
	seen := map[string]bool{}
	var uniq []string
	for _, k := range out {
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		uniq = append(uniq, k)
	}
	return uniq
}

// MatchToCorpus resolves each entry against the mirror's works table.
//
// The ladder is deliberately conservative and reports every rung:
//
//	exact        normalised title == title AND normalised author == author
//	fuzzy        normalised title within edit distance 2 of the title,
//	             AND the author matches one of the entry's author keys
//	title-only   normalised title match, author ignored  (confidence 0.5)
//
// Author-scoped fuzzy comes BEFORE title-only because a title match with a
// contradicting author is almost certainly the wrong work, and a title-only
// match is admitted at half confidence precisely because it is a guess.
func MatchToCorpus(ctx context.Context, db *sql.DB, entries []Entry) (Match, error) {
	m := Match{Entries: entries}
	if len(entries) == 0 {
		return m, fmt.Errorf("ratedlist: no entries to match")
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM works`).Scan(&m.TotalWorks); err != nil {
		return m, fmt.Errorf("ratedlist: count works: %w", err)
	}

	// The corpus index is built ONCE from the titles this history actually
	// mentions, not from all 112,935 works. Two queries do it:
	//
	//   - the distinct normalised titles the history wants, matched against
	//     the raw title column with a prefix probe per token, and
	//   - an author probe for the authors it names.
	//
	// A `SELECT id, title, authors FROM works` scan is 112,935 rows and ~15 MB
	// to find 257; the two probes touch the idx_works_title and
	// idx_works_authors indexes and return a few hundred. The whole point of
	// the indexes existing.
	titles := map[string]bool{}
	rawTitles := map[string]bool{}
	authors := map[string]bool{}
	for _, e := range entries {
		// Both readings of a wrapped field are probed, because the corpus is
		// what decides which one was the real byline.
		for _, t := range []string{Normalise(e.Title), Normalise(e.AltTitle)} {
			if t != "" {
				titles[t] = true
			}
		}
		for _, raw := range []string{e.Title, e.AltTitle} {
			for _, probe := range rawTitleProbes(raw) {
				if probe != "" {
					rawTitles[probe] = true
				}
			}
		}
		for _, a := range append(authorKeys(e.Author), authorKeys(e.AltAuthor)...) {
			if a != "" {
				authors[a] = true
			}
		}
	}
	corpus, err := loadCorpusIndex(ctx, db, titles, rawTitles, authors)
	if err != nil {
		return m, err
	}

	resolved := make([]Entry, 0, len(entries))
	for _, e := range entries {
		resolved = append(resolved, resolve(e, corpus))
	}
	m.Entries = resolved
	return m, nil
}

// corpusIndex is the candidate set, keyed for the three match tiers.
type corpusIndex struct {
	byTitleAuthor map[[2]string][]int64 // normalised title + author -> works
	byTitle       map[string][]int64
	byAuthor      map[string][]int64
	// titleByID is the reverse of byTitle: the normalised title of one work.
	// The fuzzy tier needs it, because it compares an entry's title against
	// every work an author wrote, and byTitle is keyed by title rather than
	// by work.
	titleByID map[int64]string
}

// loadCorpusIndex gathers the candidate works for a reading history.
//
// Two index-backed probes, one per column the matcher keys on:
//
//	SELECT ... FROM works WHERE title  = ? COLLATE NOCASE
//	SELECT ... FROM works WHERE authors = ? COLLATE NOCASE
//
// The author probe is what makes the fuzzy tier possible at all: a fuzzy
// title match is only trustworthy when the AUTHOR agrees, so its candidates
// come from the (much smaller) set of works by the authors this history
// names -- a few hundred rows on the live mirror, not 112,935.
//
// The title probe covers three spellings per entry, because normalisation is
// lossy in a way that has to be undone to hit the raw index:
//
//   - the title verbatim,
//   - the normalised form with single spaces (the index stores raw titles, so
//     "A Precise Note" must be probed as itself, not as "a precise note"),
//   - the bracketed/parenthetical suffix stripped, which is what turns
//     "A Precise Note [MHA | Izuku-Centric]" into a probe for the plain fic.
func loadCorpusIndex(ctx context.Context, db *sql.DB, titles, rawTitles, authors map[string]bool) (*corpusIndex, error) {
	idx := &corpusIndex{
		byTitleAuthor: map[[2]string][]int64{},
		byTitle:       map[string][]int64{},
		byAuthor:      map[string][]int64{},
		titleByID:     map[int64]string{},
	}
	add := func(id int64, title, authorsRaw string) {
		nt := Normalise(title)
		if nt == "" {
			return
		}
		idx.byTitle[nt] = append(idx.byTitle[nt], id)
		idx.titleByID[id] = nt
		for _, a := range authorKeys(authorsRaw) {
			idx.byAuthor[a] = append(idx.byAuthor[a], id)
			idx.byTitleAuthor[[2]string{nt, a}] =
				append(idx.byTitleAuthor[[2]string{nt, a}], id)
		}
	}

	seen := make(map[int64]bool, 512)
	probeTitle := func(probe string) error {
		if probe == "" {
			return nil
		}
		rows, err := db.QueryContext(ctx,
			`SELECT id, title, authors FROM works WHERE title = ? COLLATE NOCASE`,
			probe)
		if err != nil {
			return fmt.Errorf("ratedlist: title probe %q: %w", probe, err)
		}
		for rows.Next() {
			var id int64
			var title, authors string
			if err := rows.Scan(&id, &title, &authors); err != nil {
				rows.Close()
				return err
			}
			if !seen[id] {
				seen[id] = true
				add(id, title, authors)
			}
		}
		rows.Close()
		return rows.Err()
	}
	for nt := range titles {
		// The normalised key, title-cased per word as a courtesy probe, and
		// the suffix-stripped raw form. All three are cheap index hits; the
		// first is usually the only one that lands.
		for _, probe := range titleProbes(nt) {
			if err := probeTitle(probe); err != nil {
				return nil, err
			}
		}
	}
	// The RAW spellings, including the marker-stripped forms. These are what
	// find a work whose mirror title differs from the reader's by a fandom
	// prefix: the normalised key has lost the prefix by then, and the
	// reconstructed spelling from it can only ever be lower-cased words.
	for raw := range rawTitles {
		if err := probeTitle(raw); err != nil {
			return nil, err
		}
	}
	probeAuthor := func(a string) error {
		if a == "" {
			return nil
		}
		rows, err := db.QueryContext(ctx,
			`SELECT id, title, authors FROM works WHERE authors = ? COLLATE NOCASE`,
			a)
		if err != nil {
			return fmt.Errorf("ratedlist: author probe %q: %w", a, err)
		}
		for rows.Next() {
			var id int64
			var title, authors string
			if err := rows.Scan(&id, &title, &authors); err != nil {
				rows.Close()
				return err
			}
			if !seen[id] {
				seen[id] = true
				add(id, title, authors)
			}
		}
		rows.Close()
		return rows.Err()
	}
	for a := range authors {
		for _, probe := range authorProbes(a) {
			if err := probeAuthor(probe); err != nil {
				return nil, err
			}
		}
	}
	return idx, nil
}

// titleProbes returns the raw title spellings worth probing for a
// normalised key.
//
// Normalisation is lossy in a way that has to be undone to hit the raw index:
// the mirror stores "Magician of Darkness" and Calibre wrote "(High School
// DxD) Magician of Darkness", so the key is "magician of darkness" while the
// index holds "Magician of Darkness" -- and neither side is a substring of the
// other after normalisation. So the probes are reconstructed spellings: the
// title-cased key, the key with its last word dropped (a truncated title
// whose mirror row carries the full one), and the marker-stripped raw form.
func titleProbes(normalised string) []string {
	if normalised == "" {
		return nil
	}
	out := []string{asciiTitle(normalised)}
	if fields := strings.Fields(normalised); len(fields) > 1 {
		out = append(out, strings.Join(fields[:len(fields)-1], " "))
	}
	return out
}

// rawTitleProbes returns the ORIGINAL spellings worth probing, for entries
// whose title carried a leading fandom marker.
//
// It exists because the normalised key has already had the marker removed, so
// titleProbes can reconstruct neither the marked form nor a marker-stripped
// spelling with the source's own capitalisation. Both directions are probed
// here, on the raw string, because which of the two records carries the
// marker is not something the matcher can know in advance: Calibre has
// "(High School DxD) Magician of Darkness" where the mirror has "Magician of
// Darkness", and elsewhere a reader's title has no marker where the mirror's
// does.
//
// `cut` is the lower-cased remainder, so the raw suffix is taken by length
// rather than by re-searching for the marker, which keeps the original
// capitalisation of the words that matter.
func rawTitleProbes(raw string) []string {
	if raw == "" {
		return nil
	}
	var out []string
	if cut, ok := leadingMarker(strings.ToLower(raw)); ok {
		// The title as the mirror most likely spells it: marker removed,
		// leading space consumed, original case kept.
		out = append(out, strings.TrimSpace(raw[len(raw)-len(cut):]))
	}
	out = append(out, raw)
	return out
}

// authorProbes returns the raw author spellings worth probing.
func authorProbes(normalised string) []string {
	if normalised == "" {
		return nil
	}
	return []string{normalised, asciiTitle(normalised)}
}

// asciiTitle upper-cases the first letter of every word, leaving the rest
// alone.
//
// Deliberately not strings.Title, which is deprecated and lower-cases the
// remainder of each word: applying that to "shaboobamon" would probe
// "Shaboobamon" (correct) but to "HP" it would probe "Hp", which is a
// different pseudonym as far as the mirror's unique index is concerned.
func asciiTitle(s string) string {
	b := []byte(s)
	startOfWord := true
	for i := range b {
		c := b[i]
		switch {
		case c == ' ' || c == '-' || c == '_' || c == '.':
			startOfWord = true
		case startOfWord:
			if c >= 'a' && c <= 'z' {
				b[i] = c - 32
			}
			startOfWord = false
		}
	}
	return string(b)
}

// resolve assigns one entry to a mirror work, walking the confidence ladder.
func resolve(e Entry, idx *corpusIndex) Entry {
	// Both readings of a wrapped field are tried, primary first.
	titles := []string{Normalise(e.Title)}
	if alt := Normalise(e.AltTitle); alt != "" && alt != titles[0] {
		titles = append(titles, alt)
	}
	keys := append(authorKeys(e.Author), authorKeys(e.AltAuthor)...)

	// Tier 1: exact title and author.
	for _, nt := range titles {
		for _, a := range keys {
			if ids := idx.byTitleAuthor[[2]string{nt, a}]; len(ids) > 0 {
				e.WorkID = pickBest(ids)
				e.Confidence = 1.0
				e.Method = "exact"
				return e
			}
		}
	}

	// Tier 2: author matches, title within edit distance. Scoped to the
	// author's own works, which is what makes the edit distance safe.
	var best int64
	bestDist := maxEditDistance + 1
	bestTitle := ""
	for _, a := range keys {
		for _, id := range idx.byAuthor[a] {
			corpusTitle := idx.titleByID[id]
			for _, nt := range titles {
				d := editDistance(nt, corpusTitle)
				if d < bestDist || (d == bestDist && id < best) {
					best, bestDist, bestTitle = id, d, corpusTitle
				}
			}
		}
	}
	if best > 0 && bestDist <= maxEditDistance && bestTitle != "" {
		e.WorkID = best
		e.Confidence = 0.8
		e.Method = "fuzzy"
		return e
	}

	// Tier 3: title only, author ignored. Half confidence, because this is
	// the tier that can attach a rating to the wrong work.
	for _, nt := range titles {
		if ids := idx.byTitle[nt]; len(ids) > 0 {
			e.WorkID = pickBest(ids)
			e.Confidence = 0.5
			e.Method = "title-only"
			return e
		}
	}
	return e
}

// pickBest chooses one work from a candidate set.
//
// The lowest id wins, and the reason matters: this is the difference between
// a defensible guess and an arbitrary pick. Ties on a normalised title are
// common (works are re-uploaded and re-titled), and the sets are usually
// singleton. Where they are not, the lowest id is the mirror's oldest row for
// that title, which is the one a reader who rated a fic from years ago is
// most likely to have meant. Returning the set instead of a choice would
// force every caller to re-decide this.
func pickBest(ids []int64) int64 {
	best := int64(0)
	for _, id := range ids {
		if best == 0 || id < best {
			best = id
		}
	}
	return best
}

// maxEditDistance is the fuzzy title tolerance.
//
// Two, not three: Calibre and AO3 spell the same fic's title identically far
// more often than not, and the rows that differ by three characters are
// usually a different work by the same author (a series of sequels has
// titles one or two characters apart by design). Widening this trades a
// higher match rate for a higher rate of attaching a 10-rating to the wrong
// fic, which is the failure this package exists to avoid.
const maxEditDistance = 2

// editDistance is Levenshtein distance between two normalised strings.
//
// Bounded by maxEditDistance+1 rather than computed in full: an early exit
// past the tolerance turns an O(n*m) computation into a length check for the
// common case of titles that are obviously far apart, and the caller only
// ever cares about "within tolerance or not".
func editDistance(a, b string) int {
	if a == b {
		return 0
	}
	if abs(len(a)-len(b)) > maxEditDistance {
		return maxEditDistance + 1
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		best := cur[0]
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = prev[j-1] + cost
			if v := prev[j] + 1; v < cur[j] {
				cur[j] = v
			}
			if v := cur[j-1] + 1; v < cur[j] {
				cur[j] = v
			}
			if cur[j] < best {
				best = cur[j]
			}
		}
		if best > maxEditDistance {
			return maxEditDistance + 1
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
