// Prompt assembly. docs/PROMPT.md is the source of truth for every string in this
// file: the text constants are copied from it verbatim, placeholders included, and
// changing one is a deliberate edit there first. The rendered output is golden-tested.
//
// Assembly order is fixed — identity, scope disclosure, retrieved memory, capture
// instructions, recent turns, the member's message — and nothing is reordered by
// relevance, because a member reading a transcript should be able to predict what
// the assistant saw.

package assistant

import (
	"fmt"
	"strings"

	"github.com/BlueHeisenberg/kenward/internal/domain"
	"github.com/BlueHeisenberg/kenward/internal/memory"
	"github.com/BlueHeisenberg/kenward/internal/routing"
)

// identityText is docs/PROMPT.md "Identity and character", verbatim.
const identityText = `You are kenward, a household assistant. You are talking to {{.MemberName}}.

You are useful, brief, and specific. You answer the question that was asked. When you
do not know something, you say so plainly rather than producing something
plausible-sounding. You do not open replies with restatements of the question, and you
do not close them with offers to help further.

You are a member of this household's infrastructure, not a personality. Warmth is fine;
performance is not.

Today is {{.Date}}. The household is {{.HouseholdName}}.`

// directDisclosureText is the direct-conversation scope disclosure, verbatim. It is
// rendered from the resolved scope, never from configuration: a member must be able
// to ask "can you see that?" and get a true answer.
const directDisclosureText = `This is a private conversation with {{.MemberName}}.

You can read two memories here:
  - {{.MemberName}}'s private memory, which only they and you can read.
  - the household's shared memory, which everyone in {{.HouseholdName}} can read.

Anything you remember from this conversation goes to {{.MemberName}}'s private memory
unless they choose otherwise. Nothing you learn here is visible to anyone else in the
household unless {{.MemberName}} explicitly publishes it.`

// groupDisclosureText is the group-conversation scope disclosure, verbatim. Its last
// sentence exists because the failure mode is not the model leaking private memory —
// it structurally cannot, the retrieval never happened — but the model guessing and
// being believed.
const groupDisclosureText = `This is the {{.HouseholdName}} group conversation. Everyone in the household can read
it.

You can read the household's shared memory here, and nothing else. You cannot see any
member's private memory in this conversation, and you must not speculate about what
might be in one. If someone asks you about something only their private memory would
know, tell them to ask you directly instead.`

// confidenceText explains how to weigh lore's vocabulary, verbatim. Confidence and
// markers are passed through unchanged; kenward does not reinterpret them.
const confidenceText = `Memory entries carry a confidence: experimental, provisional, validated or hardened.
Treat provisional entries as things that were true once and may have changed. Markers
in brackets are notes from whoever recorded the entry; honour them.`

// captureText is the capture instruction block, verbatim.
const captureText = `If this conversation contains something worth remembering — a durable fact, a
preference, a decision, something the household will want recalled later — you may
propose storing it by calling the remember tool.

Propose at most one thing per reply, and only when it is genuinely durable. Do not
propose remembering: the content of this conversation as a summary, anything already in
the memory shown above, anything that will be false next week, or anything the member
has already declined.

You propose. {{.MemberName}} decides, with a button. If you are unsure whether something
belongs in private or shared memory, say unsure rather than guessing — they will be
asked.`

// captureGroupText is what a group scope adds, verbatim.
const captureGroupText = `This is a group conversation, so anything remembered here goes to the household's shared
memory. You cannot propose storing anything in a private memory from here.`

// emptyGroupText is rendered for a group whose search returned nothing. An absent
// section reads to the model as "there is no such memory"; an explicit empty one
// reads as "I looked and found nothing". The second is true and the first is not.
const emptyGroupText = "(nothing relevant found)"

// unreadableGroupText is rendered for a group whose search failed. Rendering an error
// as "nothing relevant found" would be a lie the member might act on; the honest
// statement is that the space could not be read this turn.
const unreadableGroupText = "(this memory could not be read just now)"

