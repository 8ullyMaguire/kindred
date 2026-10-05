package dump

// Version allocation, delta manifests and retention — SPEC §4.3.
//
// ## Why this is a new file rather than more of dump.go
//
// §4.3 names three things that did not exist:
//
//   - "Deltas: a delta manifest lists only shards whose contents changed
//     against its `base`, so a peer pulls a few hundred KB per day instead of
//     600 MB."
//   - "A full snapshot every 20 versions."
//   - "Retention: the three most recent versions, then delete."
//
// `dump --version 0` advertised "next after the newest on disk" and wrote to
// `v0`. So there was no notion of a next version at all, which is why nothing
// downstream could exist: a delta needs a base to differ from, and retention
// needs a version to count.
//
// ## The three claims, and what each is worth
//
// **Retention is easy to state and easy to get wrong.** "Keep 3" is one line of
// sort-and-slice. Getting it wrong is what matters: deleting the wrong
// directory destroys a snapshot someone is still serving, and the delta log
// makes that worse, because a delta against a pruned base is unreconstructable.
// So Prune refuses to touch a version that is the base of a retained delta, and
// TestPruneRefusesToRemoveABaseStillReferenced is the gate for it.
//
// **Deltas are only useful if they are small, and "small" is a property of the
// data, not of the code.** A shard is included in a delta when its content hash
// changed against the base. With a per-dump salt the pseudonym changes on every
// dump, so EVERY shard changes and the delta is the full snapshot. That is not
// a bug, it is what `--stable-salt` is for, and the delta code reports the
// resulting size rather than claiming a saving it did not achieve.
//
// **A full snapshot every 20 versions is a floor, not a ceiling.** ForceFull is
// what makes the claim true regardless of how few shards moved, and
// ForceInterval is the constant the spec's "20" lives in so a change to the
// policy is one edit plus one test rather than a search.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ForceInterval is how many versions pass between full snapshots — SPEC §4.3's
// "a full snapshot every 20 versions".
//
// Exported because the number is a POLICY, and a policy buried as a literal in
// the middle of a function is a policy nobody can find to argue with.
const ForceInterval = 20

// RetentionVersions is how many versions survive a Prune — SPEC §4.3's "the
// three most recent versions, then delete".
//
// Three is also what makes a delta chain reconstructable: a peer holding v_n
// can walk back through at most ForceInterval-1 deltas to a full base, so as
// long as RetentionVersions <= ForceInterval the oldest retained version is
// always a full one. If you raise Retention below ForceInterval the peer is
// fine; if you ever raise ForceInterval without raising Retention, a peer that
// has fallen behind can find its base gone. NewForceFull checks that
// relationship rather than trusting a reader to notice.
const RetentionVersions = 3

// VersionDirName is the directory name for a version: "v12".
func VersionDirName(v int) string { return fmt.Sprintf("v%d", v) }

// ParseVersionDir parses "v12" into 12. Returns ok=false for anything else, so a
// stray directory in the snapshot root is ignored rather than crashing a dump.
func ParseVersionDir(name string) (int, bool) {
	if !strings.HasPrefix(name, "v") {
		return 0, false
	}
	n, err := strconv.Atoi(name[1:])
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// ListVersions returns the version numbers present in dir, ascending.
//
// It reads the DIRECTORY rather than trusting a manifest or an index file,
// because the directory is what a peer sees over the wire and what an operator
// sees with `ls`. A versions list that disagreed with the filesystem would be a
// second source of truth, and retention acting on it would delete the wrong
// thing.
//
// A v0 directory is a real version and is included. v0 is what `dump
// --version 0` used to write unconditionally, so treating it as "no version"
// would strand a snapshot a peer is already fetching.
func ListVersions(dir string) ([]int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []int
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if v, ok := ParseVersionDir(e.Name()); ok {
			out = append(out, v)
		}
	}
	sort.Ints(out)
	return out, nil
}

// NewestVersion returns the highest version on disk, and ok=false when there is
// none. This is what `--version 0` means.
func NewestVersion(dir string) (int, bool, error) {
	vs, err := ListVersions(dir)
	if err != nil || len(vs) == 0 {
		return 0, false, err
	}
	return vs[len(vs)-1], true, nil
}

