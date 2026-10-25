package api

// The themes routes (#47). The failure mode this guards is a route that answers 200 with an
// empty list on a deployment that has never clustered anything: a reader cannot tell "no themes
// exist" from "the build is missing", and those are different facts.

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestThemesSaysSoWhenNothingIsBuilt(t *testing.T) {
	ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/api/v1/themes")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (an unbuilt tree is not an error)", res.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if built, _ := body["built"].(bool); built {
		t.Fatalf("built = true on a database with no work_family table")
	}
	note, _ := body["note"].(string)
	if note == "" {
		t.Fatalf("an unbuilt tree returned no note: a reader sees an empty list and no explanation")
	}
	themes, _ := body["themes"].([]any)
	if len(themes) != 0 {
		t.Fatalf("themes = %v, want an empty list beside built:false", themes)
	}
}

func TestThemeWorks404sWithoutATree(t *testing.T) {
	ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/api/v1/themes/1")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a theme on a deployment with no tree", res.StatusCode)
	}
}

func TestThemeIDMustBeANumber(t *testing.T) {
	ts := newTestServer(t)
	res, err := http.Get(ts.URL + "/api/v1/themes/not-a-number")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
}

// With a tree present the routes read it: sizes, labels and the sample come from the tables,
// not from the request.
func TestThemesReadTheBuiltTree(t *testing.T) {
	ts, st := newTestServerFull(t)
	if _, err := st.DB.Exec(`
		CREATE TABLE IF NOT EXISTS work_family (id INTEGER PRIMARY KEY, parent INTEGER, label TEXT, size INTEGER);
		CREATE TABLE IF NOT EXISTS work_family_member (work_id INTEGER PRIMARY KEY, family INTEGER, dist REAL);
		INSERT INTO work_family (id, parent, label, size) VALUES (5, NULL, 'draco malfoy/harry potter', 1200);
		INSERT INTO work_family (id, parent, label, size) VALUES (9, NULL, 'naruto uzumaki', 300);
		INSERT INTO work_family_member (work_id, family, dist) VALUES (1, 5, 0.1);
		INSERT INTO work_family_member (work_id, family, dist) VALUES (2, 9, 0.2);`); err != nil {
		t.Fatal(err)
	}
	res, err := http.Get(ts.URL + "/api/v1/themes")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if built, _ := body["built"].(bool); !built {
		t.Fatalf("built = false with two families in the table")
	}
	themes, _ := body["themes"].([]any)
	if len(themes) != 2 {
		t.Fatalf("themes = %d, want 2", len(themes))
	}
	first, _ := themes[0].(map[string]any)
	if first["id"].(float64) != 5 {
		t.Fatalf("first theme is %v, want the BIGGEST (5, 1,200 works)", first["id"])
	}
	if first["label"] != "draco malfoy/harry potter" {
		t.Fatalf("label = %v", first["label"])
	}

	res2, err := http.Get(ts.URL + "/api/v1/themes/9")
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("status = %d for an existing theme", res2.StatusCode)
	}
	var one map[string]any
	_ = json.NewDecoder(res2.Body).Decode(&one)
	if one["label"] != "naruto uzumaki" {
		t.Fatalf("theme 9 label = %v", one["label"])
	}
}
