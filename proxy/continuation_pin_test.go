package proxy

import "testing"

func TestCodexContinuationPinned(t *testing.T) {
	tests := []struct {
		name        string
		turnState   bool
		previous    bool
		affinity    bool
		hardOwnerID int64
		want        bool
	}{
		{name: "fresh turn with ordinary binding", affinity: true},
		{name: "turn state with ordinary binding", turnState: true, affinity: true, want: true},
		{name: "turn state with hard owner after restart", turnState: true, hardOwnerID: 7, want: true},
		{name: "previous response with hard owner after restart", previous: true, hardOwnerID: 7, want: true},
		{name: "previous response without owner remains hint", previous: true, affinity: true},
		{name: "fresh turn with hard owner", hardOwnerID: 7},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := codexContinuationPinned(test.turnState, test.previous, test.affinity, test.hardOwnerID)
			if got != test.want {
				t.Fatalf("codexContinuationPinned() = %v, want %v", got, test.want)
			}
		})
	}
}