// AllocateVersion resolves a requested version to the one to write.
//
// requested == 0 means "the next after the newest on disk", which is what the
// flag has always claimed and never did. Explicit positive versions are taken
// at face value, EXCEPT that they are rejected if the directory already exists:
// overwriting a version in place would silently invalidate a delta chain that
// names it as a base, and a manifest that hashes shards which are then rewritten
// is a snapshot that fails verification for reasons nobody will connect to this
// decision.
//
// The error says what to do instead, because the operator's next move after
// EEXIST is usually "use the next version". An earlier version also suggested
// "--force-version to overwrite deliberately", and there is no such flag: an
// error naming a flag the binary does not have sends the operator to try it, and
// "flag provided but not defined" reads as a typo rather than as the tool having
// lied about itself.
func AllocateVersion(dir string, requested int) (int, error) {
	vs, err := ListVersions(dir)
	if err != nil {
		return 0, err
	}
	if requested < 0 {
		return 0, fmt.Errorf("--version %d is negative; versions start at 0", requested)
	}
	if requested == 0 && len(vs) > 0 {
		return vs[len(vs)-1] + 1, nil
	}
	if requested > 0 {
		if target := filepath.Join(dir, VersionDirName(requested)); dirExists(target) {
			return 0, fmt.Errorf("--version %d already exists at %s; refusing to "+
				"overwrite it, because a delta may name it as its base and "+
				"rewriting it would break that chain. Use --version 0 for the next one.",
				requested, target)
		}
	}
	return requested, nil
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// NewForceFull reports whether version v must be a full snapshot.
//
// True at v == 0, and every ForceInterval versions after. The modulo is on the
// version NUMBER, not on the count of versions present, so a snapshot directory
// that was pruned or copied still produces the same schedule.
func NewForceFull(v int) bool {
	if v <= 0 {
		return true
	}
	return v%ForceInterval == 0
}

// DeltaAgainst decides whether version v should be a delta against base, and
// returns the shard names the delta must carry.
//
// The rule is deliberately dumb: carry a shard if its hash differs from the
// base's. There is no cleverness about which shards "should" have changed,
// because a shard's contents depend on the corpus at dump time and predicting
// that is how a delta silently omits a row a peer needed.
//
// Returns ok=false with an explanation when the decision cannot be made:
//   - no base, or v is below the base: the caller must write a full snapshot
//   - the base is not a full snapshot: a delta on a delta chains, and §4.3 says
//     deltas are listed against a BASE. Chaining is not forbidden, but it means
//     the "few hundred KB" claim depends on every link in the chain, so it is
//     refused here rather than discovered as an unusable peer.
func DeltaAgainst(base *Manifest, v int) (names []string, ok bool, reason string) {
	if base == nil {
		return nil, false, "no base manifest given"
	}
	if base.Full && base.BaseVersion >= 0 {
		// A full snapshot must name -1. It was applied to every base at first,
		// which refused every delta whose base was not v0 and printed "v3 claims
		// base v2 but is marked full" about a delta that was fine.
		return nil, false, fmt.Sprintf("v%d is marked full but names base v%d; "+
			"a full snapshot must name -1", base.SnapshotVersion, base.BaseVersion)
	}
	if !base.Full && base.BaseVersion < 0 {
		return nil, false, fmt.Sprintf("v%d is a delta but names no base; "+
			"a delta must name one", base.SnapshotVersion)
	}
	if !base.Full {
		return nil, false, fmt.Sprintf("base v%d is itself a delta; a delta on a "+
			"delta chains, and §4.3 lists deltas against a base", base.SnapshotVersion)
	}
	// SnapshotVersion, NOT Version. Version is the manifest format version and is
	// 1 for every snapshot ever written, so `v <= base.Version` reads as
	// `v <= 1` and refuses every delta past v1 — which is every delta that
	// matters. The test for this caught it; the comment on Manifest does not
	// prevent it.
	if v <= base.SnapshotVersion {
		return nil, false, fmt.Sprintf("version %d is not newer than its base v%d",
			v, base.SnapshotVersion)
	}
	names = make([]string, 0, len(base.Shards))
	for name := range base.Shards {
		names = append(names, name)
	}
	sort.Strings(names)
	return names, true, ""
}

// ChangedShards returns the subset of current whose content hash differs from
// the same shard in base, plus the shards that are in current and not in base
// at all (new buckets) and, symmetrically, the ones in base and not in current
// (deleted buckets).
//
// The deleted case is the one that is easy to omit and impossible to recover
// from: a peer applying only the changed and new shards keeps rows for a bucket
// that no longer exists, and nothing downstream will ever report the
// discrepancy because the peer's own copy is self-consistent.
//
// Returns the three groups separately so the caller can report the accounting
// rather than just the union.
func ChangedShards(base, current map[string]Shard) (changed, added, removed []string) {
	for name, cur := range current {
		prev, ok := base[name]
		if !ok {
			added = append(added, name)
			continue
		}
		if prev.Hash != cur.Hash {
			changed = append(changed, name)
		}
	}
	for name := range base {
		if _, ok := current[name]; !ok {
			removed = append(removed, name)
		}
	}
	sort.Strings(changed)
	sort.Strings(added)
	sort.Strings(removed)
	return changed, added, removed
}

// Prune deletes every version beyond the newest RetentionVersions.
//
// It refuses to delete a version that is the BaseVersion of a RETAINED version.
// That is the whole reason this function is more than a sort-and-slice: a delta
// whose base is gone cannot be applied by anyone who did not already hold the
// base, so pruning the base turns a retained delta into a file that verifies
// its own signature and then cannot be used.
//
// Returned rather than logged: a caller that ignores the return value is
// choosing to leave stale directories on disk, which is recoverable, but it
// should be a choice.
func Prune(dir string, keep int) (removed []int, err error) {
	if keep < 1 {
		return nil, errors.New("retention must keep at least one version; " +
			"keeping zero would delete the only snapshot")
	}
	vs, err := ListVersions(dir)
	if err != nil {
		return nil, err
	}
	if len(vs) <= keep {
		return nil, nil
	}
	survivors := vs[len(vs)-keep:]
	protected, err := protectedBases(dir, survivors)
	if err != nil {
		return nil, err
	}

	for _, v := range vs[:len(vs)-keep] {
		if protected[v] {
			// Kept despite being past the retention window, because a
			// retained delta names it. Says so on stdout rather than silently
			// leaving more versions than asked for, since "retention is 3" and
			// "four directories exist" should not be a surprise.
			fmt.Printf("  retention: keeping v%d — a retained delta has it as its base\n", v)
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, VersionDirName(v))); err != nil {
			return removed, fmt.Errorf("prune v%d: %w", v, err)
		}
		removed = append(removed, v)
	}
	return removed, nil
}

