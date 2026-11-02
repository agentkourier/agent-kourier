package render_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/render"
)

var declared = []v1alpha1.ChatMention{
	{Name: "payments-owner", User: "U0PAYMENTS"},
	{Name: "sam", User: "U0SAM"},
	{Name: "lee", User: "U0LEE"},
}

func TestMentions(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		max        int
		made       []string // the names, in order
		undeclared int
		capped     int
	}{
		{name: "no text", body: "", max: 2},
		{name: "no mention", body: "the checkout pods are crash looping", max: 2},
		{name: "a declared name", body: "ask @sam about it", max: 2, made: []string{"sam"}},
		{name: "at the start, and before punctuation", body: "@sam, then @lee.", max: 2, made: []string{"sam", "lee"}},
		{name: "a name with a hyphen", body: "@payments-owner owns this", max: 2, made: []string{"payments-owner"}},
		{name: "in another case", body: "@Payments-Owner owns this", max: 2, made: []string{"payments-owner"}},
		{name: "in parentheses and emphasis", body: "(@sam) and *@lee*", max: 2, made: []string{"sam", "lee"}},
		{name: "named twice is one mention", body: "@sam and again @sam and @SAM", max: 2, made: []string{"sam"}},
		{name: "in order of first appearance", body: "@lee then @sam", max: 2, made: []string{"lee", "sam"}},
		{name: "past the cap", body: "@sam @lee @payments-owner", max: 2, made: []string{"sam", "lee"}, capped: 1},
		{name: "named twice past the cap counts once", body: "@sam @lee @payments-owner @payments-owner", max: 2, made: []string{"sam", "lee"}, capped: 1},
		{name: "an undeclared name", body: "ask @nobody about it", max: 2, undeclared: 1},
		{name: "an undeclared name twice", body: "@nobody @nobody", max: 2, undeclared: 2},
		{name: "a longer name that starts with a declared one", body: "@sam-2 and @samuel", max: 2, undeclared: 2},
		{name: "a broadcast word is never declared", body: "@here @channel @everyone", max: 2, undeclared: 3},
		{name: "an email address", body: "mail sam@sam or a.b@lee", max: 2},
		{name: "a package scope", body: "bump @sam/thing", max: 2},
		{name: "a host", body: "see @sam.example.com", max: 2},
		{name: "after a word or a path", body: "x@sam /@sam -@sam", max: 2},
		{name: "two at signs", body: "@@sam", max: 2},
		{name: "an at sign alone", body: "@ sam, and @", max: 2},
		{name: "not a name", body: "@1 @-sam", max: 2},
		{name: "in a code span", body: "run `notify @sam` now", max: 2},
		{name: "in a span of two backticks", body: "run ``notify @sam`` now, ask @lee", max: 2, made: []string{"lee"}},
		{name: "after runs that do not match, which the adapter shows as prose", body: "run ``a ` @sam``", max: 2, made: []string{"sam"}},
		{name: "a name in another alphabet is not the name it starts with", body: "@josé é@sam", max: 2, undeclared: 1},
		{name: "a letter that grows when lowered does not reach into a span", body: "@Ⱥ`@sam`", max: 2, undeclared: 1},
		{name: "punctuation of any alphabet ends a name", body: "“@sam”—ask @lee—now", max: 2, made: []string{"sam", "lee"}},
		{name: "an address", body: "@sam@example.com", max: 2},
		{name: "a query value", body: "https://x.test/?u=@sam", max: 2},
		{name: "a port", body: "@sam:8080", max: 2},
		{name: "a colon that ends a clause", body: "@sam: look at this", max: 2, made: []string{"sam"}},
		{name: "a backtick that opens no span", body: "a ` b @sam", max: 2, made: []string{"sam"}},
		{name: "in a fence", body: "```\n@sam\n```\nask @lee", max: 2, made: []string{"lee"}},
		{name: "in a tilde fence", body: "~~~\n@sam\n~~~\nask @lee", max: 2, made: []string{"lee"}},
		{name: "in an indented fence", body: "  ```\n  @sam\n  ```\n@lee", max: 2, made: []string{"lee"}},
		{name: "in a fence that never closes", body: "```\n@sam", max: 2},
		{name: "any fence line closes a block, as the adapter writes it", body: "```\n~~~\n@sam\n", max: 2, made: []string{"sam"}},
		{name: "a block of one line", body: "``` @sam ```\n@lee", max: 2, made: []string{"lee"}},
		{name: "prose after the closing run of a block of one line", body: "``` x ``` ask @sam", max: 2, made: []string{"sam"}},
		{name: "prose after a closing fence", body: "```\nx\n``` ask @sam\n@lee", max: 2, made: []string{"sam", "lee"}},
		{name: "a closing run that is longer than the opener", body: "``` @lee ````` ask @sam", max: 2, made: []string{"sam"}},
		{name: "a run of the other mark closes no block of one line", body: "``` @sam ~~~\n@lee", max: 2},
		{name: "a word after an opener is its info string, whatever it is", body: "```~~~\n@sam", max: 2},
		{name: "a fence line in the first line of a block closes it", body: "``` ~~~ x\n@sam", max: 2, made: []string{"sam"}},
		{name: "any white space before an opener", body: "\u00a0```\n@sam\n```\n@lee", max: 2, made: []string{"lee"}},
		{name: "a name the cut ran into is not the name it starts with", body: "ask @sam" + chat.Truncated, max: 2},
		{name: "a name before the cut", body: "ask @sam, and more" + chat.Truncated, max: 2, made: []string{"sam"}},
		{name: "the mark in the middle of the text is only words", body: "@sam" + chat.Truncated + " and more", max: 2, made: []string{"sam"}},
		{name: "two names with a slash between them", body: "cc @sam/@lee", max: 2, made: []string{"sam", "lee"}},
		{name: "a slash and a name after a path", body: "x/@sam/@lee", max: 2},
		{name: "two names run together read as an address", body: "cc @sam@lee", max: 2},
		{name: "underscore emphasis around more than the name", body: "_cc @sam_", max: 2, made: []string{"sam"}},
		{name: "underscore emphasis of the name", body: "_@sam_ and __@lee__", max: 2, made: []string{"sam", "lee"}},
		{name: "tilde emphasis of the name", body: "~~@sam~~", max: 2, made: []string{"sam"}},
		{name: "an underscore inside a word", body: "@sam_foo", max: 2, undeclared: 1},
		{name: "an underscore after a word", body: "foo_@sam foo~@sam", max: 2},
		{name: "a URL's query, fragment and path", body: "x.test/?@sam ?a=1&@sam /~@sam /#@sam a+@sam 5%@sam", max: 2},
		{name: "an escaped at sign", body: `\@sam`, max: 2},
		{name: "a label with a colon", body: "Owner:@sam", max: 2, made: []string{"sam"}},
		{name: "a host the cut ran into", body: "see @sam." + chat.Truncated, max: 2},
		{name: "a port the cut ran into", body: "see @sam:" + chat.Truncated, max: 2},
		{name: "a word with an underscore the cut ran into", body: "see @sam_" + chat.Truncated, max: 2},
		{name: "a comma before the cut", body: "see @sam," + chat.Truncated, max: 2, made: []string{"sam"}},
		{name: "a tilde does not hide what goes on", body: "@sam~/path @sam~.example.com @sam~:8080", max: 2},
		{name: "a tilde before the cut", body: "see @sam~" + chat.Truncated, max: 2},
		{name: "in a URL, after any separator", body: "https://x.test/?o=a,@sam https://x.test/(@sam) https://x.test/a;@sam https://x.test/a:@sam", max: 2},
		{name: "after a URL", body: "https://x.test/a @sam", max: 2, made: []string{"sam"}},
		{name: "a slash and no name after it", body: "@sam/", max: 2, made: []string{"sam"}},
		{name: "a slash and a space", body: "@sam/ @lee", max: 2, made: []string{"sam", "lee"}},
		{name: "emphasis on each of two names", body: "_@sam_/_@lee_", max: 2, made: []string{"sam", "lee"}},
		{name: "strikethrough on each of two names", body: "~~@sam~~/~~@lee~~", max: 2, made: []string{"sam", "lee"}},
		{name: "emphasis on the second of two names", body: "@sam/_@lee_", max: 2, made: []string{"sam", "lee"}},
		{name: "after a URL in a table", body: "|https://grafana.example/d/1|@sam|", max: 2, made: []string{"sam"}},
		{name: "after a markdown link", body: "[dash](https://a.example/d)@sam", max: 2, made: []string{"sam"}},
		{name: "after a URL in a span", body: "`https://a.example`,@sam", max: 2, made: []string{"sam"}},
		{name: "after a URL in parentheses", body: "(see https://a.example/d),@sam", max: 2, made: []string{"sam"}},
		{name: "after a URL and punctuation of another alphabet", body: "https://a.example/d。@sam お願いします", max: 2, made: []string{"sam"}},
		{name: "a slash and a closing parenthesis", body: "(@sam/)", max: 2, made: []string{"sam"}},
		{name: "a slash and a comma", body: "@sam/, ok", max: 2, made: []string{"sam"}},
		{name: "a slash and a full stop", body: "@sam/.", max: 2, made: []string{"sam"}},
		{name: "a slash and bold on the second name", body: "@sam/**@lee**", max: 2, made: []string{"sam", "lee"}},
		{name: "a glob", body: "npm i @sam/*", max: 2},
		{name: "a brace list", body: "@sam/{api,web}", max: 2},
		{name: "a route", body: "@sam/[id] @sam/(group)/page", max: 2},
		{name: "a dotfile", body: "@sam/.github", max: 2},
		{name: "a path that starts with punctuation", body: "@sam/-x @sam/#readme @sam/%20x @sam//x @sam/$x @sam/+x", max: 2},
		{name: "closing marks that do not end the word", body: "@sam/).x @sam/,x", max: 2},
		{name: "closing marks that end the word", body: "(@sam/). and @lee/,", max: 2, made: []string{"sam", "lee"}},
		{name: "a name the cut ran into after its slash", body: "see @sam/" + chat.Truncated, max: 2},
		{name: "a slash, a mark and the cut", body: "see @sam/," + chat.Truncated, max: 2},
		{name: "a raw user ID, and markup", body: "ask U0SAM or <@U0SAM> or <!channel> or <!subteam^S1>", max: 2, undeclared: 1},
		{name: "the name inside markup", body: "<@sam>", max: 2, made: []string{"sam"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := render.Mentions(tc.body, declared, tc.max)
			var names []string
			for _, m := range got.Made {
				names = append(names, m.Name)
			}
			if !reflect.DeepEqual(names, tc.made) || got.Undeclared != tc.undeclared || got.Capped != tc.capped {
				t.Errorf("Mentions(%q) = made %v, undeclared %d, capped %d; want %v, %d, %d", tc.body, names, got.Undeclared, got.Capped, tc.made, tc.undeclared, tc.capped)
			}
		})
	}
}

