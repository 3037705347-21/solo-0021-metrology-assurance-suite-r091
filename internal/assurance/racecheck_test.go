package assurance

import (
	"context"
	"sync"
	"testing"

	"example.com/solo-0021-metrology-assurance-suite/internal/contracts"
	"example.com/solo-0021-metrology-assurance-suite/internal/repository"
)

func setupCase(t *testing.T, ready bool) (*Service, string) {
	t.Helper()
	repo := repository.New()
	svc := NewService(repo)
	ctx := context.Background()
	device, err := svc.RegisterDevice(ctx, contracts.RegisterDeviceRequest{
		AssetTag: "MET-X1", Model: "M", Room: "R", Steward: "S",
	})
	if err != nil {
		t.Fatal(err)
	}
	opened, err := svc.OpenCase(ctx, contracts.OpenCaseRequest{
		DeviceID: device.ID, Nominal: 25, Tolerance: 0.5, Steward: "S",
	})
	if err != nil {
		t.Fatal(err)
	}
	caseID := opened.Case.ID
	if ready {
		if _, err := svc.RecordMeasurement(ctx, caseID, contracts.RecordMeasurementRequest{
			Observed: 25.1, Technician: "T", Baseline: 1,
		}); err != nil {
			t.Fatal(err)
		}
	}
	return svc, caseID
}

func TestConcurrentMeasurementsExactlyOneWins(t *testing.T) {
	svc, caseID := setupCase(t, false)
	const n = 30
	var wg sync.WaitGroup
	ok, conflict := 0, 0
	var mu sync.Mutex
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := svc.RecordMeasurement(context.Background(), caseID, contracts.RecordMeasurementRequest{
				Observed: 25.1 + float64(i)*0.001, Technician: "T", Baseline: 1,
			})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				ok++
			} else {
				conflict++
			}
		}(i)
	}
	wg.Wait()
	if ok != 1 || conflict != n-1 {
		t.Fatalf("measurements: %d success, %d conflict", ok, conflict)
	}
	view, err := svc.Case(context.Background(), caseID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Case.Version != 2 {
		t.Fatalf("version = %d, want 2", view.Case.Version)
	}
	if len(view.Measurements) != 1 {
		t.Fatalf("measurements stored = %d, want 1", len(view.Measurements))
	}
	measurementEvents := 0
	for _, e := range view.Events {
		if e.Kind == "measurement_accepted" || e.Kind == "measurement_rejected" {
			measurementEvents++
		}
	}
	if measurementEvents != 1 {
		t.Fatalf("measurement events = %d, want 1", measurementEvents)
	}
}

func TestConcurrentSealsExactlyOneWins(t *testing.T) {
	svc, caseID := setupCase(t, true)
	const n = 30
	var wg sync.WaitGroup
	ok, conflict := 0, 0
	var mu sync.Mutex
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := svc.SealCase(context.Background(), caseID, contracts.SealCaseRequest{
				Reviewer: "R", Summary: "s", Baseline: 2,
			})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				ok++
			} else {
				conflict++
			}
		}(i)
	}
	wg.Wait()
	if ok != 1 || conflict != n-1 {
		t.Fatalf("seals: %d success, %d conflict", ok, conflict)
	}
	view, err := svc.Case(context.Background(), caseID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Case.Version != 3 || view.Case.State != "sealed" {
		t.Fatalf("version = %d state = %s", view.Case.Version, view.Case.State)
	}
	if view.Seal == nil {
		t.Fatal("seal missing")
	}
	sealEvents := 0
	for _, e := range view.Events {
		if e.Kind == "case_sealed" {
			sealEvents++
		}
	}
	if sealEvents != 1 {
		t.Fatalf("seal events = %d, want 1", sealEvents)
	}
}

func TestReopenAndMeasurementChainWithBaselines(t *testing.T) {
	svc, caseID := setupCase(t, false)
	// out-of-tolerance measurement at baseline 1 -> version 2, recheck_required
	_, err := svc.RecordMeasurement(context.Background(), caseID, contracts.RecordMeasurementRequest{
		Observed: 30.0, Technician: "T", Baseline: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	// stale reopen must fail
	if _, err := svc.ReopenCase(context.Background(), caseID, contracts.ReopenCaseRequest{
		Actor: "A", Reason: "r", Baseline: 1,
	}); err == nil {
		t.Fatal("stale reopen unexpectedly succeeded")
	}
	// current reopen at baseline 2 -> version 3
	reopened, err := svc.ReopenCase(context.Background(), caseID, contracts.ReopenCaseRequest{
		Actor: "A", Reason: "r", Baseline: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Case.Version != 3 || reopened.Case.State != "awaiting_measurement" {
		t.Fatalf("reopen: version=%d state=%s", reopened.Case.Version, reopened.Case.State)
	}
	// future baseline must fail
	if _, err := svc.RecordMeasurement(context.Background(), caseID, contracts.RecordMeasurementRequest{
		Observed: 25.0, Technician: "T", Baseline: 99,
	}); err == nil {
		t.Fatal("future baseline unexpectedly succeeded")
	}
	// rejected attempt must not have stored a measurement
	mid, err := svc.Case(context.Background(), caseID)
	if err != nil {
		t.Fatal(err)
	}
	if len(mid.Measurements) != 1 {
		t.Fatalf("measurements after rejected attempts = %d", len(mid.Measurements))
	}
	// current measurement at baseline 3 -> version 4
	final, err := svc.RecordMeasurement(context.Background(), caseID, contracts.RecordMeasurementRequest{
		Observed: 25.0, Technician: "T", Baseline: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if final.Case.Version != 4 || final.Case.State != "review_ready" {
		t.Fatalf("final: version=%d state=%s", final.Case.Version, final.Case.State)
	}
	// sealing a sealed case with stale baseline fails with conflict, no second seal
	sealed, err := svc.SealCase(context.Background(), caseID, contracts.SealCaseRequest{
		Reviewer: "R", Summary: "s", Baseline: 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sealed.Case.Version != 5 {
		t.Fatalf("seal version = %d", sealed.Case.Version)
	}
	if _, err := svc.SealCase(context.Background(), caseID, contracts.SealCaseRequest{
		Reviewer: "R2", Summary: "s2", Baseline: 4,
	}); err == nil {
		t.Fatal("stale seal on sealed case unexpectedly succeeded")
	}
}

func TestOmittedBaselineKeepsLegacyBehavior(t *testing.T) {
	svc, caseID := setupCase(t, false)
	if _, err := svc.RecordMeasurement(context.Background(), caseID, contracts.RecordMeasurementRequest{
		Observed: 25.0, Technician: "T", // no baseline
	}); err != nil {
		t.Fatalf("measurement without baseline: %v", err)
	}
	if _, err := svc.SealCase(context.Background(), caseID, contracts.SealCaseRequest{
		Reviewer: "R", Summary: "s", // no baseline
	}); err != nil {
		t.Fatalf("seal without baseline: %v", err)
	}
}
