package memory

import "strings"

// denyPatterns are case-insensitive substrings that mark a candidate note as
// instruction-like / privilege-escalating and therefore unsafe to persist
// (ADR-020 §2 抗投毒 / ADR-021 §4). The memory block is reloaded as butler
// context every turn, so a persisted "ignore previous instructions" or
// "always bypass permissions" note would amplify a one-off prompt injection
// into a durable cross-session compromise. We drop the whole note on any hit.
//
// The list is intentionally conservative (favor dropping a borderline note
// over persisting a poisoned one): it targets meta-instructions about the
// assistant's behavior/permissions/identity, not ordinary content.
var denyPatterns = []string{
	// English instruction-injection markers.
	"ignore previous",
	"ignore all previous",
	"ignore the above",
	"disregard previous",
	"disregard the above",
	"system prompt",
	"system-prompt",
	"developer message",
	"bypass",
	"bypasspermissions",
	"dangerously-skip",
	"skip permission",
	"you are now",
	"act as",
	"from now on",
	"new instructions",
	"override",
	"jailbreak",
	"<system>",
	"assistant:",

	// Chinese instruction-injection markers.
	"忽略之前",
	"忽略上面",
	"忽略以上",
	"忽略前面",
	"忘记之前",
	"忘记上面",
	"你现在是",
	"你是一个",
	"扮演",
	"系统提示",
	"绕过",
	"越权",
	"从现在起",
	"以后总是",
	"以后都要",
}

// IsPoisoned reports whether text contains any instruction-like / escalation
// marker and must not be persisted into long-term memory.
func IsPoisoned(text string) bool {
	low := strings.ToLower(text)
	for _, p := range denyPatterns {
		if strings.Contains(low, p) {
			return true
		}
	}
	return false
}
