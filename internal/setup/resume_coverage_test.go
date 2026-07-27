package setup

import (
	"os"
	"regexp"
	"sort"
	"testing"
)

// The resume lists are a hand-maintained copy of the pipeline's status
// vocabulary, and a copy drifts. Twice already: dns_provision was renamed to
// dns_wait without updating the list, and tls_provision was added without being
// added to it — so a restart during either left the session with no goroutine
// driving it, and no step budget to eventually fail it either, because those
// live in the goroutine. It hung forever, silently.
//
// Rather than trust the next person to remember, this reads the pipeline's own
// source for every status it writes and demands each one be classified. Add a
// step, forget the list, and this fails by name.
//
// Source-scanning is deliberate: the alternative, status constants, would only
// move the problem — a new constant can be left out of the lists just as easily.
// What must be checked is what the code actually writes.

// The two files that drive a full_stack session start to finish.
var pipelineSources = []string{
	"orchestrator_fullstack.go",
	"orchestrator_vtc.go",
}

// Both forms the pipeline uses to write the column:
//
//	o.fsSetStatus(sessionID, "step_vta_setup")
//	Updates(map[string]any{"status": "step_import_admin_did", ...})
var statusWritePatterns = []*regexp.Regexp{
	regexp.MustCompile(`fsSetStatus\([A-Za-z0-9_.]+,\s*"([a-z0-9_]+)"`),
	regexp.MustCompile(`"status":\s*"([a-z0-9_]+)"`),
}

func statusesWrittenByPipeline(t *testing.T) []string {
	t.Helper()

	seen := map[string]bool{}
	for _, file := range pipelineSources {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for _, re := range statusWritePatterns {
			for _, m := range re.FindAllSubmatch(src, -1) {
				seen[string(m[1])] = true
			}
		}
	}

	if len(seen) == 0 {
		// The patterns went stale rather than the pipeline going empty —
		// silently matching nothing would make this whole test vacuous.
		t.Fatal("found no status writes; statusWritePatterns no longer matches the pipeline")
	}

	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func TestResumeCoversEveryStatus(t *testing.T) {
	classified := map[string]string{}
	for _, s := range fsResumePreGate {
		classified[s] = "fsResumePreGate"
	}
	for _, s := range fsResumePostGate {
		classified[s] = "fsResumePostGate"
	}
	for _, s := range fsResumeTerminal {
		classified[s] = "fsResumeTerminal"
	}

	for _, status := range statusesWrittenByPipeline(t) {
		if _, ok := classified[status]; !ok {
			t.Errorf("status %q is written by the pipeline but is in no resume list — "+
				"a restart while a session holds it would strand the session with nothing "+
				"driving it and no budget to fail it. Add it to fsResumePreGate (before the "+
				"admin gate), fsResumePostGate (after it), or fsResumeTerminal (nothing owed).",
				status)
		}
	}
}

// The inverse: a status in a resume list that the pipeline never writes is dead
// weight that reads as coverage. dns_provision sat here for exactly as long as
// dns_wait was missing, which is what made the gap easy to miss.
func TestResumeListsHaveNoDeadStatuses(t *testing.T) {
	written := map[string]bool{}
	for _, s := range statusesWrittenByPipeline(t) {
		written[s] = true
	}
	// markFailed lives in orchestrator.go, outside the scanned pipeline files.
	written["failed"] = true

	for _, list := range []struct {
		name    string
		entries []string
	}{
		{"fsResumePreGate", fsResumePreGate},
		{"fsResumePostGate", fsResumePostGate},
		{"fsResumeTerminal", fsResumeTerminal},
	} {
		for _, status := range list.entries {
			if !written[status] {
				t.Errorf("%s lists %q, which the pipeline never writes — "+
					"either it was renamed and this entry was left behind, or it is a typo "+
					"that silently covers nothing", list.name, status)
			}
		}
	}
}

// A status resumed through both entry points would be started twice, so the
// lists must not overlap.
func TestResumeListsAreDisjoint(t *testing.T) {
	first := map[string]string{}
	for _, list := range []struct {
		name    string
		entries []string
	}{
		{"fsResumePreGate", fsResumePreGate},
		{"fsResumePostGate", fsResumePostGate},
		{"fsResumeTerminal", fsResumeTerminal},
	} {
		for _, status := range list.entries {
			if other, dup := first[status]; dup {
				t.Errorf("%q is in both %s and %s", status, other, list.name)
				continue
			}
			first[status] = list.name
		}
	}
}

// The two statuses whose absence caused the bug. Named explicitly so the
// regression has a test that fails for the original reason, not only through
// the general coverage check.
func TestCustomDomainStatusesAreResumable(t *testing.T) {
	inPreGate := map[string]bool{}
	for _, s := range fsResumePreGate {
		inPreGate[s] = true
	}
	for _, status := range []string{"dns_wait", "tls_provision"} {
		if !inPreGate[status] {
			t.Errorf("%q must be in fsResumePreGate: it is a custom-domain step, and a "+
				"restart during it used to hang the session forever", status)
		}
	}
}
