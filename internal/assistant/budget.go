// Token estimation for the context budget. Deliberately approximate and deliberately
// biased high: kenward cannot know which endpoint's tokenizer will see the prompt —
// the router chooses after assembly — so exact counting is not on offer and this file
// does not pretend to it.

package assistant

import "github.com/BlueHeisenberg/kenward/internal/routing"

// perMessageOverhead is the estimated cost of one message's framing (role markers,
// separators) in the endpoint's chat template.
const perMessageOverhead = 8

// estimateTokens returns a conservative token estimate for s: one token per three
// bytes of ASCII, one token per non-ASCII rune, rounded up.
//
// Error bars, measured against common BPE tokenizers rather than promised:
//
//   - English prose runs about four characters per token, so this overestimates it
//     by roughly a third — which is the intended direction, since overestimating
//     trims a little too much and underestimating truncates at the endpoint.
//   - Code and dense punctuation run closer to three characters per token, so the
//     estimate is near exact there.
//   - CJK text tokenizes at one to two tokens per rune; counting one per rune can
//     undercount it by up to half. Households writing mostly CJK should set
//     Options.ContextBudget with extra headroom.
//   - Emoji and rare scripts can cost several tokens per rune and are undercounted.
func estimateTokens(s string) int {
	ascii, other := 0, 0
	for _, r := range s {
		if r < 128 {
			ascii++
		} else {
			other++
		}
	}
	return (ascii+2)/3 + other
}

// estimateRequestTokens estimates the whole request: every message's content plus a
// fixed per-message framing overhead.
func estimateRequestTokens(msgs []routing.Message) int {
	total := 0
	for _, m := range msgs {
		total += perMessageOverhead + estimateTokens(m.Content)
	}
	return total
}
