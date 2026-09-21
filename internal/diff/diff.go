// Package diff compares two captured requests structurally.
//
// By path into the data, not by line of text: object keys have no order, so
// a line diff of two semantically identical payloads can report every key as
// changed. See docs/learn/28-structural-diff.md.
package diff

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Op is what happened to a path.
type Op string

const (
	Added   Op = "added"
	Removed Op = "removed"
	Changed Op = "changed"
)

// Change is one difference.
type Change struct {
	// Path is a dotted path with bracketed indices: data.object.items[0].id
	Path string `json:"path"`
	Op   Op     `json:"op"`
	// Old and New are JSON-encoded values, or empty for Added/Removed
	// respectively.
	//
	// Encoded rather than `any` so the caller renders exactly what was
	// there: `"2500"` and `2500` are different values that would both print
	// as 2500 through a naive formatter, and telling a string from a number
	// is frequently the point of the diff.
	Old string `json:"old,omitempty"`
	New string `json:"new,omitempty"`
}

// Result is the whole comparison.
type Result struct {
	Changes []Change `json:"changes"`
	// Identical is true when there are no changes at all. A separate field
	// rather than len(Changes)==0 at the call site, because "identical" is
	// an answer worth stating and an empty list looks like a failure.
	Identical bool `json:"identical"`
	// Note explains a comparison that could not be structural.
	Note string `json:"note,omitempty"`
}

// Bodies compares two raw bodies.
//
// Both are parsed as JSON. When either is not JSON there is nothing
// structural to compare, so it falls back to a whole-body comparison and
// says so -- a wrong answer dressed as a structural diff would be worse
// than an honest "these are not JSON".
func Bodies(left, right []byte) Result {
	l, lok := decode(left)
	r, rok := decode(right)

	if !lok || !rok {
		if string(left) == string(right) {
			return Result{Identical: true, Note: "not JSON; compared as bytes"}
		}
		return Result{
			Changes: []Change{{Path: "(body)", Op: Changed,
				Old: string(left), New: string(right)}},
			Note: "not JSON; compared as bytes",
		}
	}

	var changes []Change
	walk("", l, r, &changes)
	sortChanges(changes)
	return Result{Changes: changes, Identical: len(changes) == 0}
}