// A cut that falls inside a longer name leaves what looks like a declared one, and that notifies nobody.
func TestACutInsideANameNotifiesNobody(t *testing.T) {
	limit := chat.Units("ask @sam" + chat.Truncated)
	body := render.Body([]agent.TextEvent{{ArtifactID: "a", Text: "ask @samuel about the checkout pods today"}}, limit)
	if want := "ask @sam" + chat.Truncated; body != want {
		t.Fatalf("Body = %q, want %q", body, want)
	}
	if got := render.Mentions(body, declared, 2); len(got.Made) != 0 || got.Undeclared != 0 {
		t.Errorf("Mentions(%q) = %+v, want nothing", body, got)
	}
}

// A turn that has mentioned people does not name them again, and they count towards the cap.
func TestMentionsAfterOthers(t *testing.T) {
	got := render.Mentions("@sam @lee @payments-owner", declared, 2, "U0SAM")
	if len(got.Made) != 1 || got.Made[0].Name != "lee" || got.Capped != 1 {
		t.Errorf("Mentions = %+v, want lee, and payments-owner capped", got)
	}
}

// A Binding that declares no one has nothing to scan for, whatever the text says: it counts nothing either.
func TestMentionsWithNoneDeclared(t *testing.T) {
	if got := render.Mentions("@sam @here", nil, 2); len(got.Made) != 0 || got.Undeclared != 0 || got.Capped != 0 {
		t.Errorf("Mentions with none declared = %+v, want nothing", got)
	}
}

