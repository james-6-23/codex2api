package database

import "testing"

func TestAccountRowGetCredentialInt64SliceNormalizesValues(t *testing.T) {
	row := &AccountRow{
		Credentials: map[string]interface{}{
			"allowed_api_key_ids": []interface{}{float64(3), float64(1), float64(3), float64(0)},
		},
	}

	got := row.GetCredentialInt64Slice("allowed_api_key_ids")
	want := []int64{1, 3}
	if len(got) != len(want) {
		t.Fatalf("len(got) = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %d, want %d (full=%v)", i, got[i], want[i], got)
		}
	}
}

func TestAccountRowGetCredentialInt64SliceMissingFieldReturnsEmptySlice(t *testing.T) {
	row := &AccountRow{Credentials: map[string]interface{}{}}
	got := row.GetCredentialInt64Slice("allowed_api_key_ids")
	if len(got) != 0 {
		t.Fatalf("got = %v, want empty slice", got)
	}
}

func TestNormalizeCodexCLIVersionSyncIntervalHours(t *testing.T) {
	cases := map[int]int{
		0:    12,
		-5:   12,
		1:    1,
		12:   12,
		720:  720,
		721:  720,
		9999: 720,
	}
	for in, want := range cases {
		if got := NormalizeCodexCLIVersionSyncIntervalHours(in); got != want {
			t.Errorf("NormalizeCodexCLIVersionSyncIntervalHours(%d) = %d, want %d", in, got, want)
		}
	}
}
