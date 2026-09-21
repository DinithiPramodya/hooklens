package diff

import (
	"strings"
	"testing"
)

func paths(r Result) []string {
	out := make([]string, len(r.Changes))
	for i, c := range r.Changes {
		out[i] = string(c.Op) + " " + c.Path
	}
	return out
}

func find(t *testing.T, r Result, path string) Change {
	t.Helper()
	for _, c := range r.Changes {
		if c.Path == path {
			return c
		}
	}
	t.Fatalf("no change at %q; got %v", path, paths(r))
	return Change{}
}

// TestKeyOrderIsNotAChange is the reason this package exists instead of a
// text diff. Two semantically identical payloads, serialised differently.
func TestKeyOrderIsNotAChange(t *testing.T) {
	l := []byte(`{"a":1,"b":2,"c":{"d":3,"e":4}}`)
	r := []byte(`{"c":{"e":4,"d":3},"b":2,"a":1}`)

	res := Bodies(l, r)
	if !res.Identical {
		t.Errorf("reordered keys reported as changes: %v", paths(res))
	}
}

// TestWhitespaceIsNotAChange: the other half of the same argument.
func TestWhitespaceIsNotAChange(t *testing.T) {
	res := Bodies([]byte(`{"a":1}`), []byte("{\n  \"a\" : 1\n}"))
	if !res.Identical {
		t.Errorf("whitespace reported as changes: %v", paths(res))
	}
}

// TestPlanAcceptanceCriterion: "diffing two payments highlights only the
// changed amount".
func TestPlanAcceptanceCriterion(t *testing.T) {
	l := []byte(`{"id":"evt_1","type":"payment_intent.succeeded","data":{"object":{"id":"pi_1","amount":2500,"currency":"usd","status":"succeeded"}}}`)
	r := []byte(`{"id":"evt_1","type":"payment_intent.succeeded","data":{"object":{"id":"pi_1","amount":9900,"currency":"usd","status":"succeeded"}}}`)

	res := Bodies(l, r)

	if len(res.Changes) != 1 {
		t.Fatalf("got %d changes, want exactly 1: %v", len(res.Changes), paths(res))
	}
	c := res.Changes[0]
	if c.Path != "data.object.amount" {
		t.Errorf("path = %q, want data.object.amount", c.Path)
	}
	if c.Old != "2500" || c.New != "9900" {
		t.Errorf("got %s -> %s, want 2500 -> 9900", c.Old, c.New)
	}
}

func TestAddedAndRemoved(t *testing.T) {
	res := Bodies([]byte(`{"keep":1,"gone":2}`), []byte(`{"keep":1,"fresh":3}`))

	if got := find(t, res, "gone"); got.Op != Removed || got.Old != "2" {
		t.Errorf("gone: %+v", got)
	}
	if got := find(t, res, "fresh"); got.Op != Added || got.New != "3" {
		t.Errorf("fresh: %+v", got)
	}
}

// TestNullIsNotAbsent: different states, and they must not render the same.
func TestNullIsNotAbsent(t *testing.T) {
	res := Bodies([]byte(`{"a":null}`), []byte(`{}`))
	c := find(t, res, "a")
	if c.Op != Removed {
		t.Errorf("op = %s, want removed", c.Op)
	}
	if c.Old != "null" {
		t.Errorf("old = %q, want null", c.Old)
	}

	// And null -> a value is a change, not an addition.
	res2 := Bodies([]byte(`{"a":null}`), []byte(`{"a":1}`))
	if c := find(t, res2, "a"); c.Op != Changed {
		t.Errorf("op = %s, want changed", c.Op)
	}
}

// TestStringAndNumberAreDistinguishable. "2500" and 2500 are different
// values that a naive formatter prints identically -- and telling them
// apart is frequently the point of the diff.
func TestStringAndNumberAreDistinguishable(t *testing.T) {
	res := Bodies([]byte(`{"amount":2500}`), []byte(`{"amount":"2500"}`))
	c := find(t, res, "amount")
	if c.Old != "2500" || c.New != `"2500"` {
		t.Errorf("got %s -> %s; the quotes must survive", c.Old, c.New)
	}
}

// TestLargeIntegersKeepPrecision is why decode uses UseNumber. With the
// default float64 decoding these two ids compare EQUAL.
func TestLargeIntegersKeepPrecision(t *testing.T) {
	l := []byte(`{"id":9007199254740993}`)
	r := []byte(`{"id":9007199254740992}`)

	res := Bodies(l, r)
	if res.Identical {
		t.Fatal("two different 2^53-scale integers compared equal; precision was lost")
	}
	c := find(t, res, "id")
	if !strings.Contains(c.Old, "9007199254740993") {
		t.Errorf("old = %q, want the exact token", c.Old)
	}
}

