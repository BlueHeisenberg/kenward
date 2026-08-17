package config

import (
	"reflect"
	"regexp"
	"strings"
)

// unknownField matches yaml.v3's strict-decoder complaint, one per offending line.
//
// The message is accurate and useless to the person holding the file: it names a Go
// type they do not have — config.HouseholdConfig — rather than the block they wrote,
// and it says nothing about the key belonging somewhere else in the same document,
// which is the commonest reason it is there at all. deploy/compose.isolated.yml told
// operators to put the household group's bot_token_env "per member, plus one for the
// household group"; read beside `household:`, that produced exactly this error and cost
// a tester two failed starts.
var unknownField = regexp.MustCompile(`field ([A-Za-z0-9_]+) not found in type`)

// hintMisplacedFields appends, to a strict-decoding error, the block each rejected key
// does belong under — when it belongs under one at all.
//
// A key with no home anywhere is a typo, and it is left with the plain refusal. Sending
// somebody to a block that will reject the key just as hard is worse than saying
// nothing, so the homes are read off the schema itself rather than listed by hand: a
// field moved between blocks moves this advice with it, and a field deleted stops being
// advertised.
func hintMisplacedFields(err error) error {
	matches := unknownField.FindAllStringSubmatch(err.Error(), -1)
	if len(matches) == 0 {
		return err
	}
	homes := fieldHomes()
	var seen, hints []string
	for _, m := range matches {
		key := m[1]
		where, ok := homes[key]
		if !ok || contains(seen, key) {
			continue
		}
		seen = append(seen, key)
		hints = append(hints, key+" belongs under "+where)
	}
	if len(hints) == 0 {
		return err
	}
	return &hintedError{err: err, hint: strings.Join(hints, "; ")}
}

type hintedError struct {
	err  error
	hint string
}

func (h *hintedError) Error() string { return h.err.Error() + "\n  " + h.hint }
func (h *hintedError) Unwrap() error { return h.err }

// fieldHomes maps every yaml key in the schema to the block, or blocks, it is valid in.
//
// Walked with reflection over Config rather than written down, for the reason above: a
// hand-maintained table is a second copy of the schema, and the first thing a second
// copy does is disagree with the first.
func fieldHomes() map[string]string {
	homes := map[string][]string{}
	var walk func(t reflect.Type, path string)
	walk = func(t reflect.Type, path string) {
		for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice {
			t = t.Elem()
		}
		if t.Kind() != reflect.Struct {
			return
		}
		for i := range t.NumField() {
			f := t.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("yaml"), ",")
			if name == "" || name == "-" {
				continue
			}
			where := path
			if where == "" {
				where = "the top level"
			}
			if !contains(homes[name], where) {
				homes[name] = append(homes[name], where)
			}
			child := name + ":"
			if path != "" && path != "the top level" {
				child = strings.TrimSuffix(path, ":") + "." + name + ":"
			}
			walk(f.Type, child)
		}
	}
	walk(reflect.TypeOf(Config{}), "")

	out := make(map[string]string, len(homes))
	for name, where := range homes {
		out[name] = strings.Join(where, " or ")
	}
	return out
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
