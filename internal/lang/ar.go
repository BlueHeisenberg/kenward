package lang

import (
	"fmt"
	"strings"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/remind"
	"github.com/BlueHeisenberg/kenward/internal/transport"
)

// Modern Standard Arabic. Plain, calm, second person singular, no dialect.
//
// Punctuation is ؟ U+061F, ، U+060C and ؛ U+061B. Digits are Western Arabic
// everywhere, which is not a preference: see the isolate discussion below.
//
// Terminology: household = المنزل, a deployment and a place rather than a blood
// relation — الأسرة is a family and is wrong for a shared flat. entry = عنصر, which
// counts cleanly, where مُدخَل reads as an *input* to any technical reader.
//
// # The two bidi problems that break the product rather than the layout
//
// Neither is a layout complaint and neither shows up in a test that compares Go
// strings, because in both the bytes are right and the rendering is wrong.
//
// 1. Digit substitution, UBA rule W2. A European Number takes the type of the last
// strong character before it, and if that character is Arabic-script the digits
// become Arabic Numbers, which a renderer is then free to draw with national digit
// forms. Every digit in this table follows Arabic prose, so "الرمز 4821" can be
// drawn as "الرمز ٤٨٢١" — the reminder fires, the member reads Arabic-Indic digits,
// and cannot type the code back to cancel it. An LRI immediately before a numeric
// run resets the last-strong type and the Western forms are what gets drawn. That
// makes the rule broader than "keep {id} and {time} Latin": every numeric run needs
// one, retrieval counts included.
//
// 2. Base direction is inherited. A notice is appended to the model's own answer
// inside one Telegram message, and paragraph direction is decided by the first
// strong character of the whole message. A Latin-initial answer therefore sets the
// base to LTR, and the Arabic fragments of the notice lay out left to right relative
// to each other — the sentence reads backwards and the reader finishes at the
// right-hand edge. Notice pins each appended line to RTL whatever precedes it.
//
// FSI is U+2068 and LRI is U+2066. They are not interchangeable: LRI on {title}
// would force an Arabic-titled entry to render left to right, which for this
// audience is the more common case. Direction is a property of the string, not of
// the member's locale — the language chooses this table and says nothing about what
// the member typed into it.
//
// Brackets and parentheses are left exactly as the other tables write them. In an
// RTL context "(" is drawn mirrored and sits at the visual right, which is correct
// Arabic typography: the pair still encloses. Swapping the characters in source
// produces a genuinely broken pair the moment the base direction is LTR.
//
// # Gender
//
// Arabic second person is gendered and has no neutral form, and there is no member
// gender to look up. Masculine singular is used throughout as the unmarked form,
// which is the standard convention and which roughly half of any household will read
// as not addressed to them. Rewriting every imperative as a verbal noun is genuinely
// neutral and reads like a government form, so it is flagged rather than done.
const (
	arLRI = "\u2066" // left-to-right isolate: content known to be Latin or digits
	arRLI = "\u2067" // right-to-left isolate: pins a whole notice to an RTL base
	arFSI = "\u2068" // first-strong isolate: content of unknown direction
	arPDI = "\u2069" // closes any of the three
	arRLM = "\u200f" // strong RTL mark; sets base direction without isolating
)

// arNum isolates a numeric run so rule W2 cannot reclassify its digits.
func arNum(s string) string { return arLRI + s + arPDI }

// arAny isolates a value whose direction is not knowable — a title, a reminder's
// text, a machine name. The value is escaped by whatever produced it.
func arAny(s string) string { return arFSI + s + arPDI }

// arJoin is Arabic list grammar. و is a proclitic: a space before it and none after,
// so two items are "A وB" and three are "A، B وC". The n == 2 case needs no branch,
// because joining a one-element slice returns that element. Strict classical usage
// repeats و before every item; the comma form is standard modern usage in every
// Arabic newspaper and interface and is what a member expects.
//
// Each item already carries its own isolate, which lands immediately after the و and
// is zero width, so the no-space rule is honoured and the renderer still gets a run
// boundary.
func arJoin(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	default:
		return strings.Join(items[:len(items)-1], "، ") + " و" + items[len(items)-1]
	}
}

