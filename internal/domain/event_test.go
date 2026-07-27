package domain

import (
	"testing"
	"time"
)

func TestClassifyUpdateCadenceRequiresTwoUpdatesPerWeekForFrequent(t *testing.T) {
	first := time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name  string
		count int
		last  time.Time
		want  UpdateCadence
	}{
		{name: "two per week", count: 3, last: first.Add(7 * 24 * time.Hour), want: UpdateCadenceFrequent},
		{name: "slower than two per week", count: 3, last: first.Add(8 * 24 * time.Hour), want: UpdateCadenceUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyUpdateCadence(tt.count, first, tt.last); got != tt.want {
				t.Fatalf("ClassifyUpdateCadence() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAverageUpdateInterval(t *testing.T) {
	first := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if got := AverageUpdateInterval(7, first, first.Add(7*24*time.Hour)); got != 28*time.Hour {
		t.Fatalf("AverageUpdateInterval() = %s, want 28h", got)
	}
	if got := AverageUpdateInterval(1, first, first); got != 0 {
		t.Fatalf("one observation interval = %s, want zero", got)
	}
}
