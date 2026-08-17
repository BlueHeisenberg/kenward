package lang

import (
	"fmt"
	"strings"
	"time"

	"github.com/BlueHeisenberg/kenward/internal/remind"
	"github.com/BlueHeisenberg/kenward/internal/transport"
)

// Simplified Chinese.
//
// Conventions applied throughout:
//
//   - Prose punctuation is full-width: ，。：？（）. The em dash is 破折号 ——, two
//     glyphs forming one unit.
//   - The brackets on the reminder notices stay ASCII. They are the same bytes the
//     code emits and the same bytes a support log greps for; making the marker
//     language-dependent buys no reader anything. What is inside them is ordinary
//     Chinese prose and does use full-width punctuation.
//   - One space between a Latin or digit run and an adjacent CJK character, and none
//     between a Latin run and full-width punctuation, which carries its own
//     side-bearing. So 代码 {id} and {n} 条记录 and （{n} 条记录）.
//   - Nouns do not inflect for number, so every count in the English collapses to one
//     form. TierWord does not collapse: 这个 against 这些 is a real distinction, made
//     on the demonstrative rather than on the noun.
//   - There is no grammatical case, so the destination needs no split — one noun
//     phrase is correct in all nine sentences. This is the mirror image of the German
//     problem, and the reason the API takes a bool rather than a phrase: the split
//     costs Chinese one line per entry and saves German from being wrong.

var zhWeekdays = [7]string{"星期日", "星期一", "星期二", "星期三", "星期四", "星期五", "星期六"}

