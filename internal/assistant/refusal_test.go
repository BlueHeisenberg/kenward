package assistant

import (
	"testing"

	"github.com/BlueHeisenberg/kenward/internal/routing"
)

// Refusal strings are golden files: each of these fixtures is the exact text a
// member sees, and changing one is a deliberate edit, never a drive-by.

func TestRefusalGoldens(t *testing.T) {
	tests := []struct {
		name   string
		golden string
		text   string
	}{
		{
			name:   "direct single tier",
			golden: "refusal_direct.golden",
			text: englishUnit().refusalText(testDirectScope(), &routing.NoBackendError{
				Chain: []string{"local"},
				Tried: []string{"monster", "5090"},
			}),
		},
		{
			name:   "group multiple tiers",
			golden: "refusal_group.golden",
			text: englishUnit().refusalText(testGroupScope(), &routing.NoBackendError{
				Chain: []string{"local", "cloud"},
				Tried: []string{"monster", "5090", "openrouter"},
			}),
		},
		{
			name:   "nothing to try",
			golden: "refusal_none_tried.golden",
			text: englishUnit().refusalText(testDirectScope(), &routing.NoBackendError{
				Chain: []string{"local"},
				Tried: nil,
			}),
		},
		{
			name:   "empty chain",
			golden: "refusal_empty_chain.golden",
			text: englishUnit().refusalText(testDirectScope(), &routing.NoBackendError{
				Chain: nil,
				Tried: nil,
			}),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			golden(t, tc.golden, tc.text)
		})
	}
}
