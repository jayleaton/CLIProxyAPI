package management

import "testing"

func TestNormalizeRoutingStrategySoonestReset(t *testing.T) {
	for _, input := range []string{"soonest-reset", "SoonestReset", "sr"} {
		got, ok := normalizeRoutingStrategy(input)
		if !ok || got != "soonest-reset" {
			t.Fatalf("normalizeRoutingStrategy(%q) = %q, %v; want soonest-reset, true", input, got, ok)
		}
	}
}