// TestIntegerAndFloatFormAreDistinguishable: 2500 and 2500.0 are the same
// number and different tokens. Reporting them as a change is the honest
// answer for a tool that shows what was on the wire.
func TestIntegerAndFloatFormAreDistinguishable(t *testing.T) {
	if Bodies([]byte(`{"a":2500}`), []byte(`{"a":2500.0}`)).Identical {
		t.Error("2500 and 2500.0 compared equal; the wire form differs")
	}
}

func TestTypeChange(t *testing.T) {
	res := Bodies([]byte(`{"a":{"b":1}}`), []byte(`{"a":[1]}`))
	if c := find(t, res, "a"); c.Op != Changed {
		t.Errorf("op = %s, want changed for an object becoming an array", c.Op)
	}
}

// ---- arrays ----

// TestArrayByIndexOnInsertion documents the failure mode that motivates id
// matching: without it, one insertion changes everything after it.
func TestArrayByIndexOnInsertion(t *testing.T) {
	// No ids, so index matching applies.
	res := Bodies([]byte(`{"xs":[1,2,3]}`), []byte(`{"xs":[0,1,2,3]}`))

	// Every position differs, plus one addition. Technically true.
	if len(res.Changes) < 4 {
		t.Errorf("got %d changes, expected index matching to report every position: %v",
			len(res.Changes), paths(res))
	}
}

// TestArrayByIDSurvivesInsertion is the payoff. Same insertion, but the
// elements carry ids -- so only the new element is reported.
func TestArrayByIDSurvivesInsertion(t *testing.T) {
	l := []byte(`{"items":[{"id":"a","v":1},{"id":"b","v":2}]}`)
	r := []byte(`{"items":[{"id":"c","v":3},{"id":"a","v":1},{"id":"b","v":2}]}`)

	res := Bodies(l, r)

	if len(res.Changes) != 1 {
		t.Fatalf("got %d changes, want 1 addition: %v", len(res.Changes), paths(res))
	}
	c := res.Changes[0]
	if c.Op != Added {
		t.Errorf("op = %s, want added", c.Op)
	}
	if !strings.Contains(c.Path, "id=c") {
		t.Errorf("path = %q, want it to name the id rather than an index", c.Path)
	}
}

// TestArrayByIDMatchesMovedElements: only the changed field, even though
// the element moved position.
func TestArrayByIDMatchesMovedElements(t *testing.T) {
	l := []byte(`{"items":[{"id":"a","v":1},{"id":"b","v":2}]}`)
	r := []byte(`{"items":[{"id":"b","v":99},{"id":"a","v":1}]}`)

	res := Bodies(l, r)

	if len(res.Changes) != 1 {
		t.Fatalf("got %d changes, want 1: %v", len(res.Changes), paths(res))
	}
	c := res.Changes[0]
	if !strings.Contains(c.Path, "id=b") || !strings.Contains(c.Path, "v") {
		t.Errorf("path = %q, want items[id=b].v", c.Path)
	}
	if c.Old != "2" || c.New != "99" {
		t.Errorf("got %s -> %s", c.Old, c.New)
	}
}

// TestArrayIDHeuristicRequiresEveryElement: a partial match would mean two
// matching strategies in one array, which produces output nobody can reason
// about.
func TestArrayIDHeuristicRequiresEveryElement(t *testing.T) {
	// One element has no id.
	l := []byte(`{"items":[{"id":"a"},{"v":1}]}`)
	r := []byte(`{"items":[{"id":"a"},{"v":2}]}`)

	res := Bodies(l, r)
	for _, c := range res.Changes {
		if strings.Contains(c.Path, "id=") {
			t.Errorf("id matching applied to a mixed array: %q", c.Path)
		}
	}
	if len(res.Changes) != 1 {
		t.Errorf("got %v, want one indexed change", paths(res))
	}
}

func TestArrayIDHeuristicRequiresUniqueIDs(t *testing.T) {
	l := []byte(`{"items":[{"id":"a","v":1},{"id":"a","v":2}]}`)
	r := []byte(`{"items":[{"id":"a","v":1},{"id":"a","v":3}]}`)

	for _, c := range Bodies(l, r).Changes {
		if strings.Contains(c.Path, "id=") {
			t.Errorf("id matching applied to duplicate ids: %q", c.Path)
		}
	}
}