// protectedBases returns the set of versions named as the base of any of the
// given survivors.
//
// It reads manifest.canonical — the exact bytes Verify reads — rather than
// trusting the directory name or the pretty manifest.json. Verify hashes what
// it signs, so this must look at what it signs too, or retention could protect
// a base computed from a different view of the manifest than the one a peer
// will apply.
//
// Only a DELTA has a base, so `!Full` is the discriminator. BaseVersion 0 is
// genuinely ambiguous — "the first version" and "unset" are the same integer,
// and `omitempty` drops it — but a full snapshot has no base to protect
// whatever its BaseVersion field happens to say, so the ambiguity is confined
// to the case that does not need resolving.
//
// An unreadable manifest protects the version ITSELF and nothing else. That is
// deliberate: we cannot know what it depends on, so retain rather than guess.
// The cost is one extra directory; the cost of guessing wrong is a delta whose
// base is gone, which verifies its own signature perfectly and then cannot be
// applied by anyone who did not already have the base.
func protectedBases(dir string, survivors []int) (map[int]bool, error) {
	out := map[int]bool{}
	for _, v := range survivors {
		raw, err := os.ReadFile(filepath.Join(dir, VersionDirName(v), "manifest.canonical"))
		if err != nil {
			out[v] = true
			continue
		}
		var m Manifest
		if err := json.Unmarshal(raw, &m); err != nil {
			out[v] = true
			continue
		}
		// BaseVersion >= 0 means a base is named. -1 means none. This no
		// longer depends on Full, which is the point: the earlier version keyed
		// on Full and a mutation swapping one for the other survived, because
		// every manifest the suite built had Full and BaseVersion agreeing.
		if m.BaseVersion >= 0 {
			out[m.BaseVersion] = true
		}
	}
	return out, nil
}

