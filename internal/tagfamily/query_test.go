package tagfamily

import (
	"context"
	"testing"
	"time"
)

func TestQueryIntersectsTwoFamiliesAndKeepsOnlyRecentWorks(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	mustExec(t, db, `CREATE TABLE works (id INTEGER PRIMARY KEY, title TEXT, url TEXT, authors TEXT,
		kudos INTEGER, word_count INTEGER, rating TEXT, complete INTEGER, update_date TEXT)`)
	mustExec(t, db, `CREATE TABLE tags (id INTEGER PRIMARY KEY, name TEXT NOT NULL)`)
	mustExec(t, db, `CREATE TABLE work_tags (work_id INTEGER, tag_id INTEGER, tag_type TEXT)`)
	mustExec(t, db, `INSERT INTO tags (id,name) VALUES
		(1,'worm - fandom'),(2,'isekai'),(3,'harry potter - all media types')`)
	mustExec(t, db, `INSERT INTO work_tags (work_id,tag_id,tag_type) VALUES
		(10,1,'freeforms'),(10,2,'freeforms'),
		(11,1,'freeforms'),(11,3,'freeforms'),
		(12,1,'freeforms'),(12,2,'freeforms')`)

	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	mustExec(t, db, `INSERT INTO works (id,title,url,authors,kudos,word_count,rating,complete,update_date) VALUES
		(10,'both and recent','u','a',100,1000,'General',1,'01 Oct 2026'),
		(11,'only the fandom','u','a',100,1000,'General',1,'01 Oct 2026'),
		(12,'both but stale','u','a',100,1000,'General',1,'01 Oct 2024')`)

	res, err := Query(context.Background(), db, QueryOptions{
		UniverseTags: []string{"worm - fandom"},
		ConceptTags:  []string{"isekai"},
		RecentDays:   365,
		Now:          now,
		Limit:        10,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Items) != 1 {
		t.Fatalf("got %d items, want 1: %+v", len(res.Items), res.Items)
	}
	got := res.Items[0]
	if got.ID != 10 {
		t.Errorf("ID = %d, want 10", got.ID)
	}
	if got.AgeDays != 20 {
		t.Errorf("AgeDays = %d, want 20", got.AgeDays)
	}
	if len(got.MatchedTags) != 1 || got.MatchedTags[0] != "isekai" {
		t.Errorf("MatchedTags = %v, want [isekai]", got.MatchedTags)
	}
	if res.Meta.UniverseWorks != 3 {
		t.Errorf("UniverseWorks = %d, want 3", res.Meta.UniverseWorks)
	}
	if res.Meta.ConceptWorks != 2 {
		t.Errorf("ConceptWorks = %d, want 2", res.Meta.ConceptWorks)
	}
}

func TestQueryReportsAWorkThatAlsoCarriesAnotherFandom(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()
	mustExec(t, db, `CREATE TABLE works (id INTEGER PRIMARY KEY, title TEXT, url TEXT, authors TEXT,
		kudos INTEGER, word_count INTEGER, rating TEXT, complete INTEGER, update_date TEXT)`)
	mustExec(t, db, `CREATE TABLE tags (id INTEGER PRIMARY KEY, name TEXT NOT NULL)`)
	mustExec(t, db, `CREATE TABLE work_tags (work_id,tag_id,tag_type TEXT)`)
	mustExec(t, db, `INSERT INTO tags (id,name) VALUES
		(1,'worm - fandom'),(2,'isekai'),(3,'harry potter - all media types')`)
	mustExec(t, db, `INSERT INTO work_tags (work_id,tag_id,tag_type) VALUES
		(10,1,'freeforms'),(10,2,'freeforms'),(10,3,'freeforms')`)
	now := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	mustExec(t, db, `INSERT INTO works (id,title,url,authors,kudos,word_count,rating,complete,update_date)
		VALUES (10,'crossover','u','a',100,1000,'General',1,'01 Oct 2026')`)

	res, err := Query(context.Background(), db, QueryOptions{
		UniverseTags: []string{"worm - fandom"},
		ConceptTags:  []string{"isekai"},
		RecentDays:   365,
		Now:          now,
		Limit:        10,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(res.Items))
	}
	if res.Items[0].OtherFandom != "harry potter" {
		t.Errorf("OtherFandom = %q, want %q", res.Items[0].OtherFandom, "harry potter")
	}
}