var chinese = Catalogue{
	Tag:         Chinese,
	Name:        "中文",
	EnglishName: "Chinese",

	Locked:        "你的助手已锁定，需要在它运行的那台机器上解锁。",
	ContentFilter: "模型拒绝回答你的消息。",
	Queued:        "我还在处理你上一条消息，这一条已经排队，接下来就轮到它。",
	Dropped:       "我这边积压了，只好丢掉了那条消息。过一会儿再发一次。",
	NoAnswer:      "我没有得到可用的回答。再问一次试试。",
	ToolMisfire:   "我想为此做点什么，但做错了，所以什么都没发生。再问我一次。",
	NothingSaved:  "我刚才什么都没记下。如果想让我记住，再说一遍。",
	ResetNotice:   "重新开始——我已经清掉了这段对话较早的部分。你的记忆没有任何变化，这是预定的重置。",

	BareAcknowledgements: []string{
		"好的", "好", "明白", "明白了", "知道了", "了解", "收到",
		"记下了", "已记录", "已保存", "没问题", "行",
	},

	// Chinese puts no space anywhere useful, so every entry here is matched as a
	// substring of a longer clause — "帮你记下了" has to hit "记下".
	SaveClaims: []string{
		"已记录", "已保存", "记下", "记住", "会记住", "记录下来", "保存好",
		"收到了",
		"存好了", "已经记", "帮你记", "添加到你的", "在你的记忆里",
	},

	ModelBusy:         "模型现在很忙。过一会儿再试。",
	Misconfigured:     "这个家庭的配置有问题——请告诉负责运行它的人。",
	TurnFailed:        "连接模型时出了问题，你的消息没有得到回答。过一会儿再试。",
	ReasoningOnly:     "模型一直在思考，没来得及给出回答。没有出故障——再问一次，或者把问题拆小一些。",
	RefusalEmptyChain: "没有任何机器被配置来回答这段对话。请负责运行这个节点的人配置一台。",

	// {tried} is followed directly by 这段对话, with no space. The English needs a
	// space after it because an English sentence boundary is a space; the Chinese
	// values end in 。, which already contains the boundary. A hardcoded "%s " join
	// would emit a stray space here — which is why the space belongs to this
	// template and not to a shared format string.
	RefusalAssembled: func(whose, chain, tried, tierWord string) string {
		return fmt.Sprintf("%s（%s）里现在没有任何机器可以连上。%s这段对话仅限于%s，所以我不会把它发到别处。唤醒其中一台，然后再问一次。",
			whose, chain, tried, tierWord)
	},
	WhoseDirect: "你允许的层级",
	WhoseGroup:  "这个家庭允许的层级",
	TierWord: func(n int) string {
		if n == 1 {
			return "这个层级"
		}
		return "这些层级"
	},
	// Chinese enumerates with the 顿号 、 and has no coordinating word before the
	// last item, so the separator and the conjunction are the same character. ，
	// would be wrong: it separates clauses, not list items.
	Chain: func(names []string) string { return codeJoin(names, "、") },
	Tried: func(names []string) string {
		items := codeAll(names)
		switch len(items) {
		case 0:
			return "它们都没有可以连接的地址。"
		case 1:
			return items[0] + "无法连接。"
		default:
			// 都 carries the plurality the English gets from "were".
			return strings.Join(items, "、") + "都无法连接。"
		}
	},

	Searched:    func(parts []string) string { return "已检索" + strings.Join(parts, "、") },
	PartPrivate: func(count string) string { return "你的私人记忆" + count },
	PartShared:  func(count string) string { return "家庭记忆" + count },
	// One plural form for every n. 条 does not agree, so （0 条记录）,（1 条记录）
	// and（7 条记录）are all correct from one template. Zero keeps a lexical form
	// because the English countZero is a word rather than a count, and dropping that
	// would flatten the line for no gain.
	Count: func(unreadable bool, n int) string {
		switch {
		case unreadable:
			return "（无法读取）"
		case n == 0:
			return "（没有）"
		default:
			return fmt.Sprintf("（%d 条记录）", n)
		}
	},

	RemindFull:    "[你的提醒数量已经到了我能保存的上限——先取消一个]",
	RemindPast:    "[那个时间已经过去了，所以我没有设置任何提醒]",
	RemindFailed:  "[我没能设置那条提醒]",
	UnremindNone:  "[没有这个代码对应的提醒]",
	UnremindFails: "[我没能取消那条提醒]",
	ReminderSet: func(when, text, id string) string {
		return "[提醒已设置，" + when + "：" + transport.Esc(text) + "——代码 " + id + "]"
	},
	ReminderCancelled: func(text string) string {
		return "[提醒已取消：" + transport.Esc(text) + "]"
	},
	// Chinese has no "at" preposition for a clock time in this position;
	// juxtaposition is the idiom, and 在 would be over-formal for the register.
	// The one-off date is the numeric written form 8月15日, not the prose month
	// name 八月 — they are two different jobs and this is the one that wants digits.
	When: func(r remind.Reminder, loc *time.Location) string {
		at := " " + clock(r)
		switch r.Every {
		case remind.EveryDaily:
			return "每天" + at
		case remind.EveryWeekly:
			return "每" + zhWeekdays[r.Weekday] + at
		default:
			d := r.Next.In(loc)
			return fmt.Sprintf("%d月%d日%s", int(d.Month()), d.Day(), at)
		}
	},

	SaveFailed: "我没能保存那条记录——什么都没有写入。",
	AskFailed: func(title string) string {
		return "我本想问你要不要保存" + transport.Bold(title) + "，但那个问题没有发出去。什么都没有写入。"
	},
	Saved: func(private bool, title string) string {
		if private {
			return "已把" + transport.Bold(title) + "保存到你的私人记忆。"
		}
		return "已把" + transport.Bold(title) + "保存到家庭记忆。"
	},
	SavedNoUndo: func(private bool, title string) string {
		where := "家庭记忆"
		if private {
			where = "你的私人记忆"
		}
		return "已把" + transport.Bold(title) + "保存到" + where + "，但撤销按钮没有发出去，所以我在这里没法收回它。"
	},
	Removed: func(private bool, title string) string {
		where := "家庭记忆"
		if private {
			where = "你的私人记忆"
		}
		return "已把" + transport.Bold(title) + "从" + where + "中移除。它不会再出现在任何回答里，这里不会，家里的其他设备上也不会。"
	},
	UndoFailed: func(private bool, title string) string {
		where := "家庭记忆"
		if private {
			where = "你的私人记忆"
		}
		return "我没能收回它：" + transport.Bold(title) + "仍然在" + where + "里。"
	},
	StoreRefused: func(private bool, title string) string {
		where := "家庭记忆"
		if private {
			where = "你的私人记忆"
		}
		return "我没能把" + transport.Bold(title) + "保存到" + where + "——记忆存储拒绝了这次写入，所以什么都没有存下。"
	},
	WrongSpace: func(title string) string {
		return "出了问题：我没有把" + transport.Bold(title) + "存到它应该去的地方。在再次保存之前，请告诉负责运行这个节点的人。"
	},
	PublishNoShared:   "我没能发布那条记录——什么都没有发布。",
	PublishUnreadable: "我没能读取那条记录，所以什么都没有发布。",
	PublishAskFailed: func(title string) string {
		return "我本想问你要不要发布" + transport.Bold(title) + "，但那个问题没有发出去。什么都没有发布。"
	},
	PublishRefused: func(title string) string {
		return "我没能发布" + transport.Bold(title) + "——记忆存储拒绝了这次复制，所以没有任何内容进入家庭记忆。"
	},
	PublishWrongSpace: func(title string) string {
		return "出了问题：我没有把" + transport.Bold(title) + "发布到你选择的地方。请告诉负责运行这个节点的人。"
	},
	Published: func(title string) string {
		return "已把" + transport.Bold(title) + "发布到家庭记忆。现在家里所有人都能看到它。"
	},

	OnlyOneProposal: "我一次只问一件事；那条消息里的其他内容都没有保存。",

	ProposalOpener: "这件事我可以记下来：",
	ProposalNoDest: "我该把它保存到哪里？",
	ProposalWithDest: func(private bool) string {
		if private {
			return "把它保存到你的私人记忆吗？"
		}
		return "把它保存到家庭记忆吗？"
	},
	UndoExpiredNote: "撤销窗口已经关闭，这条内容仍然在你的私人记忆里",
	WrittenOpener: func(private bool) string {
		if private {
			return "我已经把这条内容写入你的私人记忆："
		}
		return "我已经把这条内容写入家庭记忆："
	},
	WrittenHint:     "撤销按钮会把它移除。",
	PromotionOpener: "这条内容会原样发布给整个家庭，并且无法撤回：",
	PromotionCloser: "要发布吗？",
	AlsoKnownAs:     func(words []string) string { return "也称：" + strings.Join(words, "、") },
	EnglishGloss:    func(summary string) string { return "上面的内容以英文保存，意思是：" + summary },

	BtnUndo:             "撤销",
	BtnPublishHousehold: "发布给家庭",
	BtnCancel:           "取消",
	BtnSavePersonal:     "保存到个人",
	BtnDontSave:         "不保存",
	BtnPersonal:         "个人",
	BtnHousehold:        "家庭",
	BtnSaveHousehold:    "保存到家庭",

	// 破折号 occupies its own full-width cells and takes no following space.
	Dash:      "——",
	Declined:  "没有回答，视为拒绝",
	Withdrawn: "问题已撤回",

	EnrolPrivateHeading: "这个聊天是私密的",
	EnrolPrivateBody: "这个聊天——只有你和我——就是你的私人记忆。你在这里告诉我的内容，会留在你自己的空间里。" +
		"家里的其他人都读不到，我也不会在群里提起。",
	EnrolGroupHeading: "群聊是共享的",
	EnrolGroupBody: "家庭群聊就是共享记忆。我在那里记下的一切，所有人都能看到。没有任何内容会自己越过界线：" +
		"如果有私密的内容需要变成共享的，告诉我，在它挪动之前，我会先把确切的文字给你看。",
	EnrolMemoryHeading: "我记下某件事的时候会发生什么",
	EnrolMemoryBodyDefault: "当某件事听起来值得存进你自己的记忆时，我会记下来，然后原样告诉你我写了什么、存进了哪个记忆，" +
		"并附上一个撤销按钮，按一下就能收回。凡是要进入家庭共享记忆的内容，我都会先问你，在你点“保存到家庭”之前不会写入任何东西。\n\n" +
		"无论哪种情况，你总能看到。就这些。像平常一样跟我说话就好。",
	EnrolMemoryBodyAsk: "我从不自己保存任何东西。当某件事听起来值得留下时，我会先问你——你会看到我打算写下什么、存进哪个记忆，" +
		"然后你选一个记忆，或者点“不保存”。如果你不回答，我就不保存。\n\n" +
		"就这些。像平常一样跟我说话就好。",

	Notice: identity,
}
