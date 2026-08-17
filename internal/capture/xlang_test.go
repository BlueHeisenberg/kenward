package capture

import (
	"context"
	"strings"
	"testing"

	"github.com/BlueHeisenberg/lore"

	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/lang"
	"github.com/BlueHeisenberg/kenward/internal/memory"
	"github.com/BlueHeisenberg/kenward/internal/transport"
)

// Retrieval across languages, against a real lore store.
//
// These tests run against lore itself, for the same reason internal/memory's do: the
// defect they are about is a property of lore's index — a conjunctive lexical match
// over title, body and domain, with no stemming and no translation — and a stub
// memory cannot have it. What answers here is lore.

// newRealStore returns a Client over a fresh lore home holding one space per name.
// Nothing here touches the operator's own store: the home is under t.TempDir() and is
// passed explicitly, so a LORE_HOME in the shell cannot redirect it.
func newRealStore(t *testing.T, names ...string) (*memory.Client, map[string]domain.SpaceID) {
	t.Helper()
	home := t.TempDir()
	if _, err := lore.Init(home, "kenward-test"); err != nil {
		t.Fatalf("lore.Init: %v", err)
	}
	c, err := memory.NewClient(memory.Config{LoreHome: home})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	// Before t.TempDir's own cleanup, which on Windows cannot remove a database
	// file that is still open.
	t.Cleanup(func() { _ = c.Close() })

	ids := map[string]domain.SpaceID{}
	for _, n := range names {
		sp, err := c.CreateSpace(t.Context(), n)
		if err != nil {
			t.Fatalf("CreateSpace(%q): %v", n, err)
		}
		ids[n] = domain.SpaceID(sp.ID)
	}
	return c, ids
}

// findable reports whether every one of these words retrieves the entry on its own,
// which is what retrieval needs: the assistant searches one content word at a time
// and unions the hits, so a word that finds nothing contributes nothing.
func findable(t *testing.T, c *memory.Client, space domain.SpaceID, words ...string) bool {
	t.Helper()
	for _, w := range words {
		hits, err := c.Search(t.Context(), memory.SearchQuery{
			Text:   w,
			Spaces: []domain.SpaceID{space},
		})
		if err != nil {
			t.Fatalf("Search(%q): %v", w, err)
		}
		if len(hits) == 0 {
			return false
		}
	}
	return true
}

// spanishScope is a Spanish-speaking member's direct conversation over a real store.
func spanishScope(ids map[string]domain.SpaceID) domain.Scope {
	return domain.Scope{
		Kind:   domain.ScopeDirect,
		Member: &domain.Member{ID: "ana", Name: "Ana", TelegramID: davidID, Private: ids["Ana"]},
		Write:  ids["Ana"],
		Read:   []domain.SpaceID{ids["Ana"], ids["Test House"]},
		ChatID: 100,
	}
}

// gardenGate is the entry from the live run, exactly as the model writes it: an
// English title and an English body, in a conversation held in Spanish. Only the
// aliases are the member's own language, and they are the whole of the fix.
func gardenGate() Proposal {
	return Proposal{
		Draft: memory.Draft{
			Domain: "household/logistics",
			Title:  "Garden gate code",
			Body:   "The code for the garden gate is 4821.",
		},
		Target:  TargetPersonal,
		Aliases: []string{"código de la puerta del jardín", "cancela"},
	}
}

// TestAnEntryIsFoundByTheWordsItsOwnerUses is the live defect.
//
// A household that chose Spanish said the garden gate code in Spanish, kenward stored
// it, and forty seconds later "¿Cuál es el código de la puerta del jardín?" retrieved
// nothing and the member was told it had never been said. The same question in
// English retrieved it, twice, from the same entry.
func TestAnEntryIsFoundByTheWordsItsOwnerUses(t *testing.T) {
	c, ids := newRealStore(t, "Ana", "Test House")
	sc := spanishScope(ids)

	tr := &stubTransport{answers: []transport.Answer{{TimedOut: true}}}
	e := New(c, tr, Options{Language: "Spanish"})

	out, err := e.Offer(context.Background(), sc, gardenGate(), davidID)
	if err != nil {
		t.Fatalf("Offer: %v", err)
	}
	if !out.Stored() {
		t.Fatalf("nothing was stored: %v", out.Kind)
	}

	// The words the member's own question reduces to.
	if !findable(t, c, sc.Write, "código", "puerta", "jardín") {
		t.Error("the member cannot find their own entry in the language they chose")
	}
	// And English still finds it. Whatever the fix costs, it must not cost this:
	// a household's entries written before it switched language are all English,
	// and an English word must go on retrieving them.
	if !findable(t, c, sc.Write, "garden", "gate", "code") {
		t.Error("the entry is no longer findable in English")
	}
}