// groupMemberPhrase stands in for the member's name in a group scope, where the
// deciding member is whoever spoke. It is capitalised to fit the one place the
// capture text uses the name at the start of a sentence.
const groupMemberPhrase = "The member who asked"

// dateFormat is how {{.Date}} renders. The weekday is included because household
// questions are usually about the week, not the calendar.
const dateFormat = "Monday, 2 January 2006"

// promptInput is everything the renderer needs for one turn's system prompt. The
// budget loop trims its elastic parts — entries, then a note that it did.
type promptInput struct {
	scope         domain.Scope
	memberName    string
	householdName string
	date          string

	// private is the member's group; absent entirely in a group scope, where the
	// retrieval never happened.
	private        []memory.Entry
	hasPrivate     bool
	privateErr     bool
	privateDropped int

	// shared is the household's group.
	shared        []memory.Entry
	hasShared     bool
	sharedErr     bool
	sharedDropped int
}

// assemble renders the turn's request within the context budget.
//
// The elastic parts give way in this order: recent turns are trimmed first, oldest
// first — a forgotten fact is worse than a forgotten pleasantry — then retrieved
// entries are dropped from the end of the shared group, then from the end of the
// private group, never from the middle, and the prompt states that entries were
// dropped rather than hiding it. If everything elastic is gone and the estimate is
// still over budget, the request goes as it is: the identity, disclosure and the
// member's message are not negotiable.
func (u *Unit) assemble(sc domain.Scope, groups []spaceGroup, text string) routing.Request {
	inp := u.promptInput(sc, groups)
	hist := u.history.snapshot()
	budget := u.opts.ContextBudget - u.opts.MaxTokens
	if budget < 1 {
		budget = u.opts.ContextBudget
	}

	for {
		msgs := buildMessages(renderSystem(inp), hist, text)
		if estimateRequestTokens(msgs) <= budget {
			return u.request(msgs)
		}
		switch {
		case len(hist) > 0:
			hist = hist[1:]
		case len(inp.shared) > 0:
			inp.shared = inp.shared[:len(inp.shared)-1]
			inp.sharedDropped++
		case len(inp.private) > 0:
			inp.private = inp.private[:len(inp.private)-1]
			inp.privateDropped++
		default:
			return u.request(msgs)
		}
	}
}

func (u *Unit) request(msgs []routing.Message) routing.Request {
	req := routing.Request{
		Messages:  msgs,
		MaxTokens: u.opts.MaxTokens,
		// The remember tool rides on every turn; the schema is the one published
		// in docs/PROMPT.md, so the model is offered exactly what the member was
		// told exists.
		Tools: rememberTools(),
	}
	if u.opts.Temperature != nil {
		t := *u.opts.Temperature
		req.Temperature = &t
	}
	return req
}

// promptInput maps retrieval groups onto the prompt's two memory sections using the
// scope, and only the scope: in a direct scope Read is private first then shared, in
// a group scope Read is the shared space alone.
func (u *Unit) promptInput(sc domain.Scope, groups []spaceGroup) promptInput {
	inp := promptInput{
		scope:         sc,
		householdName: u.opts.HouseholdName,
		date:          u.opts.Now().Format(dateFormat),
	}
	if sc.Kind == domain.ScopeDirect && sc.Member != nil {
		inp.memberName = sc.Member.Name
		if len(groups) > 0 {
			inp.hasPrivate = true
			inp.private = groups[0].entries
			inp.privateErr = groups[0].err != nil
		}
		if len(groups) > 1 {
			inp.hasShared = true
			inp.shared = groups[1].entries
			inp.sharedErr = groups[1].err != nil
		}
		return inp
	}
	inp.memberName = groupMemberPhrase
	if len(groups) > 0 {
		inp.hasShared = true
		inp.shared = groups[0].entries
		inp.sharedErr = groups[0].err != nil
	}
	return inp
}