// decode parses JSON while preserving number tokens exactly.
//
// UseNumber, not the default. encoding/json decodes numbers into float64
// otherwise, which makes 2500 and 2500.0 indistinguishable and silently
// loses precision on integers above 2^53 -- so two payloads differing in the
// last digit of a large id would compare EQUAL. json.Number keeps the
// original token, which is what a diff should be comparing anyway.
func decode(b []byte) (any, bool) {
	if len(b) == 0 {
		return nil, false
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.UseNumber()

	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	// Reject trailing content: `{"a":1} garbage` decodes the object and
	// leaves the rest, which would make a malformed body look structured.
	if dec.More() {
		return nil, false
	}
	return v, true
}

func walk(path string, l, r any, out *[]Change) {
	switch lv := l.(type) {
	case map[string]any:
		rv, ok := r.(map[string]any)
		if !ok {
			*out = append(*out, change(path, l, r))
			return
		}
		walkObjects(path, lv, rv, out)

	case []any:
		rv, ok := r.([]any)
		if !ok {
			*out = append(*out, change(path, l, r))
			return
		}
		walkArrays(path, lv, rv, out)

	default:
		// Scalars, including nil. Compared through their encoded form so
		// that null-vs-absent and "2500"-vs-2500 stay distinguishable.
		if encode(l) != encode(r) {
			*out = append(*out, change(path, l, r))
		}
	}
}

func walkObjects(path string, l, r map[string]any, out *[]Change) {
	// The union of both key sets, SORTED. Not for looks: Go randomises map
	// iteration deliberately, so without this the same two documents
	// produce a differently ordered diff on every run -- untestable, and
	// impossible to compare against a previous run by eye.
	keys := make([]string, 0, len(l)+len(r))
	seen := make(map[string]bool, len(l)+len(r))
	for k := range l {
		keys, seen[k] = append(keys, k), true
	}
	for k := range r {
		if !seen[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	for _, k := range keys {
		lv, inL := l[k]
		rv, inR := r[k]
		p := join(path, k)

		switch {
		case !inR:
			*out = append(*out, Change{Path: p, Op: Removed, Old: encode(lv)})
		case !inL:
			*out = append(*out, Change{Path: p, Op: Added, New: encode(rv)})
		default:
			walk(p, lv, rv, out)
		}
	}
}

// walkArrays matches elements by id when it can, and by index when it
// cannot.
//
// The choice matters more than it looks. By index, inserting one element at
// the front reports EVERY subsequent element as changed -- technically true
// and useless. Matching on a stable id gives the answer a person wanted.
// The heuristic can be wrong, so it only applies when every element on both
// sides has a unique id; otherwise index, which is at least predictable.
func walkArrays(path string, l, r []any, out *[]Change) {
	lIDs, lok := elementIDs(l)
	rIDs, rok := elementIDs(r)

	if lok && rok {
		walkArraysByID(path, l, r, lIDs, rIDs, out)
		return
	}

	for i := range max(len(l), len(r)) {
		p := fmt.Sprintf("%s[%d]", path, i)
		switch {
		case i >= len(r):
			*out = append(*out, Change{Path: p, Op: Removed, Old: encode(l[i])})
		case i >= len(l):
			*out = append(*out, Change{Path: p, Op: Added, New: encode(r[i])})
		default:
			walk(p, l[i], r[i], out)
		}
	}
}

func walkArraysByID(path string, l, r []any, lIDs, rIDs []string, out *[]Change) {
	rByID := make(map[string]int, len(r))
	for i, id := range rIDs {
		rByID[id] = i
	}

	matched := make(map[string]bool, len(l))
	for i, id := range lIDs {
		j, ok := rByID[id]
		if !ok {
			*out = append(*out, Change{
				Path: fmt.Sprintf("%s[id=%s]", path, id), Op: Removed, Old: encode(l[i])})
			continue
		}
		matched[id] = true
		// The path names the id rather than either index, because the
		// element may have moved and a positional path would be a lie.
		walk(fmt.Sprintf("%s[id=%s]", path, id), l[i], r[j], out)
	}
	for i, id := range rIDs {
		if !matched[id] {
			*out = append(*out, Change{
				Path: fmt.Sprintf("%s[id=%s]", path, id), Op: Added, New: encode(r[i])})
		}
	}
}

// elementIDs returns each element's identity, or false if the heuristic does
// not apply.
//
// Applies only when EVERY element is an object carrying a scalar id under a
// recognised key, and the ids are unique. A partial match would mean mixing
// two matching strategies in one array, which produces output nobody can
// reason about.
func elementIDs(items []any) ([]string, bool) {
	if len(items) == 0 {
		return nil, false
	}
	ids := make([]string, 0, len(items))
	seen := make(map[string]bool, len(items))

	for _, it := range items {
		obj, ok := it.(map[string]any)
		if !ok {
			return nil, false
		}
		var id string
		// In priority order. "id" first because it is overwhelmingly the
		// most common; the others cover the providers this tool targets.
		for _, key := range []string{"id", "uuid", "key", "name"} {
			if v, present := obj[key]; present {
				if s, isScalar := scalarString(v); isScalar {
					id = s
					break
				}
			}
		}
		if id == "" || seen[id] {
			return nil, false
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids, true
}

func scalarString(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case json.Number:
		return t.String(), true
	case bool:
		return strconv.FormatBool(t), true
	}
	return "", false
}

func change(path string, l, r any) Change {
	return Change{Path: path, Op: Changed, Old: encode(l), New: encode(r)}
}

// encode renders a value as JSON for display and comparison.
func encode(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		// Unreachable for anything that came out of json.Unmarshal, and
		// rendered rather than dropped if it ever happens: a diff that
		// silently omits a value is worse than one showing a Go type.
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// sortChanges puts the output in a stable, readable order.
func sortChanges(c []Change) {
	sort.SliceStable(c, func(i, j int) bool { return c[i].Path < c[j].Path })
}
