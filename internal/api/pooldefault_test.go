package api

// The default pool mode, tested where it is actually decided.
//
// The measurement that chose it (2026-10-23: pool recall@1000 0.1895 ->
// 0.2113, recall@10 0.1031 -> 0.1050 on the same 38 readers) is recorded in
// engine.DefaultPoolMode's comment. This file is the other half: proving the
// default is what an unqualified request gets, and that asking for the old
// behaviour still gives it.
//
// Both halves matter. A default nobody can opt out of is a removed parameter,
// and a default that is silently not applied is how "the parameter is missing"
// and "the parameter says tags" became the same request in the first place.

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"math"
	"net/http"
	"testing"
)

func putVecs2(t *testing.T, db *sql.DB, dim int, vecs map[int64][]float32) {
	t.Helper()
	for id, v := range vecs {
		blob := make([]byte, dim*4)
		for i, x := range v {
			binary.LittleEndian.PutUint32(blob[i*4:], math.Float32bits(x))
		}
		if _, err := db.ExecContext(context.Background(),
			`INSERT OR REPLACE INTO embeddings(kind, entity_id, dim, vec) VALUES('tag',?,?,?)`,
			id, dim, blob); err != nil {
			t.Fatalf("insert embedding %d: %v", id, err)
		}
	}
}

// TestRecommendDefaultsToTheMeasuredPoolMode: a request that names no pool
// mode must get the default, and the default's sources must be visible.
func TestRecommendDefaultsToTheMeasuredPoolMode(t *testing.T) {
	ts, s := newTestServerFull(t)
	putVecs2(t, s.DB, 2, map[int64][]float32{
		1: {1, 0}, 2: {0.99, 0.1}, 3: {0.5, 0.8}, 4: {0, 1}, 5: {0.95, 0.05},
	})

	var body struct {
		Meta struct {
			PoolSize    int `json:"pool_size"`
			PoolSources *struct {
				Tags      int `json:"tags"`
				Collab    int `json:"collab"`
				Embedding int `json:"embedding"`
			} `json:"pool_sources"`
		} `json:"meta"`
	}
	res, err := http.Get(ts.URL + "/api/v1/recommend?seed=ao3_work:1&n=5")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("recommend -> %d", res.StatusCode)
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Meta.PoolSources == nil {
		t.Fatal("meta.pool_sources missing; the breakdown is the only way to see " +
			"which sources contributed")
	}
	if body.Meta.PoolSources.Embedding == 0 {
		t.Fatalf("an unqualified request contributed no embedding candidates (%+v): "+
			"the default pool mode is not being applied", body.Meta.PoolSources)
	}
}

// TestPoolModeTagsStillMeansTagsOnly: the opt-out must work, or the default is
// not a choice.
func TestPoolModeTagsStillMeansTagsOnly(t *testing.T) {
	ts, s := newTestServerFull(t)
	putVecs2(t, s.DB, 2, map[int64][]float32{
		1: {1, 0}, 2: {0.99, 0.1}, 3: {0.5, 0.8}, 4: {0, 1}, 5: {0.95, 0.05},
	})
	var body struct {
		Meta struct {
			PoolSources *struct {
				Embedding int `json:"embedding"`
				Collab    int `json:"collab"`
			} `json:"pool_sources"`
		} `json:"meta"`
	}
	res, err := http.Get(ts.URL + "/api/v1/recommend?seed=ao3_work:1&n=5&pool_mode=tags")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Meta.PoolSources == nil {
		t.Fatal("meta.pool_sources missing")
	}
	if body.Meta.PoolSources.Embedding != 0 || body.Meta.PoolSources.Collab != 0 {
		t.Fatalf("pool_mode=tags contributed %+v; the explicit opt-out is ignored",
			body.Meta.PoolSources)
	}
}