// renderSystem produces the system prompt in the fixed assembly order: identity,
// scope disclosure, retrieved memory, capture instructions.
func renderSystem(inp promptInput) string {
	group := inp.scope.Kind == domain.ScopeGroup

	identityName := inp.memberName
	if group {
		identityName = "the " + inp.householdName + " household"
	}
	fill := strings.NewReplacer(
		"{{.MemberName}}", inp.memberName,
		"{{.Date}}", inp.date,
		"{{.HouseholdName}}", inp.householdName,
	)

	var sections []string
	sections = append(sections, strings.NewReplacer(
		"{{.MemberName}}", identityName,
		"{{.Date}}", inp.date,
		"{{.HouseholdName}}", inp.householdName,
	).Replace(identityText))

	if group {
		sections = append(sections, fill.Replace(groupDisclosureText))
	} else {
		sections = append(sections, fill.Replace(directDisclosureText))
	}

	if inp.hasPrivate {
		sections = append(sections, renderMemorySection(
			fmt.Sprintf("## From %s's private memory", inp.memberName),
			inp.private, inp.privateErr, inp.privateDropped))
	}
	if inp.hasShared {
		sections = append(sections, renderMemorySection(
			"## From the household's shared memory",
			inp.shared, inp.sharedErr, inp.sharedDropped))
	}
	sections = append(sections, confidenceText)

	captureSection := fill.Replace(captureText)
	if group {
		captureSection += "\n\n" + captureGroupText
	}
	sections = append(sections, captureSection)

	return strings.Join(sections, "\n\n")
}

// renderMemorySection renders one space's group: header, entries with confidence and
// markers passed through verbatim, an explicit statement when there is nothing to
// show, and an explicit statement when entries were dropped for budget.
func renderMemorySection(header string, entries []memory.Entry, unreadable bool, dropped int) string {
	var b strings.Builder
	b.WriteString(header)
	switch {
	case unreadable:
		b.WriteString("\n")
		b.WriteString(unreadableGroupText)
	case len(entries) == 0 && dropped == 0:
		b.WriteString("\n")
		b.WriteString(emptyGroupText)
	default:
		for _, e := range entries {
			b.WriteString("\n")
			b.WriteString(renderEntry(e))
		}
	}
	if dropped > 0 {
		b.WriteString("\n")
		if dropped == 1 {
			b.WriteString("(1 more entry was retrieved but dropped to fit the context budget)")
		} else {
			fmt.Fprintf(&b, "(%d more entries were retrieved but dropped to fit the context budget)", dropped)
		}
	}
	return b.String()
}

// renderEntry renders one entry in docs/PROMPT.md's shape:
//
//   - Title [confidence] (marker, marker)
//     Body
//
// Every body line is indented under the bullet so a multi-line body cannot be read
// as a new entry.
func renderEntry(e memory.Entry) string {
	var b strings.Builder
	b.WriteString("- ")
	b.WriteString(e.Title)
	b.WriteString(" [")
	b.WriteString(e.Confidence)
	b.WriteString("]")
	if len(e.Markers) > 0 {
		b.WriteString(" (")
		b.WriteString(strings.Join(e.Markers, ", "))
		b.WriteString(")")
	}
	for _, line := range strings.Split(e.Body, "\n") {
		b.WriteString("\n  ")
		b.WriteString(line)
	}
	return b.String()
}

// buildMessages lays the prompt out on the routing seam: the assembled system prompt,
// then the history ring oldest first as alternating user and assistant messages, then
// the member's message.
func buildMessages(system string, hist []turnRecord, text string) []routing.Message {
	msgs := make([]routing.Message, 0, 2+2*len(hist))
	msgs = append(msgs, routing.Message{Role: "system", Content: system})
	for _, t := range hist {
		if t.user != "" {
			msgs = append(msgs, routing.Message{Role: "user", Content: t.user})
		}
		if t.assistant != "" {
			msgs = append(msgs, routing.Message{Role: "assistant", Content: t.assistant})
		}
	}
	msgs = append(msgs, routing.Message{Role: "user", Content: text})
	return msgs
}
