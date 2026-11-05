package v1alpha1

import (
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/limitsdoc"
)

// largestAccepted returns the greatest n below ceiling for which accepts holds, given that it holds for 1 and stops
// holding past some n. It reads a limit that the code has no constant for, such as a rule of Kubernetes, from what
// Validate does.
func largestAccepted(t *testing.T, ceiling int, accepts func(n int) bool) int {
	t.Helper()
	if !accepts(1) {
		t.Fatal("a value of 1 is not accepted: the probe has no ground to stand on")
	}
	largest := 1
	for n := 2; n < ceiling; n++ {
		if accepts(n) {
			largest = n
		}
	}
	return largest
}

// TestLimitsPage holds the page docs/reference/limits.md to the limits of this package.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Count(t, "triggers", maxTriggers)
	limitsdoc.Count(t, "extract", maxExtract)
	limitsdoc.Count(t, "messageFields", len(messageFields()))
	limitsdoc.Count(t, "regex", maxTriggerRegexLen)
	limitsdoc.Count(t, "regexInstructions", maxRegexInstructions)
	limitsdoc.Count(t, "bindingRegexInstructions", maxBindingRegexInstructions)
	limitsdoc.Count(t, "promptTemplate", maxPromptTemplateLen)
	limitsdoc.Count(t, "agentName", maxAgentNameLen)
	limitsdoc.Count(t, "userId", maxUserIDLen)
	limitsdoc.Count(t, "headers", maxHeaders)
	limitsdoc.Count(t, "headerName", maxHeaderNameLen)
	limitsdoc.Count(t, "mentions", maxMentions)
	limitsdoc.Count(t, "mentionsPerTurnDefault", DefaultMaxMentionsPerTurn)
	limitsdoc.Count(t, "approverGroups", maxApproverGroups)

	limitsdoc.Duration(t, "cooldownMin", minThreadCooldown)
	limitsdoc.Duration(t, "cooldownMax", maxThreadCooldown)
	limitsdoc.Duration(t, "maxIdleMax", MaxThreadMaxIdle)
	limitsdoc.Duration(t, "maxIdleDefault", DefaultMaxIdle)
	limitsdoc.Duration(t, "rateWindowMin", MinRateWindow)
	limitsdoc.Duration(t, "rateWindowMax", MaxLimitWindow)
	limitsdoc.Duration(t, "triggerRuns", MaxLimitWindow)
	limitsdoc.Duration(t, "botDay", MaxLimitWindow)
	limitsdoc.Duration(t, "interactionTimeout", DefaultInteractionTimeout)
	limitsdoc.Duration(t, "interactionTimeoutMax", MaxInteractionTimeout)
	limitsdoc.Duration(t, "threadTTL", DefaultThreadTTL)
	limitsdoc.Duration(t, "threadTTLMax", MaxThreadTTL)

	limitsdoc.Text(t, "templateFuncs", allowedFuncList())
	limitsdoc.Bytes(t, "promptOutput", MaxPromptOutput)
	limitsdoc.Count(t, "placeholder", placeholderLen)
}

// TestLimitsPageByWhatValidateDoes reads the limits that Kubernetes names, and the cap of mentions per turn, which has no
// constant, from the largest value that Validate accepts.
func TestLimitsPageByWhatValidateDoes(t *testing.T) {
	binding := func(mutate func(*Binding)) error {
		b := load[Binding](t, "binding.yaml")
		mutate(b)
		return b.Validate()
	}
	limitsdoc.Count(t, "mentionsPerTurnMax", largestAccepted(t, 40, func(n int) bool {
		return binding(func(b *Binding) { b.Spec.Chat.MaxMentionsPerTurn = &n }) == nil
	}))
	limitsdoc.Count(t, "nameLen", largestAccepted(t, 400, func(n int) bool {
		return binding(func(b *Binding) { b.Name = strings.Repeat("a", n) }) == nil
	}))
	limitsdoc.Count(t, "namespaceLen", largestAccepted(t, 100, func(n int) bool {
		return binding(func(b *Binding) { b.Namespace = strings.Repeat("a", n) }) == nil
	}))
	limitsdoc.Count(t, "mentionNameLen", largestAccepted(t, 100, func(n int) bool {
		return binding(func(b *Binding) {
			b.Spec.Chat.Mentions = []ChatMention{{Name: strings.Repeat("a", n), User: "U0123456789"}}
		}) == nil
	}))
}
