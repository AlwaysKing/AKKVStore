package admin

import "testing"

func TestCompactCount(t *testing.T) {
	tests := map[int64]string{
		0:             "0",
		999:           "999",
		1000:          "1K",
		1500:          "1.5K",
		1_000_000:     "1M",
		1_500_000:     "1.5M",
		999_950:       "1M",
		999_999:       "1M",
		999_950_000:   "1B",
		2_000_000_000: "2B",
	}
	for input, want := range tests {
		if got := compactCount(input); got != want {
			t.Fatalf("compactCount(%d) = %q, want %q", input, got, want)
		}
	}
}