func arIsolateAll(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = arAny(transport.Code(n))
	}
	return out
}

// Definite for a weekday shown on its own; كل takes the indefinite, because
// كل يوم الاثنين is common speech and grammatically loose, and this string is a
// promise about when something fires. Both are Sunday-indexed, matching
// time.Weekday.
var arWeekdaysIndefinite = [7]string{"أحد", "اثنين", "ثلاثاء", "أربعاء", "خميس", "جمعة", "سبت"}

// The يناير set, chosen on comprehension asymmetry rather than population. A
// Levantine or Iraqi reader parses يناير without effort — it is what pan-Arab media,
// airline tickets and software use — and the reverse does not hold: a Gulf or
// Egyptian reader does not reliably map كانون الثاني onto January, and جانفي is
// opaque outside the Maghreb. The cost is that it reads slightly foreign in
// Damascus, Beirut, Amman and Baghdad.
var arMonths = [13]string{"", "يناير", "فبراير", "مارس", "أبريل", "مايو", "يونيو",
	"يوليو", "أغسطس", "سبتمبر", "أكتوبر", "نوفمبر", "ديسمبر"}

var arabic = Catalogue{
	Tag:         Arabic,
	Name:        "العربية",
	EnglishName: "Arabic",

	Locked:        "المساعد مقفل. يجب فتح قفله على الجهاز الذي يعمل عليه.",
	ContentFilter: "رفض النموذج الإجابة عن رسالتك.",
	Queued:        "ما زلت أعمل على رسالتك السابقة — هذه في الانتظار وسآخذها بعدها.",
	Dropped:       "تراكمت عليّ الرسائل واضطررت إلى إسقاط تلك الرسالة. أرسلها مرة أخرى بعد قليل.",
	NoAnswer:      "لم أحصل على إجابة صالحة عن ذلك. حاول السؤال مرة أخرى.",
	ToolMisfire:   "حاولت أن أفعل شيئًا من أجل ذلك فأخطأت، فلم يحدث شيء. اسألني مرة أخرى.",
	NothingSaved:  "لم أحفظ أي شيء الآن. قل لي مرة أخرى إن أردت أن أتذكره.",
	ResetNotice:   "نبدأ من جديد — مسحت الجزء السابق من هذه المحادثة. لم يتغير شيء في ذاكرتك؛ هذه هي إعادة الضبط المُجدولة.",

	// Both spellings of حسنا: the diacritic sits before the alif in one and after
	// it in the other, and the two reduce to different letter sequences.
	BareAcknowledgements: []string{
		"تم", "تمام", "حسنًا", "حسناً", "مفهوم", "فهمت", "تم الحفظ", "سجلت",
		"لا مشكلة", "بالتأكيد", "أكيد", "حاضر",
	},

	// Written without diacritics on purpose. Normalization drops a shadda or a
	// fatha, which splits the word around it — "دوّنت" would become two tokens and
	// stop matching the undiacriticized "دونت" a model actually types.
	SaveClaims: []string{
		"تم الحفظ", "حفظت", "سجلت", "دونت", "تم التسجيل", "تم تسجيل",
		"احتفظت", "محفوظ", "مسجل",
		"استلمت", "تم الاستلام",
		"أضفت إلى", "في ذاكرتك",
	},

	// Undiacriticized for the same reason as SaveClaims above.
	SavePromises: []string{
		"سأتذكر", "سوف أتذكر", "لن أنسى", "سأحفظ", "سوف أحفظ", "سأسجل",
		"لن انسى",
	},

	// Undiacriticized for the same reason as SaveClaims above. The imperative of
	// دوّن is deliberately absent: without its shadda it is دون, which is the
	// ordinary word for "without" and would fire on half the messages a household
	// sends.
	SaveRequests: []string{
		"احفظ", "احتفظ", "تذكر", "ذكرني", "سجل", "اكتب", "لا تنس", "لا تنسى",
		"خذ ملاحظة", "للمرة القادمة", "لا تنساه",
	},

	ModelBusy:         "النموذج مشغول الآن. حاول مرة أخرى بعد قليل.",
	Misconfigured:     "هناك خلل في إعداد هذا المنزل — أخبر من يديره.",
	TurnFailed:        "حدث خطأ أثناء الاتصال بالنموذج، ولم تُجَب رسالتك. حاول مرة أخرى بعد قليل.",
	ReasoningOnly:     "قضى النموذج الوقت كله في التفكير ولم يصل إلى إجابة. لا شيء معطّل — حاول السؤال مرة أخرى، أو قسّمه إلى أجزاء أصغر.",
	RefusalEmptyChain: "لا يوجد جهاز مُعدّ للإجابة في هذه المحادثة. اطلب ممن يدير هذه العقدة أن يضبط واحدًا.",

	RefusalAssembled: func(whose, chain, tried, tierWord string) string {
		return fmt.Sprintf("لا يمكن الوصول الآن إلى أي جهاز في %s (%s). %s هذه المحادثة مقصورة على %s، فلن أرسلها إلى أي مكان آخر. شغّل أحدها ثم اسأل مرة أخرى.",
			whose, chain, tried, tierWord)
	},
	// After في, so genitive. Both are sound feminine plurals whose genitive is
	// orthographically identical to the nominative in unvocalised text, so this is
	// the one destination-shaped slot in the whole table that is genuinely safe to
	// share. It is not split, and splitting it would be noise.
	WhoseDirect: "مستوياتك المسموح بها",
	WhoseGroup:  "مستويات المنزل المسموح بها",
	// مقصورة على governs the genitive, and the dual is the one place Arabic case is
	// orthographically visible: المستويَين, not المستويان. Three forms rather than
	// six — مستوى is invariant and مستويات is a sound feminine plural — but the dual
	// has to be its own entry, and it is an id the English does not have.
	TierWord: func(n int) string {
		switch n {
		case 1:
			return "ذلك المستوى"
		case 2:
			return "هذين المستويَين"
		default:
			return "تلك المستويات"
		}
	},
	Chain: func(names []string) string { return strings.Join(arIsolateAll(names), "، ") },
	// Non-rational plurals take feminine-singular agreement, which is why three,
	// thirty and three hundred share one form; the dual takes dual agreement
	// regardless, so it needs its own.
	Tried: func(names []string) string {
		items := arIsolateAll(names)
		switch len(items) {
		case 0:
			return "لم يكن لأي منها عنوان يمكن الوصول إليه."
		case 1:
			return items[0] + " غير متاح."
		case 2:
			return arJoin(items) + " غير متاحَين."
		default:
			return arJoin(items) + " غير متاحة."
		}
	},

	Searched:    func(parts []string) string { return "بحثت في " + arJoin(parts) },
	PartPrivate: func(count string) string { return "ذاكرتك الخاصة " + count },
	PartShared:  func(count string) string { return "ذاكرة المنزل " + count },
	// Six CLDR categories and five distinct strings. zero and other share a shape
	// and differ only because zero is replaced by a word; one and two carry the
	// count in the morphology and do not interpolate n at all, so a completeness
	// test asserting every count string contains a numeral is wrong rather than
	// these are. The n%100 rule is required: 103 is few, 111 is many, and an
	// n <= 10 shortcut is wrong above a hundred. The tanwīn on عنصرًا is grammar,
	// not optional vocalisation.
	Count: func(unreadable bool, n int) string {
		switch m := n % 100; {
		case unreadable:
			return "(تعذّرت قراءتها)"
		case n == 0:
			return "(لا شيء)"
		case n == 1:
			return "(عنصر واحد)"
		case n == 2:
			return "(عنصران)"
		case m >= 3 && m <= 10:
			return "(" + arNum(fmt.Sprint(n)) + " عناصر)"
		case m >= 11 && m <= 99:
			return "(" + arNum(fmt.Sprint(n)) + " عنصرًا)"
		default:
			return "(" + arNum(fmt.Sprint(n)) + " عنصر)"
		}
	},

	RemindFull:    "[لديك بالفعل أقصى عدد من التذكيرات يمكنني الاحتفاظ به — ألغِ واحدًا أولًا]",
	RemindPast:    "[ذلك الوقت قد مضى، فلم أضبط شيئًا]",
	RemindFailed:  "[لم أتمكن من ضبط ذلك التذكير]",
	UnremindNone:  "[لا يوجد تذكير بهذا الرمز]",
	UnremindFails: "[لم أتمكن من إلغاء ذلك التذكير]",
	// Four direction runs in one line: the brackets are neutral, the prose is RTL,
	// {when} carries its own isolate around the clock reading, {text} is of unknown
	// direction and {id} is Latin. Without the FSI on {text} a Latin reminder pulls
	// its own trailing punctuation to the wrong end — "Buy milk!" renders as
	// "!Buy milk". The RLI that pins the whole line comes from Notice, so it is not
	// applied twice here.
	ReminderSet: func(when, text, id string) string {
		return "[ضُبط التذكير، " + when + ": " + arAny(transport.Esc(text)) +
			" — الرمز " + arNum(id) + "]"
	},
	ReminderCancelled: func(text string) string {
		return "[أُلغي التذكير: " + arAny(transport.Esc(text)) + "]"
	},
	// The day numeral and the clock reading are isolated separately, because the
	// month name between them is Arabic and one isolate around the whole date would
	// force the month left to right.
	When: func(r remind.Reminder, loc *time.Location) string {
		at := " الساعة " + arNum(clock(r))
		switch r.Every {
		case remind.EveryDaily:
			return "كل يوم" + at
		case remind.EveryWeekly:
			return "كل يوم " + arWeekdaysIndefinite[r.Weekday] + at
		default:
			d := r.Next.In(loc)
			return arNum(fmt.Sprint(d.Day())) + " " + arMonths[d.Month()] + at
		}
	},

	SaveFailed: "لم أتمكن من حفظ ذلك العنصر — لم يُكتب شيء.",
	AskFailed: func(title string) string {
		return "كنت سأسألك عن حفظ " + arAny(transport.Bold(title)) + "، لكن السؤال لم يصل. لم يُكتب شيء."
	},
	// The visible driver of the split is the preposition the verb governs, not the
	// case ending: ذاكرة and ذاكرتك الخاصة end in tāʾ marbūṭa and an adjective, and
	// their genitive and accusative are identical in unvocalised text. If case were
	// the only issue a shared slot would have been safe. It is not, because the
	// preposition sits inside the English phrase rather than in the template.
	Saved: func(private bool, title string) string {
		if private {
			return "حفظت " + arAny(transport.Bold(title)) + " في ذاكرتك الخاصة."
		}
		return "حفظت " + arAny(transport.Bold(title)) + " في ذاكرة المنزل."
	},
	SavedNoUndo: func(private bool, title string) string {
		where := "في ذاكرة المنزل"
		if private {
			where = "في ذاكرتك الخاصة"
		}
		return "حفظت " + arAny(transport.Bold(title)) + " " + where + "، لكن زر التراجع لم يصل، فلا أستطيع استرجاعه من هنا."
	},
	Removed: func(private bool, title string) string {
		where := "من ذاكرة المنزل"
		if private {
			where = "من ذاكرتك الخاصة"
		}
		return "أزلت " + arAny(transport.Bold(title)) + " " + where + ". لن يظهر مرة أخرى في أي إجابة، لا هنا ولا على أي جهاز آخر في المنزل."
	},
	// العنصر is a classifier noun and it is load-bearing. Where {title} is the
	// subject of what follows, Arabic must agree with it in gender, and the title is
	// arbitrary runtime text whose gender is unknowable. العنصر is masculine and
	// fixed, so ما زال and لم يُخزَّن and لم يُنشر agree with it and never with the
	// title. Where {title} is an object there is no agreement and no classifier.
	UndoFailed: func(private bool, title string) string {
		where := "في ذاكرة المنزل"
		if private {
			where = "في ذاكرتك الخاصة"
		}
		return "لم أتمكن من التراجع: العنصر " + arAny(transport.Bold(title)) + " ما زال " + where + "."
	},
	StoreRefused: func(private bool, title string) string {
		where := "في ذاكرة المنزل"
		if private {
			where = "في ذاكرتك الخاصة"
		}
		return "لم أتمكن من حفظ " + arAny(transport.Bold(title)) + " " + where + " — رفض مخزن الذاكرة الكتابة، فلم يُخزَّن شيء."
	},
	WrongSpace: func(title string) string {
		return "حدث خطأ: العنصر " + arAny(transport.Bold(title)) + " لم يُخزَّن في المكان الصحيح. أخبر من يدير هذه العقدة قبل حفظه مرة أخرى."
	},
	PublishNoShared:   "لم أتمكن من نشر ذلك العنصر — لم يُنشر شيء.",
	PublishUnreadable: "لم أتمكن من قراءة ذلك العنصر، فلم يُنشر شيء.",
	PublishAskFailed: func(title string) string {
		return "كنت سأسألك عن نشر " + arAny(transport.Bold(title)) + "، لكن السؤال لم يصل. لم يُنشر شيء."
	},
	PublishRefused: func(title string) string {
		return "لم أتمكن من نشر " + arAny(transport.Bold(title)) + " — رفض مخزن الذاكرة النسخ، فلم يصل شيء إلى ذاكرة المنزل."
	},
	PublishWrongSpace: func(title string) string {
		return "حدث خطأ: العنصر " + arAny(transport.Bold(title)) + " لم يُنشر في المكان الذي اخترته. أخبر من يدير هذه العقدة."
	},
	Published: func(title string) string {
		return "نشرت " + arAny(transport.Bold(title)) + " في ذاكرة المنزل. يمكن للجميع في المنزل رؤيته الآن."
	},

	OnlyOneProposal: "أسأل عن شيء واحد في كل مرة؛ ولم يُحفظ شيء آخر من تلك الرسالة.",

	ProposalOpener: "يمكنني أن أدوّن هذا:",
	ProposalNoDest: "أين أحفظه؟",
	ProposalWithDest: func(private bool) string {
		if private {
			return "هل أحفظه في ذاكرتك الخاصة؟"
		}
		return "هل أحفظه في ذاكرة المنزل؟"
	},
	UndoExpiredNote: "انتهت مهلة التراجع؛ هذا ما زال في ذاكرتك الخاصة",
	WrittenOpener: func(private bool) string {
		if private {
			return "كتبت هذا في ذاكرتك الخاصة:"
		}
		return "كتبت هذا في ذاكرة المنزل:"
	},
	// "Undo" is a button name here, not a verb: التراجع on its own reads as the
	// abstract act of undoing, so the button is named.
	WrittenHint: "زر التراجع يزيله.",
	// Two sentences rather than a dash. The struck entry below this line is Latin
	// script, and a dash at the end of an Arabic clause detaches from it across that
	// boundary the way the outcome dash does; a full stop does not.
	NotSaved: func(private bool) string {
		if private {
			return "لم يُحفظ في ذاكرتك الخاصة."
		}
		return "لم يُحفظ في ذاكرة المنزل."
	},
	PromotionOpener: "سيُنشر هذا للمنزل كما هو تمامًا، ولا يمكن التراجع عن نشره:",
	PromotionCloser: "هل أنشره؟",
	AlsoKnownAs:     func(words []string) string { return "أيضًا: " + strings.Join(words, "، ") },
	EnglishGloss: func(summary string) string {
		return "النص أعلاه محفوظ بالإنجليزية، ومعناه: " + arFSI + summary + arPDI
	},

	BtnUndo:             "تراجع",
	BtnPublishHousehold: "نشر للمنزل",
	BtnCancel:           "إلغاء",
	BtnSavePersonal:     "حفظ في ذاكرتي",
	BtnDontSave:         "لا تحفظ",
	BtnPersonal:         "ذاكرتي الخاصة",
	BtnHousehold:        "ذاكرة المنزل",
	BtnSaveHousehold:    "حفظ في ذاكرة المنزل",

	// The leading em dash is an English convention with no Arabic equivalent, and
	// the text above it is the member's own question, of unknown direction. If the
	// question was Latin the dash resolves to LTR and stays on the left while the
	// Arabic runs away from it, so the dash and the phrase it introduces end up at
	// opposite ends of the line. RLM is the right tool: there is nothing to
	// isolate, only a base direction to assert for a fragment that begins with a
	// neutral.
	Dash:      arRLM + "— ",
	Declined:  "لا إجابة، اعتُبر رفضًا",
	Withdrawn: "سُحب السؤال",

	EnrolPrivateHeading: "هذه المحادثة خاصة",
	EnrolPrivateBody: "هذه المحادثة — أنا وأنت فقط — هي ذاكرتك الخاصة. ما تقوله لي هنا يُحفظ في مساحتك أنت، " +
		"والمحادثة الجماعية للمنزل لا يمكنها قراءته أبدًا. ولن أذكره هناك أنا أيضًا.",
	EnrolPrivateSealed: "هذا المنزل يعمل في الوضع المعزول: لمساعدك عمليةٌ خاصة به ومفتاحٌ خاص به. " +
		"لا يستطيع أحد آخر في المنزل قراءة ذاكرتك الخاصة، ولا الشخص الذي يشغّل هذا الجهاز. " +
		"والحد الصادق: من يملك صلاحية الجذر على هذا الجهاز يمكنه الوصول إلى مفتاحك ما دام مساعدك يعمل.",
	EnrolSharedOnlyHeading: "نتشارك ذاكرة واحدة",
	EnrolSharedOnlyBody: "أنت من أفراد هذا المنزل، لذا أجيبك هنا وفي مجموعة العائلة، ويمكنك أن تسألني " +
		"عن أي شيء يعرفه المنزل.\n\nما ليس لديك هو ذاكرة خاصة بك. لا توجد هنا مساحة " +
		"لك ولي وحدنا: كل ما أتذكره منك يذهب إلى الذاكرة المشتركة للمنزل، حيث يمكن " +
		"للجميع قراءته. لذلك أعرض عليك النص بالضبط أولاً ولا أكتب شيئاً حتى توافق، في " +
		"هذه المحادثة كما في المجموعة، لأنها الذاكرة نفسها في الحالتين.\n\nإن كنت " +
		"تفضّل أن تكون لك ذاكرة خاصة، فاطلب ذلك ممن قام بإعدادي. لا يُنقل شيء عند " +
		"تغيير ذلك، لأنه لم يُحفظ شيء قط.",
	EnrolGroupHeading: "المحادثة الجماعية مشتركة",
	EnrolGroupBody: "المحادثة الجماعية للمنزل هي الذاكرة المشتركة. كل ما أتذكره هناك يراه الجميع. " +
		"لا شيء ينتقل من تلقاء نفسه: إذا أردت أن يصبح شيء خاص مشتركًا، اطلب مني ذلك، " +
		"وسأعرض عليك النص بالضبط قبل أن ينتقل أي جزء منه.",
	EnrolMemoryHeading: "ماذا يحدث عندما أدوّن شيئًا",
	// The button names in this prose are byte-identical to BtnSaveHousehold and
	// BtnDontSave. «» are U+00AB and U+00BB, the standard Arabic quotation pair,
	// and they are prose rather than markup, so Esc leaves them alone.
	EnrolMemoryBodyDefault: "عندما يبدو شيء جديرًا بالحفظ في ذاكرتك الخاصة، أكتبه ثم أعرض عليك بالضبط ما كتبته " +
		"وفي أي ذاكرة وضعته، مع زر تراجع يسترجعه — وإذا ضغطته، لن يظهر مرة أخرى في أي إجابة، لا هنا ولا على أي جهاز آخر في المنزل. " +
		"وأي شيء للذاكرة المشتركة في المنزل أسألك عنه أولًا " +
		"ولا أكتب شيئًا حتى تضغط «حفظ في ذاكرة المنزل».\n\n" +
		"في الحالتين ترى كل شيء دائمًا. هذا كل ما في الأمر. تحدّث معي بشكل طبيعي.",
	EnrolMemoryBodyAsk: "لا أحفظ أي شيء من تلقاء نفسي. عندما يبدو شيء جديرًا بالحفظ سأسألك — سترى ما كنت سأكتبه " +
		"وفي أي ذاكرة سيوضع، وتختار ذاكرة أو تضغط «لا تحفظ». إذا لم تجب، لا أحفظه.\n\n" +
		"هذا كل ما في الأمر. تحدّث معي بشكل طبيعي.",

	// The only table where this is not the identity. It is per-language behaviour
	// and deliberately not in transport's formatting code, which owns markup and
	// nothing else.
	Notice: func(s string) string { return arRLI + s + arPDI },
}