// The user of a mention is the one config gives, whatever the case of the name in the text.
func TestMentionsTakesTheUserFromConfig(t *testing.T) {
	got := render.Mentions("@SAM", declared, 2)
	if len(got.Made) != 1 || got.Made[0] != (v1alpha1.ChatMention{Name: "sam", User: "U0SAM"}) {
		t.Errorf("Made = %+v, want the declared entry", got.Made)
	}
}

// The message of a mention holds nothing of the agent's: only its own words, and names and users from config. It is
// made of mentions alone, since a platform may not notify anyone of a mention beside a body (chat.Mention).
func TestMentionMessage(t *testing.T) {
	m := render.MentionMessage(v1alpha1.ChatMention{Name: "sam", User: "U0SAM"})
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	want := []chat.Part{chat.Mention{User: "U0SAM", Text: "was named as sam in the reply above."}}
	if !reflect.DeepEqual(m.Parts, want) {
		t.Errorf("Parts = %+v, want %+v", m.Parts, want)
	}
	if m.Fallback != "The agent named sam in this thread." {
		t.Errorf("Fallback = %q", m.Fallback)
	}
}

func TestMentionPrompt(t *testing.T) {
	if got := render.MentionPrompt(nil); got != "" {
		t.Errorf("MentionPrompt(nil) = %q, want nothing: a Binding with no mentions adds nothing to a prompt", got)
	}
	got := render.MentionPrompt(declared)
	for _, want := range []string{"@name", "payments-owner, sam, lee", "No other name notifies anyone"} {
		if !strings.Contains(got, want) {
			t.Errorf("MentionPrompt = %q, want it to say %q", got, want)
		}
	}
	for _, leak := range []string{"U0SAM", "U0PAYMENTS"} { // the agent has no use for an ID, and never supplies one
		if strings.Contains(got, leak) {
			t.Errorf("MentionPrompt = %q, which holds the user ID %s", got, leak)
		}
	}
}