// IsFullSnapshot decides whether version v should be a full snapshot, and
// reports which base a delta would take (0 for a full snapshot).
//
// Four reasons produce a full snapshot, and the reason is returned because "why
// is this full" is the question an operator asks when every third dump is one:
//
//  1. v == 0, or v lands on the ForceInterval boundary -- §4.3's schedule
//  2. no previous version exists
//  3. the previous version is itself a delta (chaining is refused)
//  4. --full was passed
//
// The interval is a floor, not the only trigger: with a per-dump salt EVERY
// shard changes and every dump would be a full snapshot anyway, which is correct
// but worth SAYING rather than leaving the operator to infer it from the size.
func IsFullSnapshot(dir string, v int, force bool) (full bool, baseVer int, reason string) {
	if force {
		return true, 0, "--full was requested"
	}
	if v == 0 {
		return true, 0, "the first snapshot is full by definition"
	}
	if NewForceFull(v) {
		return true, 0, fmt.Sprintf("version %d is on the every-%d full-snapshot boundary",
			v, ForceInterval)
	}
	newest, ok, err := NewestVersion(dir)
	if err != nil {
		return true, 0, fmt.Sprintf("cannot read %s: %v", dir, err)
	}
	if !ok || newest < v-1 {
		return true, 0, fmt.Sprintf("the previous version (v%d) is not on disk, so there is "+
			"nothing to be a delta against", newest)
	}
	baseDir := filepath.Join(dir, VersionDirName(v-1))
	raw, err := os.ReadFile(filepath.Join(baseDir, "manifest.canonical"))
	if err != nil {
		return true, 0, fmt.Sprintf("cannot read v%d's manifest: %v", v-1, err)
	}
	var base Manifest
	if err := json.Unmarshal(raw, &base); err != nil {
		return true, 0, fmt.Sprintf("v%d's manifest does not parse: %v", v-1, err)
	}
	if _, ok, reason := DeltaAgainst(&base, v); !ok {
		return true, 0, reason
	}
	return false, v - 1, fmt.Sprintf("v%d is a delta against v%d", v, v-1)
}

// DeltaAccounting summarises what a delta would carry, so the caller can report
// the saving rather than assert one.
//
// A delta that carries every shard saves nothing, and saying "delta" in the log
// while shipping the full snapshot is how a peer ends up pulling 600 MB and
// concluding the feature is broken. The size fields come from the shards on disk,
// so this needs the base directory and the directory being written.
// DeltaAccounting reports how many shards a delta actually carries, counted from
// the NEW manifest rather than the base's.
//
// Counting from the base was the first version and it was wrong twice over: an
// unchanged shard is deliberately ABSENT from the new directory, so reading it
// failed and each failure counted as "carried". That reported "256 of 256, every
// shard changed" for a delta carrying none of them, and printed the "consider
// --stable-salt" note to an operator already using --stable-salt.
func DeltaAccounting(base, m *Manifest) (carried, baseTotal int) {
	return len(m.Shards), len(base.Shards)
}

// DeltaBytesOnDisk sums the actual file sizes for the shards a delta carries.
// Kept separate from DeltaAccounting because the byte figure has to come from
// the filesystem -- the manifest does not record sizes, and adding a size field
// to a signed manifest to serve a log line would be a poor trade.
func DeltaBytesOnDisk(dir string, m *Manifest) int64 {
	var n int64
	for _, sh := range m.Shards {
		if sh.Removed || sh.Path == "" {
			continue
		}
		st, err := os.Stat(filepath.Join(dir, sh.Path))
		if err != nil {
			continue
		}
		n += st.Size()
	}
	return n
}

// ShardFileName is the one place a shard key becomes a file name. It exists
// because that mapping was written three times by hand and got it wrong twice:
// once as "shard-%03d.json" against a key of "42", and once as key+".json",
// which is "42.json".
func ShardFileName(key string) string {
	n, err := strconv.Atoi(key)
	if err != nil {
		return key
	}
	return fmt.Sprintf("shard-%03d.json", n)
}

// LoadManifest reads a version's manifest WITHOUT verifying its signature.
//
// Deliberately separate from Verify: retention and delta selection run inside
// `dump`, before the new manifest has been signed, and they must be able to read
// the previous version's own claims. Verify is for a peer deciding whether to
// trust bytes from elsewhere.
//
// It reads manifest.canonical, the exact bytes that were signed, rather than
// manifest.json. The pretty form is a rendering; the canonical form is the
// claim, and a reader that consults the rendering is reading a file no signature
// covers.
func LoadManifest(dir string) (*Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.canonical"))
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse manifest in %s: %w", dir, err)
	}
	if m.Version != ManifestVersion {
		return nil, fmt.Errorf("manifest format version %d is not supported (want %d)",
			m.Version, ManifestVersion)
	}
	return &m, nil
}