// TestTheMemberIsShownWhatWasStored. The announcement is a report on a write that has
// already happened, so the body on the screen and the body in lore are one string. An
// alias line added on the way past would make "kenward tells you what it wrote" false
// in the one way nobody would notice.
func TestTheMemberIsShownWhatWasStored(t *testing.T) {
	c, ids := newRealStore(t, "Ana", "Test House")
	sc := spanishScope(ids)

	tr := &stubTransport{answers: []transport.Answer{{TimedOut: true}}}
	e := New(c, tr, Options{Language: "Spanish"})

	out, err := e.Offer(context.Background(), sc, gardenGate(), davidID)
	if err != nil {
		t.Fatalf("Offer: %v", err)
	}
	stored, err := c.Get(t.Context(), out.Space, out.EntryID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(tr.asks) != 1 {
		t.Fatalf("the member was asked %d times, want 1", len(tr.asks))
	}
	shown := tr.asks[0].q.Text
	for _, line := range strings.Split(stored.Body, "\n") {
		if line = strings.TrimSpace(line); line == "" {
			continue
		}
		if !strings.Contains(shown, transport.Esc(line)) {
			t.Errorf("the announcement does not show a stored line:\nstored: %q\nshown:  %s", line, shown)
		}
	}
	// And the label is the member's, not English's.
	if want := lang.For("Spanish").AlsoKnownAs([]string{"cancela"}); !strings.Contains(stored.Body, "También:") {
		t.Errorf("the alias line is not in the member's language: %q, want the form %q", stored.Body, want)
	}
}

// TestTheSharedSpaceOfAMultilingualHousehold is the case that decides the design, and
// the one it is bounded by.
//
// Ana speaks Spanish and Bernd speaks German, and they share one household memory.
// Ana publishes something to it. Ana finds it in Spanish, because her own words are
// on the entry. Bernd finds it in English, because English is the one language every
// entry is guaranteed to hold — which is the whole reason titles and bodies stay
// English. Bernd does not find it in German, and nothing here pretends otherwise: a
// lexical index cannot match a word that is not there, and the alternative — a
// translation on every retrieval — is a model round trip before every turn's search.
func TestTheSharedSpaceOfAMultilingualHousehold(t *testing.T) {
	c, ids := newRealStore(t, "Ana", "Test House")
	sc := spanishScope(ids)
	house := ids["Test House"]

	tr := &stubTransport{answers: []transport.Answer{accept(ChoiceShared)}}
	e := New(c, tr, Options{Language: "Spanish"})

	p := gardenGate()
	p.Target = TargetShared
	out, err := e.Offer(context.Background(), sc, p, davidID)
	if err != nil {
		t.Fatalf("Offer: %v", err)
	}
	if out.Space != house {
		t.Fatalf("stored in %s, want the household space %s", out.Space, house)
	}

	if !findable(t, c, house, "código", "puerta", "jardín") {
		t.Error("Ana cannot find, in Spanish, what she put in the household memory")
	}
	if !findable(t, c, house, "garden", "gate", "code") {
		t.Error("Bernd cannot find in English what Ana put in the household memory")
	}
	// The bound, asserted so that a later change which lifts it fails here and gets
	// read rather than shipped quietly.
	if findable(t, c, house, "gartentor") {
		t.Error("German now retrieves it; the comment above this test is stale")
	}
}

// TestAnEnglishConversationIsUnchanged. A model told to supply the member's own words
// supplies them in English too, where they are the words already in the entry.
// Storing those would put a line of duplication on the end of every entry in every
// English household and buy no retrieval at all.
func TestAnEnglishConversationIsUnchanged(t *testing.T) {
	c, ids := newRealStore(t, "Ana", "Test House")
	sc := spanishScope(ids)

	tr := &stubTransport{answers: []transport.Answer{{TimedOut: true}}}
	e := New(c, tr, Options{})

	p := gardenGate()
	p.Aliases = []string{"garden gate", "the code for the garden gate", "   "}
	out, err := e.Offer(context.Background(), sc, p, davidID)
	if err != nil {
		t.Fatalf("Offer: %v", err)
	}
	stored, err := c.Get(t.Context(), out.Space, out.EntryID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Body != p.Draft.Body {
		t.Errorf("the body gained a line that says nothing new:\n%q", stored.Body)
	}
}

// TestUsefulAliases covers the bounds on a field whose contents are model-written
// text arriving out of a member's conversation.
func TestUsefulAliases(t *testing.T) {
	const title, body = "Garden gate code", "The code for the garden gate is 4821."
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"nothing proposed", nil, nil},
		{"blank and whitespace dropped", []string{"", "   ", "\n"}, nil},
		{"already in the entry", []string{"garden gate", "CODE"}, nil},
		{"whitespace collapsed", []string{"  puerta   del jardín "}, []string{"puerta del jardín"}},
		{
			"a repeat contributing nothing new is dropped",
			[]string{"puerta del jardín", "jardín", "código"},
			[]string{"puerta del jardín", "código"},
		},
		{"a sentence is not a name for something", []string{strings.Repeat("ñ", maxAliasRunes+1)}, nil},
		{
			"bounded",
			[]string{"uno", "dos", "tres", "cuatro", "cinco", "seis", "siete"},
			[]string{"uno", "dos", "tres", "cuatro", "cinco", "seis"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := usefulAliases(title, body, tt.in)
			if strings.Join(got, "|") != strings.Join(tt.want, "|") {
				t.Errorf("usefulAliases(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestNoAliasLineInAnEnglishConversation is D2 from the second live run.
//
// Aliases exist so a member who is not speaking English can find an English entry.
// An English conversation has nothing to bridge, so there is no such thing as a
// useful alias in one — but the model is still asked for them, and it obliges: an
// English member's private note about a penicillin allergy was stored, and
// announced to them, carrying "Also known as: penny allergy".
//
// usefulAliases cannot catch that. It drops aliases whose tokens the entry already
// has, and "penny" is a word the entry does not contain — which is precisely what
// makes it junk rather than a synonym. The rule has to be structural and it has to
// be the language, which the engine already knows.
func TestNoAliasLineInAnEnglishConversation(t *testing.T) {
	junk := Proposal{
		Draft: memory.Draft{
			Domain: "household/health",
			Title:  "Penicillin allergy",
			Body:   "David is allergic to penicillin.",
		},
		Target:  TargetPersonal,
		Aliases: []string{"penny allergy", "the pen thing"},
	}
	for name, opts := range map[string]Options{
		"named English": {Language: "English"},
		"unset":         {},
	} {
		t.Run(name, func(t *testing.T) {
			c, ids := newRealStore(t, "Ana", "Test House")
			sc := spanishScope(ids)
			tr := &stubTransport{answers: []transport.Answer{{TimedOut: true}}}
			e := New(c, tr, opts)

			out, err := e.Offer(context.Background(), sc, junk, davidID)
			if err != nil {
				t.Fatalf("Offer: %v", err)
			}
			if !out.Stored() {
				t.Fatalf("nothing was stored: %v", out.Kind)
			}
			stored, err := c.Get(t.Context(), out.Space, out.EntryID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if stored.Body != junk.Draft.Body {
				t.Errorf("an English conversation stored an alias line on a member's entry:\n%q\nwant the body as written: %q", stored.Body, junk.Draft.Body)
			}
			if len(tr.asks) == 1 && strings.Contains(tr.asks[0].q.Text, "penny") {
				t.Errorf("the member was shown an invented alias:\n%s", tr.asks[0].q.Text)
			}
		})
	}
}

// TestANonEnglishConversationKeepsItsAliases is the other side of the same rule:
// the mechanism the alias line exists for must survive it.
func TestANonEnglishConversationKeepsItsAliases(t *testing.T) {
	c, ids := newRealStore(t, "Ana", "Test House")
	sc := spanishScope(ids)
	tr := &stubTransport{answers: []transport.Answer{{TimedOut: true}}}
	e := New(c, tr, Options{Language: "Spanish"})

	if _, err := e.Offer(context.Background(), sc, gardenGate(), davidID); err != nil {
		t.Fatalf("Offer: %v", err)
	}
	if !findable(t, c, sc.Write, "código", "puerta", "jardín") {
		t.Error("a Spanish conversation lost the aliases the English rule must not reach")
	}
}
