package assistant

import (
	"github.com/BlueHeisenberg/kenward/internal/lang"
	"github.com/BlueHeisenberg/kenward/internal/transport"
)

// enCat is the English catalogue, which is what every golden file in this package
// asserts and what every test that names a notice compares against. A unit with no
// persona resolves to exactly this, so a test built on it is testing production's
// default rather than a fixture of its own.
var enCat = lang.For(lang.English)

// enNotice prepends the problem glyph the way Unit.problem does. It is the one piece
// of structure the catalogue deliberately does not carry.
func enNotice(s string) string { return transport.GlyphProblem + " " + s }

// englishUnit is a Unit with nothing set but the catalogue, for the tests that only
// want to render a string. It is not usable for a turn and is not meant to be.
func englishUnit() *Unit { return &Unit{cat: enCat} }
