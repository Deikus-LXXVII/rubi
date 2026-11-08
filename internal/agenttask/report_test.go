package agenttask

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTaskReportRequiresExplicitFinalEvidence(t *testing.T) {
	for _, text := range []string{"ordinary reply", "STATUS: PASS", "STATUS: PASS\nCOMMIT: none", "STATUS: PASS\nCOMMIT: none\nEVIDENCE: tests passed\nBut I have a question", "STATUS: PASS\nCOMMIT: none\nEVIDENCE: old report\nSTATUS: BLOCKED\nCOMMIT: none\nEVIDENCE: need input"} {
		r := ReportFromText(text)
		if r.Status == "PASS" {
			t.Fatal("incomplete or stale report permits finalization")
		}
	}
	text := "Implemented change\nSTATUS: PASS\nCOMMIT: abc123\nEVIDENCE: go test ./...: pass\nEVIDENCE: integration: pass"
	r := ReportFromText(text)
	if r.Status != "PASS" || !strings.Contains(r.Tests, "go test") {
		t.Fatal("explicit report lost")
	}
	r = ReportFromText(strings.Repeat("界", 10000))
	if len([]rune(r.Work)) > 4096 {
		t.Fatal("unbounded agent text")
	}
}
func TestTaskLegacyLaunchCannotBeDeclaredQuiescent(t *testing.T) {
	st, w := taskFixture(t)
	// An older Home's record has no supervision version. Never infer absence of
	// processes from the absence of newly introduced receipt files.
	state, err := st.Get(t.Context(), w.ID)
	if err != nil {
		t.Fatal(err)
	}
	state.Spec.Supervision = 0
	state.Phase = Stopped
	data, _ := json.Marshal(state)
	err = os.WriteFile(filepath.Join(st.Home, "t3", "task-state", w.ID+".json"), data, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err = st.Quiesce(t.Context(), w.ID); err == nil {
		t.Fatal("legacy worker declared stopped")
	}
}
