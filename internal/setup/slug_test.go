package setup

import "testing"

func TestSlugify(t *testing.T) {
	for in, want := range map[string]string{
		"David":            "david",
		"María":            "maria",
		"José Luis":        "jose-luis",
		"Ana-Sofía":        "ana-sofia",
		"  Björn  ":        "bjorn",
		"Müller":           "muller",
		"Ægir":             "gir", // Æ is not in the table; the letters that are survive
		"O'Brien":          "o-brien",
		"Anne Marie Smith": "anne-marie-smith",
		"local-slow":       "local-slow",
		"Our House!":       "our-house",
		"---":              "",
		"":                 "",
		"あかり":              "",
	} {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestUniqueSlug covers the household with a father and a son of the same name,
// which is common enough that a collision here would be a real first-run failure.
func TestUniqueSlug(t *testing.T) {
	taken := map[string]bool{}
	got := []string{
		uniqueSlug("David", taken, "member-1"),
		uniqueSlug("David", taken, "member-2"),
		uniqueSlug("david", taken, "member-3"),
	}
	want := []string{"david", "david-2", "david-3"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("uniqueSlug #%d = %q, want %q", i+1, got[i], want[i])
		}
	}
}

// TestUniqueSlugFallsBackForANameItCannotTransliterate: the person keeps their name,
// which is stored beside the id exactly as they typed it. Only the machine
// identifier falls back.
func TestUniqueSlugFallsBackForANameItCannotTransliterate(t *testing.T) {
	taken := map[string]bool{}
	if got := uniqueSlug("あかり", taken, "member-1"); got != "member-1" {
		t.Errorf("uniqueSlug = %q, want the fallback", got)
	}
	if got := uniqueSlug("ゆうき", taken, "member-1"); got != "member-1-2" {
		t.Errorf("uniqueSlug = %q, want a unique fallback", got)
	}
}

func TestPrivateSpaceFor(t *testing.T) {
	if got := privateSpaceFor("david"); got != "david-private" {
		t.Errorf("privateSpaceFor = %q", got)
	}
}

func TestEnvVarFor(t *testing.T) {
	for in, want := range map[string]string{
		"david":     "KENWARD_BOT_TOKEN_DAVID",
		"ana-sofia": "KENWARD_BOT_TOKEN_ANA_SOFIA",
	} {
		if got := envVarFor(MemberBotTokenPrefix, in); got != want {
			t.Errorf("envVarFor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAPIKeyEnvFor(t *testing.T) {
	for in, want := range map[string]string{
		"openrouter":  "OPENROUTER_API_KEY",
		"together ai": "TOGETHER_AI_API_KEY",
		"":            "KENWARD_API_KEY",
	} {
		if got := apiKeyEnvFor(in); got != want {
			t.Errorf("apiKeyEnvFor(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseTiers(t *testing.T) {
	for in, want := range map[string]string{
		"local":                "local",
		"local, cloud":         "local,cloud",
		"local,local":          "local",
		" Local , Local-Slow ": "local,local-slow",
		",,":                   "",
	} {
		got := ""
		for i, tier := range parseTiers(in) {
			if i > 0 {
				got += ","
			}
			got += tier
		}
		if got != want {
			t.Errorf("parseTiers(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLooksLikeBotToken(t *testing.T) {
	for in, want := range map[string]bool{
		"987654321:BBH-9zzqW_kK1mn5R2t8vXcYbNmQwErTyU3": true,
		"1:aaaaaaaaaaaaaaaaaaaaaaaaa":                   true,
		"@our_household_bot":                            false,
		"987654321":                                     false,
		"abc:defghijklmnopqrstuvwxyz":                   false,
		"987654321:short":                               false,
		"987654321:has spaces in it and is long enough": false,
		"": false,
	} {
		if got := looksLikeBotToken(in); got != want {
			t.Errorf("looksLikeBotToken(%q) = %v, want %v", in, got, want)
		}
	}
}
