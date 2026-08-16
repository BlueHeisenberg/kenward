package main

import (
	"strings"
	"testing"
)

// TestBuildAnswersCarriesSpaceIDs.
//
// A scripted install has to be able to say which lore space is whose, and it has to
// be able to say it with an id: spaces are resolved by id, and a display name here
// writes memory happily and returns nothing on the first read. The values are passed
// through unchecked on purpose — internal/setup validates them against lore's real
// listing, and a second, weaker check here would let --non-interactive write a
// configuration the interactive wizard would have refused.
func TestBuildAnswersCarriesSpaceIDs(t *testing.T) {
	t.Parallel()
	const (
		shared = "dac31e70-72e4-4b10-9cef-a6276c4a87b8"
		david  = "7d5047bb-d939-4539-b3db-8b6221a2e245"
	)
	a, err := buildAnswers("simple", "Casa", shared, "KENWARD_BOT_TOKEN",
		stringList{"David"}, stringList{"david=" + david},
		stringList{"name=m,url=http://m:8000/v1,model=q"}, nil, "", false)
	if err != nil {
		t.Fatalf("buildAnswers: %v", err)
	}
	if a.SharedSpace != shared {
		t.Errorf("SharedSpace = %q, want %q", a.SharedSpace, shared)
	}
	if got := a.MemberSpaces["david"]; got != david {
		t.Errorf("MemberSpaces[david] = %q, want %q", got, david)
	}
}

// TestBuildAnswersRejectsAMalformedMemberSpace: the error names the shape and where
// to find an id, because the operator reaching for --non-interactive is scripting an
// install and would otherwise put a display name there.
func TestBuildAnswersRejectsAMalformedMemberSpace(t *testing.T) {
	t.Parallel()
	for _, spec := range []string{"david", "=abc", "david="} {
		_, err := buildAnswers("simple", "Casa", "s", "KENWARD_BOT_TOKEN",
			stringList{"David"}, stringList{spec},
			stringList{"name=m,url=http://m:8000/v1,model=q"}, nil, "", false)
		if err == nil {
			t.Fatalf("--member-space %q was accepted", spec)
		}
		if !strings.Contains(err.Error(), "ID=SPACE_ID") || !strings.Contains(err.Error(), "lore spaces") {
			t.Errorf("--member-space %q: error does not say the shape and where ids come from: %v", spec, err)
		}
	}
}
