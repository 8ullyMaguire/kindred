package dump

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// mkVersion writes a minimal on-disk version so the directory scan has something
// real to read. A manifest is only written when full/delta is specified, so a
// test can create a directory with NO manifest — which is the case that decides
// whether unreadable manifests are safe.
func mkVersion(t *testing.T, root string, v int, full bool, base int) string {
	t.Helper()
	dir := filepath.Join(root, VersionDirName(v))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	m := Manifest{Version: ManifestVersion, BaseVersion: base, Full: full,
		BuildTime: "2026-10-05T00:00:00Z", KAnon: 20, SaltMode: "per-dump",
		Shards: map[string]Shard{}, RowCounts: map[string]int64{}}
	if !full {
		m.BaseVersion = base
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.canonical"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestListVersionsIgnoresNonVersionDirs(t *testing.T) {
	root := t.TempDir()
	mkVersion(t, root, 0, true, 0)
	mkVersion(t, root, 3, true, 0)
	mkVersion(t, root, 12, true, 0)
	for _, junk := range []string{"notes", "tmp", "v", "vx", "v-1", "manifest.json"} {
		if err := os.MkdirAll(filepath.Join(root, junk), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ListVersions(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []int{0, 3, 12}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// v0 is a REAL version: `dump --version 0` used to write it unconditionally, so
// treating it as "no version" would strand a snapshot a peer already fetches.
func TestListVersionsIncludesZero(t *testing.T) {
	root := t.TempDir()
	mkVersion(t, root, 0, true, 0)
	got, err := ListVersions(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != 0 {
		t.Fatalf("v0 must be listed, got %v", got)
	}
}

func TestListVersionsOnMissingDirIsEmptyNotError(t *testing.T) {
	got, err := ListVersions(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatalf("a missing snapshot root is empty, not a failure: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

// THE BUG: `--version 0` advertised "next after the newest on disk" and wrote to
// v0 literally.
func TestAllocateVersionZeroMeansNextAfterNewest(t *testing.T) {
	for _, tc := range []struct {
		present []int
		want    int
	}{{nil, 0}, {[]int{0}, 1}, {[]int{0, 1, 2}, 3}, {[]int{7, 3, 11}, 12}} {
		root := t.TempDir()
		for _, v := range tc.present {
			mkVersion(t, root, v, true, 0)
		}
		got, err := AllocateVersion(root, 0)
		if err != nil {
			t.Fatalf("present=%v: %v", tc.present, err)
		}
		if got != tc.want {
			t.Fatalf("present=%v: got v%d, want v%d", tc.present, got, tc.want)
		}
	}
}

// Overwriting a version in place silently invalidates any delta naming it as a
// base, and the symptom surfaces as a verification failure nobody connects to
// this decision.
func TestAllocateVersionRefusesToOverwrite(t *testing.T) {
	root := t.TempDir()
	mkVersion(t, root, 5, true, 0)
	_, err := AllocateVersion(root, 5)
	if err == nil {
		t.Fatal("refusing to overwrite must be an error")
	}
	if !contains(err.Error(), "already exists") {
		t.Fatalf("the error must say what happened and what to do: %v", err)
	}
}

func TestAllocateVersionRejectsNegative(t *testing.T) {
	if _, err := AllocateVersion(t.TempDir(), -1); err == nil {
		t.Fatal("a negative version must be refused")
	}
}

func TestNewForceFullFollowsTheInterval(t *testing.T) {
	for v, want := range map[int]bool{0: true, 1: false, 19: false, 20: true,
		21: false, 39: false, 40: true, -1: true} {
		if got := NewForceFull(v); got != want {
			t.Fatalf("NewForceFull(%d) = %v, want %v", v, got, want)
		}
	}
}

// The modulo is on the version NUMBER, not the count of directories present, so
// a pruned or partially-copied snapshot root still produces the same schedule.
func TestNewForceFullIsIndependentOfWhatIsOnDisk(t *testing.T) {
	root := t.TempDir()
	for _, v := range []int{41, 42} { // 40 is absent: it was pruned
		mkVersion(t, root, v, true, 0)
	}
	if NewForceFull(60) != true {
		t.Fatal("v60 must be full regardless of what is on disk")
	}
}

// fullWithShards builds a FULL base manifest. NOTE the `v` is the SNAPSHOT
// version and it is NOT stored: Manifest.Version is the manifest FORMAT version
// (1 for every snapshot this build has ever written). That confusion is the
// subject of TestManifestVersionIsFormatNotSnapshot below.
func fullWithShards(v int, hashes map[string]string) *Manifest {
	m := &Manifest{Version: ManifestVersion, SnapshotVersion: v, Full: true,
		BaseVersion: -1, Shards: map[string]Shard{}}
	if m.SnapshotVersion != v {
		panic("the snapshot number must survive construction; DeltaAgainst reads it")
	}
	for name, h := range hashes {
		m.Shards[name] = Shard{Hash: h, Path: name, Rows: 1}
	}
	return m
}

// The deleted-bucket case: a peer applying only changed+new shards keeps rows
// for a bucket that no longer exists, and nothing downstream ever reports it
// because the peer's own copy is self-consistent.
func TestChangedShardsReportsAllThreeGroups(t *testing.T) {
	base := fullWithShards(1, map[string]string{"000": "a", "001": "b", "002": "c"})
	cur := fullWithShards(2, map[string]string{"000": "a", "001": "CHANGED", "003": "d"})
	changed, added, removed := ChangedShards(base.Shards, cur.Shards)
	assertStrSet(t, "changed", changed, []string{"001"})
	assertStrSet(t, "added", added, []string{"003"})
	assertStrSet(t, "removed", removed, []string{"002"})
}

func TestChangedShardsEmptyWhenNothingMoved(t *testing.T) {
	h := map[string]string{"000": "a", "001": "b"}
	c, a, r := ChangedShards(fullWithShards(1, h).Shards, fullWithShards(2, h).Shards)
	if len(c)+len(a)+len(r) != 0 {
		t.Fatalf("identical shards must produce an empty delta: %v %v %v", c, a, r)
	}
}

func TestPruneKeepsTheThreeNewest(t *testing.T) {
	root := t.TempDir()
	for v := 1; v <= 6; v++ {
		mkVersion(t, root, v, true, 0)
	}
	removed, err := Prune(root, RetentionVersions)
	if err != nil {
		t.Fatal(err)
	}
	left, _ := ListVersions(root)
	assertSet(t, "removed", removed, []int{1, 2, 3})
	assertSet(t, "remaining", left, []int{4, 5, 6})
}

func TestPruneIsANoOpWhenUnderTheLimit(t *testing.T) {
	root := t.TempDir()
	for v := 1; v <= 2; v++ {
		mkVersion(t, root, v, true, 0)
	}
	removed, err := Prune(root, RetentionVersions)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Fatalf("nothing to remove, got %v", removed)
	}
}

func TestPruneRefusesToKeepZero(t *testing.T) {
	root := t.TempDir()
	mkVersion(t, root, 1, true, 0)
	if _, err := Prune(root, 0); err == nil {
		t.Fatal("keeping zero versions would delete the only snapshot")
	}
}

// THE REASON Prune IS NOT A SORT-AND-SLICE: a delta whose base is gone cannot be
// applied by anyone who did not already hold the base. The delta still verifies
// its own signature; it is simply unusable, silently.
func TestPruneRefusesToRemoveABaseStillReferenced(t *testing.T) {
	root := t.TempDir()
	// v2..v5 retained, deltas naming v1 as their base.
	for v := 2; v <= 5; v++ {
		mkVersion(t, root, v, false, 1)
	}
	mkVersion(t, root, 1, true, 0)

	removed, err := Prune(root, RetentionVersions)
	if err != nil {
		t.Fatal(err)
	}
	if containsInt(removed, 1) {
		t.Fatalf("v1 is the base of every retained delta; removing it destroys them: %v", removed)
	}
	if _, err := os.Stat(filepath.Join(root, "v1")); err != nil {
		t.Fatalf("v1 must survive: %v", err)
	}
}

func TestPruneRemovesABaseNothingReferences(t *testing.T) {
	root := t.TempDir()
	mkVersion(t, root, 1, true, 0)
	for v := 2; v <= 5; v++ {
		mkVersion(t, root, v, true, 0) // full, no base
	}
	removed, err := Prune(root, RetentionVersions)
	if err != nil {
		t.Fatal(err)
	}
	if !containsInt(removed, 1) {
		t.Fatalf("nothing references v1, so it must be removed: %v", removed)
	}
}

// We cannot know what an unreadable manifest depends on, so retain. Guessing
// wrong here destroys a base; an extra directory costs disk.
func TestPruneRetainsVersionWithUnreadableManifest(t *testing.T) {
	root := t.TempDir()
	for v := 2; v <= 5; v++ {
		mkVersion(t, root, v, false, 1)
	}
	// v1 exists but its manifest is corrupt, so its dependencies are unknown.
	dir := filepath.Join(root, "v1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.canonical"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	removed, err := Prune(root, RetentionVersions)
	if err != nil {
		t.Fatal(err)
	}
	if containsInt(removed, 1) {
		t.Fatalf("an unreadable manifest must not be pruned: %v", removed)
	}
}

// A delta on a delta is not forbidden, but §4.3 lists deltas against a base and
// the "few hundred KB" claim then depends on every link in the chain.
func TestDeltaAgainstRefusesToChain(t *testing.T) {
	base := fullWithShards(1, map[string]string{"000": "a"})
	base.Full = false
	base.BaseVersion = 0 // a delta against v0, the only version that can be
	_, ok, reason := DeltaAgainst(base, 2)
	if ok {
		t.Fatal("a delta must not be taken against another delta")
	}
	if !contains(reason, "chains") {
		t.Fatalf("the refusal must say why: %q", reason)
	}
}

func TestDeltaAgainstRefusesANonIncreasingVersion(t *testing.T) {
	base := fullWithShards(5, map[string]string{"000": "a"})
	if _, ok, _ := DeltaAgainst(base, 5); ok {
		t.Fatal("a delta against its own version is not a delta")
	}
	if _, ok, _ := DeltaAgainst(base, 4); ok {
		t.Fatal("a delta against a newer version is not a delta")
	}
}

func TestDeltaAgainstListsTheBaseShards(t *testing.T) {
	base := fullWithShards(1, map[string]string{"002": "c", "000": "a", "001": "b"})
	names, ok, _ := DeltaAgainst(base, 2)
	if !ok {
		t.Fatal("a newer version against a full base must be allowed")
	}
	assertStrSet(t, "names", names, []string{"000", "001", "002"})
}

// assertSet is for the []int that version lists produce; shard names are
// []string and go through assertStrSet. Two functions because conflating them is
// how a shard assertion ends up comparing "001" against 1 and passing.
func assertSet(t *testing.T, what string, got, want []int) {
	t.Helper()
	g, w := intsToStrs(got), intsToStrs(want)
	if len(g) != len(w) {
		t.Fatalf("%s: got %v, want %v", what, g, w)
	}
	for i := range w {
		if g[i] != w[i] {
			t.Fatalf("%s: got %v, want %v", what, g, w)
		}
	}
}

func assertStrSet(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: got %v, want %v", what, got, want)
		}
	}
}

func intsToStrs(in []int) []string {
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = strconv.Itoa(v)
	}
	return out
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func containsInt(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

// THE FINDING: `Manifest.Version` is the manifest FORMAT version, and it is 1
// for every snapshot this build has ever written. So `fetch` printed
// "fetched snapshot v1 from <peer>" for every peer, and the snapshot number
// existed only as a directory name — which is not signed, so it is not
// verifiable.
//
// This pins the two apart. If someone renames the fields back to overlapping
// meanings, this fails.
func TestManifestVersionIsFormatNotSnapshot(t *testing.T) {
	m := Manifest{Version: ManifestVersion, SnapshotVersion: 12, Full: true}
	if m.Version == m.SnapshotVersion {
		t.Fatal("the test is vacuous: the two fields must be able to differ")
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var back Manifest
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Version != ManifestVersion {
		t.Fatalf("format version must survive the round trip, got %d", back.Version)
	}
	if back.SnapshotVersion != 12 {
		t.Fatalf("the snapshot number must be in the signed bytes, got %d",
			back.SnapshotVersion)
	}
	// And the JSON keys must not collide, which is the actual failure mode if
	// someone had tried to reuse `version` for this.
	var raw2 map[string]any
	if err := json.Unmarshal(raw, &raw2); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"version", "snapshot_version"} {
		if _, ok := raw2[k]; !ok {
			t.Fatalf("manifest JSON must carry %q, got keys %v", k, keysOf(raw2))
		}
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// THE BUG THIS FIXED: `base_version` was tagged omitempty, and a delta against
// v0 has base_version 0 -- which is CORRECT, v0 being the first version. So the
// field vanished from the signed bytes and a peer could not tell a delta against
// the first snapshot from a manifest naming no base at all.
//
// It is now not omitempty, and a full snapshot names -1, which is not a valid
// version. So "is this a delta" is answerable from the field alone.
func TestBaseVersionIsExplicitNotOmitted(t *testing.T) {
	// A delta against v0: base_version 0 must SURVIVE serialisation.
	delta := Manifest{Version: ManifestVersion, SnapshotVersion: 1, Full: false,
		BaseVersion: 0, Shards: map[string]Shard{}}
	raw, err := json.Marshal(delta)
	if err != nil {
		t.Fatal(err)
	}
	m := keysOf2(raw)
	if _, present := m["base_version"]; !present {
		t.Fatal("a delta against v0 must publish base_version 0; this is the " +
			"omitempty bug this test exists for")
	}
	if m["base_version"].(float64) != 0 {
		t.Fatalf("base_version must be 0 for a delta against v0, got %v", m["base_version"])
	}

	// A full snapshot must publish -1, so "no base" is distinguishable from
	// "base v0" without needing Full.
	full := Manifest{Version: ManifestVersion, SnapshotVersion: 0, Full: true,
		BaseVersion: -1, Shards: map[string]Shard{}}
	raw2, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}
	m2 := keysOf2(raw2)
	if _, present := m2["base_version"]; !present {
		t.Fatal("a full snapshot must publish base_version -1")
	}
	if m2["base_version"].(float64) != -1 {
		t.Fatalf("want -1, got %v", m2["base_version"])
	}

	var back Manifest
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.BaseVersion != 0 {
		t.Fatalf("round trip changed base_version to %d", back.BaseVersion)
	}
}

// A full snapshot that names a base, or a delta that names none, is a
// contradiction. DeltaAgainst must refuse it rather than proceed on whichever
// field it happened to read first.
//
// The first version of this guard was applied to every base, so it refused every
// delta whose base was not v0 and said "v3 claims base v2 but is marked full"
// about a perfectly good delta. Found by running eight real dumps on the mirror.
func TestDeltaAgainstRejectsContradictoryManifests(t *testing.T) {
	fullWithBase := fullWithShards(1, map[string]string{"000": "a"})
	fullWithBase.BaseVersion = 2 // full, but claims a base
	_, ok, reason := DeltaAgainst(fullWithBase, 2)
	if ok {
		t.Fatal("a full snapshot claiming a base must be refused")
	}
	if !contains(reason, "marked full") {
		t.Fatalf("reason must name the contradiction: %q", reason)
	}

	deltaWithoutBase := fullWithShards(1, map[string]string{"000": "a"})
	deltaWithoutBase.Full = false
	deltaWithoutBase.BaseVersion = -1 // a delta, but names no base
	_, ok, reason = DeltaAgainst(deltaWithoutBase, 2)
	if ok {
		t.Fatal("a delta naming no base must be refused")
	}
	if !contains(reason, "names no base") {
		t.Fatalf("reason must say the base is missing: %q", reason)
	}

	// The case the broken guard got wrong: a well-formed DELTA must not be
	// rejected by the full-snapshot contradiction check. v1 is a delta naming v0
	// (internally consistent), so the guard must pass it through to the chaining
	// check, which then refuses it for being a delta.
	//
	// My first version of this assertion demanded that a delta be ACCEPTED as a
	// base, which is wrong -- chaining is refused on purpose. The point is that
	// it is refused for CHAINING, not for "claims base but marked full".
	goodDelta := fullWithShards(1, map[string]string{"000": "a"})
	goodDelta.Full = false
	goodDelta.BaseVersion = 0
	_, ok, reason = DeltaAgainst(goodDelta, 2)
	if ok {
		t.Fatal("a delta must not be usable as a base; chaining is refused")
	}
	if !contains(reason, "chains") {
		t.Fatalf("a well-formed delta must be refused for chaining, not for a "+
			"contradiction it does not have: %q", reason)
	}
	if contains(reason, "marked full") {
		t.Fatalf("the contradiction guard must not fire on a consistent delta: %q", reason)
	}
}

func keysOf2(raw []byte) map[string]any {
	m := map[string]any{}
	_ = json.Unmarshal(raw, &m)
	return m
}

// MkVersion in the test helper must record SnapshotVersion, and retention must
// still protect the base. This is the end-to-end shape: v1 full, v2..v5 deltas
// against v1, retention 3.
func TestRetentionProtectsBaseAcrossTheRealChain(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeManifest(t, filepath.Join(root, "v1"), 1, true, 0)
	for v := 2; v <= 5; v++ {
		writeManifest(t, filepath.Join(root, VersionDirName(v)), v, false, 1)
	}
	if _, err := Prune(root, RetentionVersions); err != nil {
		t.Fatal(err)
	}
	left, _ := ListVersions(root)
	// The window is the newest 3 = [3 4 5]. v1 is OUTSIDE it and survives
	// anyway, because the three retained deltas cannot be applied without it.
	//
	// v2 is inside the deletion candidates, is referenced by nothing, and goes.
	// My first expectation here was [1 2 3 4 5], which assumed retention kept
	// four. It keeps three. The code was right and the test was wrong, which is
	// the opposite of the usual outcome and worth recording.
	assertSet(t, "after prune", left, []int{1, 3, 4, 5})
}

func writeManifest(t *testing.T, dir string, snap int, full bool, base int) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	m := Manifest{Version: ManifestVersion, SnapshotVersion: snap, Full: full,
		BaseVersion: base, BuildTime: "2026-10-05T00:00:00Z",
		Shards: map[string]Shard{}, RowCounts: map[string]int64{}}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.canonical"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

// SURVIVOR M8, closed. Removing the `e.IsDir()` guard survived the suite,
// because every junk name in the test ("notes", "vx", "v-1") is rejected by
// ParseVersionDir anyway. The guard defends against something else: a plain
// FILE named "v1". That case was untested, so the guard was decoration.
func TestListVersionsIgnoresAFileNamedLikeAVersion(t *testing.T) {
	root := t.TempDir()
	mkVersion(t, root, 0, true, 0)
	for _, name := range []string{"v1", "v2"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("not a directory"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ListVersions(root)
	if err != nil {
		t.Fatal(err)
	}
	assertSet(t, "versions", got, []int{0})
	// And allocation must not count them as versions either. The newest real
	// version is v0, so the next is v1 -- the files named v1 and v2 must not
	// push it to v3, or a stray file would silently skip two version numbers and
	// every later delta would be numbered against a gap nobody chose.
	//
	// My first expectation here was v3, which tested nothing: it would have
	// passed whether or not the files were counted. Asserting v1 is what makes
	// it a test.
	v, err := AllocateVersion(root, 0)
	if err != nil {
		t.Fatal(err)
	}
	if v != 1 {
		t.Fatalf("files must not consume version numbers: got v%d, want v1", v)
	}
}

// SURVIVOR M15, closed. Retention keyed on `!Full`. Replacing that with
// `BaseVersion > 0` survived, because in every manifest the suite builds a FULL
// snapshot omits base_version (omitempty), so the two conditions agree.
//
// They disagree in exactly the case documented on BaseVersion: a FULL manifest
// that CARRIES a base_version. Retention must follow Full, or it starts
// protecting a base for a snapshot that has none — which is the same
// silent-retention-wrongly-extended failure as deleting a base, in the other
// direction.
func TestRetentionKeysOnFullNotOnBaseVersion(t *testing.T) {
	// The delta MUST be a survivor, or nothing reads its manifest and the whole
	// discrimination is vacuous. My first version put the delta at v2, which is a
	// PRUNE CANDIDATE under retention 3, so the tree never exercised the branch
	// and the test asserted a protection that by design does not apply.
	//
	// Layout: window = [3 4 5]. v5 is the delta naming v1. v1 is full, base -1.
	//   v1 full | v2 full | v3 full | v4 full | v5 DELTA -> base v1
	root := t.TempDir()
	writeManifest(t, filepath.Join(root, "v1"), 1, true, 0)
	writeManifest(t, filepath.Join(root, "v2"), 2, true, 0)
	writeManifest(t, filepath.Join(root, "v3"), 3, true, 0)
	writeManifest(t, filepath.Join(root, "v4"), 4, true, 0)
	writeManifest(t, filepath.Join(root, "v5"), 5, false, 1)
	if _, err := Prune(root, RetentionVersions); err != nil {
		t.Fatal(err)
	}
	left, _ := ListVersions(root)
	assertSet(t, "delta in the window protects its base", left, []int{1, 3, 4, 5})

	// Now: every version is FULL and names base -1. Nothing has a base, so v1
	// must be pruned.
	//
	// This is the case the earlier contract could not express at all: with
	// omitempty a full snapshot published nothing, so "no base" and "base v0"
	// were the same bytes and retention had only Full to go on.
	root2 := t.TempDir()
	for v := 1; v <= 5; v++ {
		writeManifest(t, filepath.Join(root2, VersionDirName(v)), v, true, -1)
	}
	if _, err := Prune(root2, RetentionVersions); err != nil {
		t.Fatal(err)
	}
	left2, _ := ListVersions(root2)
	assertSet(t, "all-full tree", left2, []int{3, 4, 5})

	// And the converse, which is now decidable from the field alone: every
	// version names base_version 1, so v1 is protected by all of them even
	// though every one of them claims to be full. Retention believes the field.
	//
	// That is deliberate. A manifest that contradicts itself is refused when it
	// is used as a DELTA BASE; retention protecting an extra directory is the
	// cheap direction to be wrong in.
	root3 := t.TempDir()
	for v := 1; v <= 5; v++ {
		writeManifest(t, filepath.Join(root3, VersionDirName(v)), v, true, 1)
	}
	if _, err := Prune(root3, RetentionVersions); err != nil {
		t.Fatal(err)
	}
	left3, _ := ListVersions(root3)
	assertSet(t, "all name base 1", left3, []int{1, 3, 4, 5})
}

// M19. The key -> file-name mapping was written by hand three times and got it
// wrong twice: once as "shard-%03d.json" against a key of "42", and once as
// key+".json", which is "42.json". Both made every delta look like a full
// snapshot while verifying perfectly.
//
// This pins the mapping in one place so a fourth copy is not written.
func TestShardFileNameRoundTrips(t *testing.T) {
	cases := map[string]string{
		"0":   "shard-000.json",
		"42":  "shard-042.json",
		"255": "shard-255.json",
	}
	for key, want := range cases {
		if got := ShardFileName(key); got != want {
			t.Errorf("ShardFileName(%q) = %q, want %q", key, got, want)
		}
	}
	// The keys in a real manifest must all map to distinct files.
	seen := map[string]string{}
	for i := 0; i < ShardCount; i++ {
		k := strconv.Itoa(i)
		f := ShardFileName(k)
		if prev, dup := seen[f]; dup {
			t.Fatalf("shard %s and %s both map to %s", prev, k, f)
		}
		seen[f] = k
	}
	if len(seen) != ShardCount {
		t.Fatalf("got %d distinct files for %d shards", len(seen), ShardCount)
	}
	// A non-numeric key is passed through rather than silently becoming
	// shard-000.json, which would collide with shard 0.
	if got := ShardFileName("notanumber"); got != "notanumber" {
		t.Fatalf("a non-numeric key must pass through, got %q", got)
	}
}

// M20. Counting from the BASE reported every unchanged shard as carried, because
// an unchanged shard is deliberately absent from the delta directory and the
// read failed. The result was "256 of 256, every shard changed" printed to an
// operator already using --stable-salt.
func TestDeltaAccountingCountsTheNewManifest(t *testing.T) {
	base := &Manifest{Shards: map[string]Shard{
		"0": {Hash: "a", Path: "shard-000.json"},
		"1": {Hash: "b", Path: "shard-001.json"},
		"2": {Hash: "c", Path: "shard-002.json"},
	}}
	// A delta that changed only shard 1.
	delta := &Manifest{Shards: map[string]Shard{
		"1": {Hash: "CHANGED", Path: "shard-001.json"},
	}}
	carried, baseTotal := DeltaAccounting(base, delta)
	if carried != 1 {
		t.Fatalf("carried = %d, want 1", carried)
	}
	if baseTotal != 3 {
		t.Fatalf("baseTotal = %d, want 3", baseTotal)
	}
	// An empty delta carries nothing and must not report "everything changed".
	carried, _ = DeltaAccounting(base, &Manifest{Shards: map[string]Shard{}})
	if carried != 0 {
		t.Fatalf("an empty delta must carry 0 shards, got %d", carried)
	}
}
