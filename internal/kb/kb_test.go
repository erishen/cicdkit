package kb

import "testing"

func TestAdd_DedupIncrementsCount(t *testing.T) {
	k := Load("") // in-memory, no file
	e := Entry{ProjectID: "p1", Stage: "build", ErrorKeyword: "docker: denied", Diagnosis: "fix perm"}
	saved := k.Add(e)
	if saved.ID == "" {
		t.Fatal("id empty")
	}
	if saved.AdoptedCount != 1 {
		t.Fatalf("count=%d want 1", saved.AdoptedCount)
	}
	// same signature -> merge, not a new entry
	e2 := Entry{ProjectID: "p1", Stage: "build", ErrorKeyword: "docker: denied", Diagnosis: "fix perm longer detail"}
	saved2 := k.Add(e2)
	if saved2.ID != saved.ID {
		t.Fatalf("expected merge, got new id %s", saved2.ID)
	}
	if saved2.AdoptedCount != 2 {
		t.Fatalf("count=%d want 2", saved2.AdoptedCount)
	}
	if saved2.Diagnosis != e2.Diagnosis {
		t.Fatalf("expected longer diagnosis kept, got %q", saved2.Diagnosis)
	}
}

func TestList_OrderByAdoptedAtDesc(t *testing.T) {
	k := Load("")
	k.entries = []Entry{
		{ID: "1", ProjectID: "a", AdoptedAt: "2020-01-01T00:00:00Z"},
		{ID: "2", ProjectID: "b", AdoptedAt: "2024-01-01T00:00:00Z"},
	}
	list := k.List()
	if len(list) != 2 {
		t.Fatalf("len=%d", len(list))
	}
	if list[0].ProjectID != "b" {
		t.Fatalf("expected newest first, got %s", list[0].ProjectID)
	}
}

func TestMatch_SubstringHit(t *testing.T) {
	k := Load("")
	k.Add(Entry{ProjectID: "a", ErrorKeyword: "manifest unknown", Diagnosis: "d"})
	hits := k.Match("error: manifest unknown: failed to pull")
	if len(hits) != 1 {
		t.Fatalf("hits=%d want 1", len(hits))
	}
	none := k.Match("everything fine")
	if len(none) != 0 {
		t.Fatalf("none=%d want 0", len(none))
	}
}

func TestNormalize_CollapsesAndLowers(t *testing.T) {
	got := Normalize("  Docker  DENIED\tpermission ")
	if got != "docker denied permission" {
		t.Fatalf("got %q", got)
	}
}

func TestRemove_ById(t *testing.T) {
	k := Load("")
	a := k.Add(Entry{ProjectID: "p1", Stage: "build", ErrorKeyword: "x", Diagnosis: "d1"})
	k.Add(Entry{ProjectID: "p2", Stage: "push", ErrorKeyword: "y", Diagnosis: "d2"})
	if len(k.entries) != 2 {
		t.Fatalf("len=%d want 2", len(k.entries))
	}
	if !k.Remove(a.ID) {
		t.Fatal("Remove returned false for existing id")
	}
	if len(k.entries) != 1 {
		t.Fatalf("len=%d want 1 after remove", len(k.entries))
	}
	if k.entries[0].ProjectID != "p2" {
		t.Fatalf("wrong remaining entry: %s", k.entries[0].ProjectID)
	}
	if k.Remove("nope") {
		t.Fatal("Remove returned true for missing id")
	}
}
