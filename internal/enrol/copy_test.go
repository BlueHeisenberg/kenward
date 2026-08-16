package enrol

import "testing"

// TestTagForResolvesAHouseholdsLanguage covers the seam between two settings that are
// honestly different shapes: household.persona.language is free text and reaches the
// model, and this package's copy is a closed list somebody has actually written.
//
// The fallback is the point. A household writing "Brazilian Portuguese" gets a
// tutorial in English and a persona that still says Brazilian Portuguese, which is
// the honest outcome — the tutorial says so out loud where the member can see it.
func TestTagForResolvesAHouseholdsLanguage(t *testing.T) {
	for in, want := range map[string]string{
		"":                     LangEnglish,
		"English":              LangEnglish,
		"Spanish":              LangSpanish,
		"español":              LangSpanish,
		"  Español  ":          LangSpanish,
		"castellano":           LangSpanish,
		"Brazilian Portuguese": LangEnglish,
	} {
		if got := TagFor(in); got != want {
			t.Errorf("TagFor(%q) = %q, want %q", in, got, want)
		}
		if !Spoken(TagFor(in)) {
			t.Errorf("TagFor(%q) returned a tag this package holds no copy for", in)
		}
	}
	// And what the tutorial records round-trips: it stores the language's own name,
	// never the tag, so the value that reaches a system prompt is one a person wrote.
	for _, tbl := range tables {
		if got := TagFor(tbl.name); got != tbl.tag {
			t.Errorf("TagFor(%q) = %q, want %q; the name recorded by askLanguage must resolve back", tbl.name, got, tbl.tag)
		}
	}
}
