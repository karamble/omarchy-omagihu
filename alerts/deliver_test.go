package alerts

import "testing"

// TestAgentForRepo pins the rule that decides who is woken by --deliver repo:
// the agent working in that checkout, and nobody who merely looks like it.
func TestAgentForRepo(t *testing.T) {
	agents := []Agent{
		{PaneID: "w1:p1", CWD: "/home/dev/src/other"},
		{PaneID: "w2:p1", CWD: "/home/dev/src/radar"},
		{PaneID: "w3:p1", CWD: "/home/dev/src/radar/forge"},
		{PaneID: "w4:p1", CWD: "/home/dev/src/radar-notes"},
	}

	tests := []struct {
		name  string
		entry map[string]any
		want  string
		found bool
	}{
		{
			name:  "an agent sitting at the checkout",
			entry: map[string]any{"path": "/home/dev/src/radar"},
			want:  "w2:p1",
			found: true,
		},
		{
			name:  "an agent further down the checkout",
			entry: map[string]any{"path": "/home/dev/src/radar/forge"},
			want:  "w3:p1",
			found: true,
		},
		{
			name:  "a sibling sharing a name prefix is not inside it",
			entry: map[string]any{"path": "/home/dev/src/radar-notes/deep"},
			found: false,
		},
		{
			name:  "nobody is working there",
			entry: map[string]any{"path": "/home/dev/src/quiet"},
			found: false,
		},
		{
			name:  "an alarm with no checkout to speak of",
			entry: map[string]any{},
			found: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := agentForRepo(agents, Fire{Entry: tc.entry})
			if ok != tc.found {
				t.Fatalf("found = %v, want %v", ok, tc.found)
			}
			if ok && got != tc.want {
				t.Fatalf("agent = %q, want %q", got, tc.want)
			}
		})
	}
}