// ---- determinism ----

// TestOutputIsDeterministic. Go randomises map iteration deliberately, so
// without the sort the same two documents produce a differently ordered
// diff every run -- untestable, and impossible to compare by eye.
func TestOutputIsDeterministic(t *testing.T) {
	l := []byte(`{"a":1,"b":2,"c":3,"d":4,"e":5,"f":6,"g":7,"h":8}`)
	r := []byte(`{"a":9,"b":9,"c":9,"d":9,"e":9,"f":9,"g":9,"h":9}`)

	first := paths(Bodies(l, r))
	for range 50 {
		got := paths(Bodies(l, r))
		if len(got) != len(first) {
			t.Fatalf("change count varied between runs")
		}
		for i := range got {
			if got[i] != first[i] {
				t.Fatalf("order varied between runs:\n %v\n %v", first, got)
			}
		}
	}
}

// ---- non-JSON ----

func TestNonJSONBodies(t *testing.T) {
	res := Bodies([]byte("name=ada"), []byte("name=grace"))
	if res.Identical {
		t.Fatal("different form bodies compared equal")
	}
	if res.Note == "" {
		t.Error("no note explaining that the comparison was not structural")
	}
	if len(res.Changes) != 1 || res.Changes[0].Path != "(body)" {
		t.Errorf("got %v, want one whole-body change", paths(res))
	}
}

func TestIdenticalNonJSONBodies(t *testing.T) {
	res := Bodies([]byte("same"), []byte("same"))
	if !res.Identical {
		t.Error("identical non-JSON bodies reported as different")
	}
}

// TestTrailingGarbageIsNotStructured: a body like `{"a":1} oops` decodes an
// object and leaves the rest, which would make it look structured.
func TestTrailingGarbageIsNotStructured(t *testing.T) {
	res := Bodies([]byte(`{"a":1} oops`), []byte(`{"a":2}`))
	if res.Note == "" {
		t.Error("a body with trailing garbage was treated as valid JSON")
	}
}

func TestEmptyBodies(t *testing.T) {
	if !Bodies(nil, nil).Identical {
		t.Error("two empty bodies compared different")
	}
}

// ---- headers ----

func TestHeadersDiff(t *testing.T) {
	l := []Header{
		{"Content-Type", "application/json"},
		{"X-Gone", "1"},
		{"Set-Cookie", "a=1"},
		{"Set-Cookie", "b=2"},
	}
	r := []Header{
		{"content-type", "application/json"}, // case differs, value does not
		{"X-New", "2"},
		{"Set-Cookie", "a=1"},
	}

	changes := Headers(l, r)
	byPath := map[string]Change{}
	for _, c := range changes {
		byPath[c.Path] = c
	}

	// Case-insensitive: this must NOT appear.
	if _, ok := byPath["header:content-type"]; ok {
		t.Error("a header differing only in name casing was reported as a change")
	}
	if c, ok := byPath["header:x-gone"]; !ok || c.Op != Removed {
		t.Errorf("x-gone: %+v", c)
	}
	if c, ok := byPath["header:x-new"]; !ok || c.Op != Added {
		t.Errorf("x-new: %+v", c)
	}
	// Duplicates are compared as a group, and losing one is a change.
	c, ok := byPath["header:set-cookie"]
	if !ok || c.Op != Changed {
		t.Fatalf("set-cookie: %+v", c)
	}
	if !strings.Contains(c.Old, "b=2") || strings.Contains(c.New, "b=2") {
		t.Errorf("set-cookie: got %q -> %q, want the lost duplicate visible", c.Old, c.New)
	}
}

// TestWithoutVolatile: the filter is opt-in, because a diff that silently
// hides fields is a diff that lies.
func TestWithoutVolatile(t *testing.T) {
	changes := []Change{
		{Path: "header:date", Op: Changed},
		{Path: "header:stripe-signature", Op: Changed},
		{Path: "header:x-custom", Op: Changed},
		{Path: "data.amount", Op: Changed},
	}

	kept := WithoutVolatile(changes)
	if len(kept) != 2 {
		t.Fatalf("kept %d, want 2: %+v", len(kept), kept)
	}
	for _, c := range kept {
		if c.Path == "header:date" || c.Path == "header:stripe-signature" {
			t.Errorf("%q survived the filter", c.Path)
		}
	}
}
