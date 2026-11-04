package repocheck

import (
	"strings"
	"testing"
)

// What a Bot reads when Rubi Home says a feature is not available on a computer (docs/plans/home-platforms.md, stage P7): the
// sentence Home answers and the skill's rule for it say the same words, so the Bot recognises the answer and tells the user
// instead of retrying or hunting for a workaround.
func TestSkillTellsTheBotWhatToDoWhenAFeatureIsNotAvailable(t *testing.T) {
	const phrase = "isn't available on this computer"
	feat := readRepo(t, "internal/home/features.go")
	if !strings.Contains(feat, "That "+phrase) {
		t.Fatalf("Home's sentence no longer contains %q", phrase)
	}
	skill := strings.Join(strings.Fields(readRepo(t, "skills/grok-bot/rubi-project/SKILL.md")), " ")
	for _, want := range []string{`"That ` + phrase + ` (...)"`, "Tell the user once", "Do not try again", "workaround"} {
		if !strings.Contains(skill, want) {
			t.Errorf("SKILL.md lacks %q", want)
		}
	}
}