// One person with two names is one person: mentioned once, and one place of the cap, under the name the text used first.
func TestAPersonWithTwoNamesIsMentionedOnce(t *testing.T) {
	declared := []v1alpha1.ChatMention{{Name: "payments-owner", User: "U0SAM"}, {Name: "billing-owner", User: "U0SAM"}, {Name: "lee", User: "U0LEE"}, {Name: "kim", User: "U0KIM"}}
	got := render.Mentions("@billing-owner @payments-owner @lee", declared, 2)
	want := []v1alpha1.ChatMention{{Name: "billing-owner", User: "U0SAM"}, {Name: "lee", User: "U0LEE"}}
	if !slices.Equal(got.Made, want) || got.Capped != 0 {
		t.Errorf("Made = %v, capped %d; want %v, capped 0", got.Made, got.Capped, want)
	}
	if got := render.Mentions("@lee @kim @payments-owner @billing-owner", declared, 2); got.Capped != 1 {
		t.Errorf("capped = %d, want 1: both names past the cap are one person", got.Capped)
	}
}

// A letter of another alphabet that lowers into ASCII is not the name it looks like: a Kelvin sign is not a k.
func TestALookalikeLetterIsNotADeclaredName(t *testing.T) {
	declared := []v1alpha1.ChatMention{{Name: "kim", User: "U0KIM"}, {Name: "i", User: "U0I"}}
	got := render.Mentions("@\u212Aim @\u0130", declared, 2)
	if len(got.Made) != 0 || got.Undeclared != 2 {
		t.Errorf("Made = %v, undeclared %d; want none, undeclared 2", got.Made, got.Undeclared)
	}
}
