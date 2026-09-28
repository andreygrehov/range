package core

import (
	"testing"
)

func TestMetricDefinitions(t *testing.T) {
	s := Stats{
		Size: 100 << 30, WorkingSetBytes: 50 << 20,
		SessionRemoteBytes: 75 << 20, SessionRequests: 60,
	}
	if got, want := s.Amplification(), 1.5; got != want {
		t.Fatalf("amplification = %.2f, want %.2f (remote transfer over working set)", got, want)
	}
	if got := s.WorkingSetRatio(); got != float64(50<<20)/float64(100<<30)*100 {
		t.Fatalf("workingSetRatio = %.6f", got)
	}
	if got := s.TransferRatio(); got != float64(75<<20)/float64(100<<30)*100 {
		t.Fatalf("transferRatio = %.6f", got)
	}
	if s.TransferRatio() <= s.WorkingSetRatio() {
		t.Fatal("refetching should make the transfer ratio exceed the working-set ratio")
	}
	empty := Stats{}
	if empty.Amplification() != 0 || empty.WorkingSetRatio() != 0 || empty.TransferRatio() != 0 {
		t.Fatal("an empty snapshot must not divide by zero")
	}
}
