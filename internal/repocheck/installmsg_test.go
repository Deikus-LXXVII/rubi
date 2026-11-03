package repocheck

import (
	"strings"
	"testing"
)

// installMessage is what the user sends their Bot to install Rubi (project rule 2, T-06): a sentence with a link,
// no shell line. The Bot reads the instructions at the link, which the Pages deploy publishes from the Bot skill.
const installMessage = "Install Rubi-Project by following the instructions at https://rubi-panel.com/install.md"

func TestTheInstallMessageIsALinkNotAShellLine(t *testing.T) {
	for _, s := range []string{"curl", "| sh", "|sh", "sudo", "$("} {
		if strings.Contains(installMessage, s) {
			t.Fatalf("the install message holds %q", s)
		}
	}
	for _, rel := range []string{"README.md", "docs/user/getting-started.md", "panel/index.html"} {
		text := readRepo(t, rel)
		if !strings.Contains(text, installMessage) {
			t.Errorf("%s does not give the install message %q", rel, installMessage)
		}
		if strings.Contains(text, "rubi-panel.com/install.sh") {
			t.Errorf("%s still shows the installer's shell line to the user", rel)
		}
	}
}

func TestTheInstallLinkIsPublishedFromTheBotSkill(t *testing.T) {
	pages := readRepo(t, ".github/workflows/pages.yml")
	if !strings.Contains(pages, "cp skills/grok-bot/rubi-project/SKILL.md panel/install.md") {
		t.Fatal("the Pages deploy does not publish the Bot skill at rubi-panel.com/install.md, which the install message names")
	}
	skill := readRepo(t, "skills/grok-bot/rubi-project/SKILL.md")
	if !strings.Contains(skill, "## Install") || !strings.Contains(skill, "rubi-panel.com/install.sh") {
		t.Fatal("the Bot skill no longer says how to install Rubi; the install link would lead nowhere useful")
	}
}
