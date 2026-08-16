package enrol

import (
	"context"

	"github.com/BlueHeisenberg/kenward/internal/config"
	"github.com/BlueHeisenberg/kenward/internal/domain"
)

// Tone values the tutorial offers as buttons. Empty is not one of them: it means the
// member never answered and inherits the household's, which is a different fact from
// choosing the flat register on purpose and has to stay distinguishable from it.
//
// They are free text as far as config.PersonaConfig.Tone is concerned — it is a phrase
// passed to the model, not a value looked up in a table — so this list is what the
// tutorial offers rather than what the schema permits. A household writing "formal
// usted" in the wizard is doing something these three buttons cannot.
const (
	// ToneFlat is kenward's default voice: short, direct, no small talk.
	ToneFlat = "flat"
	// ToneWarm is friendly and still brief.
	ToneWarm = "warm"
	// TonePlayful allows humour.
	TonePlayful = "playful"
)

// Limits on what a member may type at the tutorial.
//
// Tighter than config.MaxPersonaLine and config.MaxPersonaCharacter on purpose. Those
// are the ceiling the schema enforces and the point at which persona text starts
// costing retrieved memory; these are what a question asked in a chat window should
// accept, and a member who pastes an essay is told so by the tutorial rather than by
// kenward refusing to load its own configuration afterwards.
const (
	// MaxAgentNameLen is the longest name a member may give their agent.
	MaxAgentNameLen = 40
	// MaxCharacterLen is the longest character description a member may write.
	MaxCharacterLen = 300
)

// Persona is what one member chose about their own agent in the Telegram tutorial,
// with the small amount of tutorial progress that has to outlive the process.
//
// The four settings are config.PersonaConfig's, embedded rather than restated: they
// are the same four questions the wizard asks the household, they are written to the
// same schema, and two vocabularies for one thing is how the wizard and the tutorial
// would drift apart. Every field's zero value means "not answered, use the
// household's", which is what makes skipping a question and abandoning the tutorial
// the same thing downstream.
type Persona struct {
	config.PersonaConfig

	// ChatID is the private chat the tutorial ran in, kept so an interrupted one can
	// be finished later without guessing at a chat id from a user id.
	ChatID int64
	// QuestionMsg is the message id of a question still on screen with live-looking
	// buttons, and zero when there is none. See Tutorial.ask: it is written when the
	// question is posted and cleared when it ends, so a value here on the next start
	// is a keyboard the killed process never got to retire.
	QuestionMsg int
	// Explained records that the memory-model explanation reached this member.
	//
	// It is not a persona setting; it is the only piece of tutorial progress worth
	// persisting. Everything else is committed as it is answered, so an interrupted
	// tutorial degrades to defaults on its own — but the explanation is the part
	// kenward owes the member rather than the part it is asking them for, and a node
	// that restarted between the greeting and it would otherwise never send it.
	Explained bool
}

// PersonaStore records what a member chose, one answer at a time.
//
// The tutorial writes through it after every question rather than at the end, which
// is what makes abandonment cheap: the answers already given are already saved, and
// there is no in-progress record anywhere for a restart to leave stale.
//
// It is one interface and not a store, because what is behind it is internal/config:
// a member's persona is members[].persona and is recorded in the state file beside
// their binding, by the same object that wrote the binding. See config.Binder.
type PersonaStore interface {
	// SetPersona replaces the persona held for a member.
	SetPersona(ctx context.Context, id domain.MemberID, p Persona) error
	// Personas returns every persona held.
	Personas(ctx context.Context) (map[domain.MemberID]Persona, error)
}

// BinderPersonas is the PersonaStore a running node uses: members[].persona, recorded
// in the state file beside the binding the claim wrote, by the object that wrote it.
//
// One writer rather than two, and the same one. The tutorial starts the instant a
// claim succeeds and writes after every question, so a separate store would be a
// second process-wide writer of the same member's record, half a second behind the
// first. config.Binder already owns the state file, its mutex and its
// clone-write-swap; this carries the tutorial's two extra facts — which chat it ran
// in, and whether the explanation was sent — across the seam and nothing else.
func BinderPersonas(b *config.Binder) PersonaStore { return binderPersonas{b} }

type binderPersonas struct{ binder *config.Binder }

func (p binderPersonas) SetPersona(ctx context.Context, id domain.MemberID, per Persona) error {
	return p.binder.SetMemberPersona(ctx, id, config.MemberPersona{
		Persona:          per.PersonaConfig,
		TutorialChat:     per.ChatID,
		TutorialQuestion: per.QuestionMsg,
		Explained:        per.Explained,
	})
}

func (p binderPersonas) Personas(ctx context.Context) (map[domain.MemberID]Persona, error) {
	held, err := p.binder.MemberPersonas(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[domain.MemberID]Persona, len(held))
	for id, mp := range held {
		out[id] = Persona{
			PersonaConfig: mp.Persona,
			ChatID:        mp.TutorialChat,
			QuestionMsg:   mp.TutorialQuestion,
			Explained:     mp.Explained,
		}
	}
	return out, nil
}
