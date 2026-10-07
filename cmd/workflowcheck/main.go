package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"time"

	"example.com/solo-0021-metrology-assurance-suite/internal/bootstrap"
)

type apiResponse struct {
	Status int
	Body   map[string]any
	Raw    string
}

type harness struct {
	baseURL string
	client  *http.Client
	server  *httptest.Server
}

func main() {
	workflow := flag.String("workflow", "", "workflow id to verify")
	flag.Parse()

	checks := map[string]func() error{
		"register-device":          checkRegisterDevice,
		"open-verification-case":   checkOpenVerificationCase,
		"record-measurement":       checkRecordMeasurement,
		"reopen-verification-case": checkReopenVerificationCase,
		"seal-verification":        checkSealVerification,
		"handling-baseline":        checkHandlingBaseline,
	}
	run, ok := checks[*workflow]
	if !ok {
		fmt.Fprintf(flag.CommandLine.Output(), "unknown workflow %q\n", *workflow)
		flag.Usage()
		os.Exit(2)
	}
	if err := run(); err != nil {
		fmt.Printf("workflow %s: failed: %v\n", *workflow, err)
		os.Exit(1)
	}
	fmt.Printf("workflow %s: ok\n", *workflow)
}

func checkRegisterDevice() error {
	h := newHarness()
	defer h.close()

	response, err := h.request(http.MethodPost, "/devices", map[string]any{
		"asset_tag": " met-1001 ",
		"model":     "Bench Meter",
		"room":      "Room A",
		"steward":   "Lin",
	})
	if err != nil {
		return err
	}
	if err := expectStatus(response, http.StatusCreated); err != nil {
		return err
	}
	id, err := nestedString(response.Body, "device", "id")
	if err != nil {
		return err
	}
	tag, err := nestedString(response.Body, "device", "asset_tag")
	if err != nil {
		return err
	}
	if tag != "MET-1001" {
		return fmt.Errorf("asset tag was not normalized: %q", tag)
	}

	read, err := h.request(http.MethodGet, "/devices/"+id, nil)
	if err != nil {
		return err
	}
	if err := expectStatus(read, http.StatusOK); err != nil {
		return err
	}
	readID, err := nestedString(read.Body, "device", "id")
	if err != nil {
		return err
	}
	if readID != id {
		return fmt.Errorf("read returned device %q, expected %q", readID, id)
	}

	duplicate, err := h.request(http.MethodPost, "/devices", map[string]any{
		"asset_tag": "MET-1001",
		"model":     "Other Meter",
		"room":      "Room B",
		"steward":   "Mira",
	})
	if err != nil {
		return err
	}
	if err := expectStatus(duplicate, http.StatusConflict); err != nil {
		return err
	}
	return expectErrorCode(duplicate, "duplicate_asset_tag")
}

func checkOpenVerificationCase() error {
	h := newHarness()
	defer h.close()

	deviceID, err := h.createDevice("MET-1101")
	if err != nil {
		return err
	}
	opened, err := h.openCase(deviceID)
	if err != nil {
		return err
	}
	caseID, err := nestedString(opened.Body, "case", "id")
	if err != nil {
		return err
	}
	state, err := nestedString(opened.Body, "case", "state")
	if err != nil {
		return err
	}
	if state != "awaiting_measurement" {
		return fmt.Errorf("opened case state = %q", state)
	}

	read, err := h.request(http.MethodGet, "/cases/"+caseID, nil)
	if err != nil {
		return err
	}
	if err := expectStatus(read, http.StatusOK); err != nil {
		return err
	}

	second, err := h.request(http.MethodPost, "/cases", map[string]any{
		"device_id": deviceID,
		"nominal":   25.0,
		"tolerance": 0.5,
		"steward":   "Qiao",
	})
	if err != nil {
		return err
	}
	if err := expectStatus(second, http.StatusConflict); err != nil {
		return err
	}
	return expectErrorCode(second, "active_case_exists")
}

func checkRecordMeasurement() error {
	h := newHarness()
	defer h.close()

	deviceID, err := h.createDevice("MET-1201")
	if err != nil {
		return err
	}
	opened, err := h.openCase(deviceID)
	if err != nil {
		return err
	}
	caseID, err := nestedString(opened.Body, "case", "id")
	if err != nil {
		return err
	}
	accepted, err := h.recordMeasurement(caseID, 25.2, "Rui")
	if err != nil {
		return err
	}
	if err := expectStatus(accepted, http.StatusCreated); err != nil {
		return err
	}
	state, err := nestedString(accepted.Body, "case", "state")
	if err != nil {
		return err
	}
	if state != "review_ready" {
		return fmt.Errorf("accepted measurement state = %q", state)
	}
	acceptedValue, err := nestedBool(accepted.Body, "measurements", 0, "accepted")
	if err != nil {
		return err
	}
	if !acceptedValue {
		return errors.New("in-tolerance measurement was not accepted")
	}

	second, err := h.recordMeasurement(caseID, 25.1, "Rui")
	if err != nil {
		return err
	}
	if err := expectStatus(second, http.StatusConflict); err != nil {
		return err
	}
	if err := expectErrorCode(second, "state_conflict"); err != nil {
		return err
	}

	rejectDeviceID, err := h.createDevice("MET-1202")
	if err != nil {
		return err
	}
	rejectCase, err := h.openCase(rejectDeviceID)
	if err != nil {
		return err
	}
	rejectCaseID, err := nestedString(rejectCase.Body, "case", "id")
	if err != nil {
		return err
	}
	rejected, err := h.recordMeasurement(rejectCaseID, 26.5, "Rui")
	if err != nil {
		return err
	}
	if err := expectStatus(rejected, http.StatusCreated); err != nil {
		return err
	}
	rejectedState, err := nestedString(rejected.Body, "case", "state")
	if err != nil {
		return err
	}
	if rejectedState != "recheck_required" {
		return fmt.Errorf("out-of-tolerance state = %q", rejectedState)
	}
	rejectedValue, err := nestedBool(rejected.Body, "measurements", 0, "accepted")
	if err != nil {
		return err
	}
	if rejectedValue {
		return errors.New("out-of-tolerance measurement was accepted")
	}
	return nil
}

func checkReopenVerificationCase() error {
	h := newHarness()
	defer h.close()

	caseID, err := h.rejectedCase("MET-1301")
	if err != nil {
		return err
	}
	reopened, err := h.request(http.MethodPost, "/cases/"+caseID+"/reopen", map[string]any{
		"actor":  "Lin",
		"reason": "measurement setup was replaced",
	})
	if err != nil {
		return err
	}
	if err := expectStatus(reopened, http.StatusOK); err != nil {
		return err
	}
	state, err := nestedString(reopened.Body, "case", "state")
	if err != nil {
		return err
	}
	if state != "awaiting_measurement" {
		return fmt.Errorf("reopened case state = %q", state)
	}
	measurements, err := nestedSlice(reopened.Body, "measurements")
	if err != nil {
		return err
	}
	if len(measurements) != 1 {
		return fmt.Errorf("reopen changed measurement history: %d entries", len(measurements))
	}

	second, err := h.request(http.MethodPost, "/cases/"+caseID+"/reopen", map[string]any{
		"actor":  "Lin",
		"reason": "duplicate request",
	})
	if err != nil {
		return err
	}
	if err := expectStatus(second, http.StatusConflict); err != nil {
		return err
	}
	if err := expectErrorCode(second, "state_conflict"); err != nil {
		return err
	}

	events, err := h.request(http.MethodGet, "/cases/"+caseID+"/events", nil)
	if err != nil {
		return err
	}
	if err := expectStatus(events, http.StatusOK); err != nil {
		return err
	}
	if !hasEvent(events.Body, "case_reopened") {
		return errors.New("reopen audit event is missing")
	}
	return nil
}

func checkSealVerification() error {
	h := newHarness()
	defer h.close()

	caseID, err := h.readyCase("MET-1401")
	if err != nil {
		return err
	}
	type outcome struct {
		response apiResponse
		err      error
	}
	results := make(chan outcome, 2)
	var wait sync.WaitGroup
	for index := 0; index < 2; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			response, err := h.request(http.MethodPost, "/cases/"+caseID+"/seal", map[string]any{
				"reviewer": fmt.Sprintf("Reviewer-%d", index+1),
				"summary":  "accepted measurement sealed for review",
			})
			results <- outcome{response: response, err: err}
		}(index)
	}
	wait.Wait()
	close(results)

	successes := 0
	conflicts := 0
	for item := range results {
		if item.err != nil {
			return item.err
		}
		switch item.response.Status {
		case http.StatusCreated:
			successes++
		case http.StatusConflict:
			conflicts++
		default:
			return fmt.Errorf("unexpected concurrent seal status %d: %s", item.response.Status, item.response.Raw)
		}
	}
	if successes != 1 || conflicts != 1 {
		return fmt.Errorf("concurrent seal outcomes: %d success, %d conflict", successes, conflicts)
	}

	read, err := h.request(http.MethodGet, "/cases/"+caseID, nil)
	if err != nil {
		return err
	}
	if err := expectStatus(read, http.StatusOK); err != nil {
		return err
	}
	state, err := nestedString(read.Body, "case", "state")
	if err != nil {
		return err
	}
	if state != "sealed" {
		return fmt.Errorf("sealed case state = %q", state)
	}
	sealID, err := nestedString(read.Body, "seal", "id")
	if err != nil {
		return err
	}
	if sealID == "" {
		return errors.New("evidence seal is missing")
	}
	return nil
}

func checkHandlingBaseline() error {
	if err := checkStaleMeasurementBaseline(); err != nil {
		return fmt.Errorf("stale measurement: %w", err)
	}
	if err := checkStaleReopenBaseline(); err != nil {
		return fmt.Errorf("stale reopen: %w", err)
	}
	if err := checkStaleSealBaseline(); err != nil {
		return fmt.Errorf("stale seal: %w", err)
	}
	if err := checkConcurrentBaselineSeal(); err != nil {
		return fmt.Errorf("concurrent seal: %w", err)
	}
	if err := checkConcurrentBaselineMeasurement(); err != nil {
		return fmt.Errorf("concurrent measurement: %w", err)
	}
	return nil
}

// checkStaleMeasurementBaseline races two technicians over one case. The
// colleague who commits first advances the count; the technician holding the
// older page must be rejected, and the rejection must leave no measurement,
// case change, or audit event behind.
func checkStaleMeasurementBaseline() error {
	h := newHarness()
	defer h.close()

	deviceID, err := h.createDevice("MET-1501")
	if err != nil {
		return err
	}
	opened, err := h.openCase(deviceID)
	if err != nil {
		return err
	}
	caseID, err := nestedString(opened.Body, "case", "id")
	if err != nil {
		return err
	}
	if err := expectVersion(opened, 1); err != nil {
		return err
	}

	first, err := h.request(http.MethodPost, "/cases/"+caseID+"/measurements", map[string]any{
		"observed":   25.2,
		"technician": "Rui",
		"baseline":   1,
	})
	if err != nil {
		return err
	}
	if err := expectStatus(first, http.StatusCreated); err != nil {
		return err
	}
	if err := expectVersion(first, 2); err != nil {
		return err
	}

	stale, err := h.request(http.MethodPost, "/cases/"+caseID+"/measurements", map[string]any{
		"observed":   25.3,
		"technician": "Tao",
		"baseline":   1,
	})
	if err != nil {
		return err
	}
	if err := expectStatus(stale, http.StatusConflict); err != nil {
		return err
	}
	if err := expectErrorCode(stale, "state_conflict"); err != nil {
		return err
	}

	read, err := h.request(http.MethodGet, "/cases/"+caseID, nil)
	if err != nil {
		return err
	}
	if err := expectStatus(read, http.StatusOK); err != nil {
		return err
	}
	if err := expectVersion(read, 2); err != nil {
		return err
	}
	state, err := nestedString(read.Body, "case", "state")
	if err != nil {
		return err
	}
	if state != "review_ready" {
		return fmt.Errorf("case state after stale submission = %q", state)
	}
	measurements, err := nestedSlice(read.Body, "measurements")
	if err != nil {
		return err
	}
	if len(measurements) != 1 {
		return fmt.Errorf("stale submission left %d measurements", len(measurements))
	}
	events, err := nestedSlice(read.Body, "events")
	if err != nil {
		return err
	}
	if len(events) != 2 {
		return fmt.Errorf("stale submission left %d audit events", len(events))
	}
	if hasEvent(read.Body, "measurement_rejected") {
		return errors.New("stale submission appended a measurement event")
	}
	return nil
}

// checkStaleReopenBaseline verifies that a reopen carrying an old or a future
// count is rejected without touching the case or its history.
func checkStaleReopenBaseline() error {
	h := newHarness()
	defer h.close()

	caseID, err := h.rejectedCase("MET-1502")
	if err != nil {
		return err
	}
	reopened, err := h.request(http.MethodPost, "/cases/"+caseID+"/reopen", map[string]any{
		"actor":    "Lin",
		"reason":   "measurement setup was replaced",
		"baseline": 2,
	})
	if err != nil {
		return err
	}
	if err := expectStatus(reopened, http.StatusOK); err != nil {
		return err
	}
	if err := expectVersion(reopened, 3); err != nil {
		return err
	}

	for _, baseline := range []int{2, 9} {
		stale, err := h.request(http.MethodPost, "/cases/"+caseID+"/reopen", map[string]any{
			"actor":    "Lin",
			"reason":   "stale reopen attempt",
			"baseline": baseline,
		})
		if err != nil {
			return err
		}
		if err := expectStatus(stale, http.StatusConflict); err != nil {
			return err
		}
		if err := expectErrorCode(stale, "state_conflict"); err != nil {
			return err
		}
	}

	read, err := h.request(http.MethodGet, "/cases/"+caseID, nil)
	if err != nil {
		return err
	}
	if err := expectVersion(read, 3); err != nil {
		return err
	}
	state, err := nestedString(read.Body, "case", "state")
	if err != nil {
		return err
	}
	if state != "awaiting_measurement" {
		return fmt.Errorf("case state after stale reopen = %q", state)
	}
	measurements, err := nestedSlice(read.Body, "measurements")
	if err != nil {
		return err
	}
	if len(measurements) != 1 {
		return fmt.Errorf("stale reopen changed measurement history: %d entries", len(measurements))
	}
	events, err := nestedSlice(read.Body, "events")
	if err != nil {
		return err
	}
	reopenEvents := 0
	for _, entry := range events {
		event, ok := entry.(map[string]any)
		if ok && event["kind"] == "case_reopened" {
			reopenEvents++
		}
	}
	if reopenEvents != 1 {
		return fmt.Errorf("stale reopen left %d reopen events", reopenEvents)
	}
	return nil
}

// checkStaleSealBaseline verifies that sealing from an old page is rejected,
// only the current count seals the case, and a second seal afterwards never
// creates another evidence seal.
func checkStaleSealBaseline() error {
	h := newHarness()
	defer h.close()

	caseID, err := h.readyCase("MET-1503")
	if err != nil {
		return err
	}

	stale, err := h.request(http.MethodPost, "/cases/"+caseID+"/seal", map[string]any{
		"reviewer": "Reviewer-Old",
		"summary":  "sealed from an old page",
		"baseline": 1,
	})
	if err != nil {
		return err
	}
	if err := expectStatus(stale, http.StatusConflict); err != nil {
		return err
	}
	if err := expectErrorCode(stale, "state_conflict"); err != nil {
		return err
	}

	read, err := h.request(http.MethodGet, "/cases/"+caseID, nil)
	if err != nil {
		return err
	}
	if err := expectVersion(read, 2); err != nil {
		return err
	}
	state, err := nestedString(read.Body, "case", "state")
	if err != nil {
		return err
	}
	if state != "review_ready" {
		return fmt.Errorf("case state after stale seal = %q", state)
	}
	if _, exists := read.Body["seal"]; exists {
		return errors.New("stale seal created an evidence seal")
	}
	if hasEvent(read.Body, "case_sealed") {
		return errors.New("stale seal appended a seal event")
	}

	sealed, err := h.request(http.MethodPost, "/cases/"+caseID+"/seal", map[string]any{
		"reviewer": "Reviewer-Current",
		"summary":  "sealed from the current page",
		"baseline": 2,
	})
	if err != nil {
		return err
	}
	if err := expectStatus(sealed, http.StatusCreated); err != nil {
		return err
	}
	if err := expectVersion(sealed, 3); err != nil {
		return err
	}

	after, err := h.request(http.MethodPost, "/cases/"+caseID+"/seal", map[string]any{
		"reviewer": "Reviewer-Late",
		"summary":  "sealed again from an old page",
		"baseline": 2,
	})
	if err != nil {
		return err
	}
	if err := expectStatus(after, http.StatusConflict); err != nil {
		return err
	}

	final, err := h.request(http.MethodGet, "/cases/"+caseID, nil)
	if err != nil {
		return err
	}
	sealID, err := nestedString(final.Body, "seal", "id")
	if err != nil {
		return err
	}
	if sealID == "" {
		return errors.New("evidence seal is missing")
	}
	reviewer, err := nestedString(final.Body, "seal", "reviewer")
	if err != nil {
		return err
	}
	if reviewer != "Reviewer-Current" {
		return fmt.Errorf("seal reviewer = %q, expected the current-page reviewer", reviewer)
	}
	return nil
}

// checkConcurrentBaselineSeal races two seals carrying the same observed
// count. Exactly one may commit; the other must be rejected.
func checkConcurrentBaselineSeal() error {
	h := newHarness()
	defer h.close()

	caseID, err := h.readyCase("MET-1504")
	if err != nil {
		return err
	}
	successes, conflicts, err := h.race(2, func(index int) (apiResponse, error) {
		return h.request(http.MethodPost, "/cases/"+caseID+"/seal", map[string]any{
			"reviewer": fmt.Sprintf("Reviewer-%d", index+1),
			"summary":  "concurrent seal attempt",
			"baseline": 2,
		})
	})
	if err != nil {
		return err
	}
	if successes != 1 || conflicts != 1 {
		return fmt.Errorf("concurrent seal outcomes: %d success, %d conflict", successes, conflicts)
	}

	read, err := h.request(http.MethodGet, "/cases/"+caseID, nil)
	if err != nil {
		return err
	}
	if err := expectVersion(read, 3); err != nil {
		return err
	}
	state, err := nestedString(read.Body, "case", "state")
	if err != nil {
		return err
	}
	if state != "sealed" {
		return fmt.Errorf("concurrent seal left case state = %q", state)
	}
	if _, err := nestedString(read.Body, "seal", "id"); err != nil {
		return fmt.Errorf("evidence seal is missing: %w", err)
	}
	return nil
}

// checkConcurrentBaselineMeasurement races two technicians that both read the
// same initial count. Only one measurement may be recorded.
func checkConcurrentBaselineMeasurement() error {
	h := newHarness()
	defer h.close()

	deviceID, err := h.createDevice("MET-1505")
	if err != nil {
		return err
	}
	opened, err := h.openCase(deviceID)
	if err != nil {
		return err
	}
	caseID, err := nestedString(opened.Body, "case", "id")
	if err != nil {
		return err
	}
	successes, conflicts, err := h.race(2, func(index int) (apiResponse, error) {
		return h.request(http.MethodPost, "/cases/"+caseID+"/measurements", map[string]any{
			"observed":   25.2 + 0.1*float64(index),
			"technician": fmt.Sprintf("Tech-%d", index+1),
			"baseline":   1,
		})
	})
	if err != nil {
		return err
	}
	if successes != 1 || conflicts != 1 {
		return fmt.Errorf("concurrent measurement outcomes: %d success, %d conflict", successes, conflicts)
	}

	read, err := h.request(http.MethodGet, "/cases/"+caseID, nil)
	if err != nil {
		return err
	}
	if err := expectVersion(read, 2); err != nil {
		return err
	}
	measurements, err := nestedSlice(read.Body, "measurements")
	if err != nil {
		return err
	}
	if len(measurements) != 1 {
		return fmt.Errorf("concurrent measurements left %d entries", len(measurements))
	}
	events, err := nestedSlice(read.Body, "events")
	if err != nil {
		return err
	}
	measurementEvents := 0
	for _, entry := range events {
		event, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		if kind, _ := event["kind"].(string); kind == "measurement_accepted" || kind == "measurement_rejected" {
			measurementEvents++
		}
	}
	if measurementEvents != 1 {
		return fmt.Errorf("concurrent measurements left %d measurement events", measurementEvents)
	}
	return nil
}

func newHarness() *harness {
	server := httptest.NewServer(bootstrap.NewHandler())
	return &harness{
		baseURL: server.URL,
		client:  server.Client(),
		server:  server,
	}
}

func (h *harness) close() {
	h.server.Close()
}

func (h *harness) request(method, path string, body any) (apiResponse, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return apiResponse{}, err
		}
		reader = bytes.NewReader(encoded)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, method, h.baseURL+path, reader)
	if err != nil {
		return apiResponse{}, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := h.client.Do(request)
	if err != nil {
		return apiResponse{}, err
	}
	defer response.Body.Close()
	rawBytes, err := io.ReadAll(response.Body)
	if err != nil {
		return apiResponse{}, err
	}
	result := apiResponse{Status: response.StatusCode, Raw: string(rawBytes)}
	if len(bytes.TrimSpace(rawBytes)) > 0 {
		if err := json.Unmarshal(rawBytes, &result.Body); err != nil {
			return apiResponse{}, fmt.Errorf("decode %s %s response: %w", method, path, err)
		}
	}
	return result, nil
}

func (h *harness) createDevice(tag string) (string, error) {
	response, err := h.request(http.MethodPost, "/devices", map[string]any{
		"asset_tag": tag,
		"model":     "Bench Meter",
		"room":      "Room A",
		"steward":   "Lin",
	})
	if err != nil {
		return "", err
	}
	if err := expectStatus(response, http.StatusCreated); err != nil {
		return "", err
	}
	return nestedString(response.Body, "device", "id")
}

func (h *harness) openCase(deviceID string) (apiResponse, error) {
	response, err := h.request(http.MethodPost, "/cases", map[string]any{
		"device_id": deviceID,
		"nominal":   25.0,
		"tolerance": 0.5,
		"steward":   "Qiao",
	})
	if err != nil {
		return apiResponse{}, err
	}
	if err := expectStatus(response, http.StatusCreated); err != nil {
		return apiResponse{}, err
	}
	return response, nil
}

func (h *harness) recordMeasurement(caseID string, observed float64, technician string) (apiResponse, error) {
	return h.request(http.MethodPost, "/cases/"+caseID+"/measurements", map[string]any{
		"observed":   observed,
		"technician": technician,
	})
}

func (h *harness) rejectedCase(tag string) (string, error) {
	deviceID, err := h.createDevice(tag)
	if err != nil {
		return "", err
	}
	opened, err := h.openCase(deviceID)
	if err != nil {
		return "", err
	}
	caseID, err := nestedString(opened.Body, "case", "id")
	if err != nil {
		return "", err
	}
	response, err := h.recordMeasurement(caseID, 26.5, "Rui")
	if err != nil {
		return "", err
	}
	if err := expectStatus(response, http.StatusCreated); err != nil {
		return "", err
	}
	return caseID, nil
}

func (h *harness) readyCase(tag string) (string, error) {
	deviceID, err := h.createDevice(tag)
	if err != nil {
		return "", err
	}
	opened, err := h.openCase(deviceID)
	if err != nil {
		return "", err
	}
	caseID, err := nestedString(opened.Body, "case", "id")
	if err != nil {
		return "", err
	}
	response, err := h.recordMeasurement(caseID, 25.2, "Rui")
	if err != nil {
		return "", err
	}
	if err := expectStatus(response, http.StatusCreated); err != nil {
		return "", err
	}
	return caseID, nil
}

func expectStatus(response apiResponse, expected int) error {
	if response.Status != expected {
		return fmt.Errorf("status = %d, expected %d: %s", response.Status, expected, response.Raw)
	}
	return nil
}

func expectVersion(response apiResponse, expected int) error {
	value, err := nestedValue(response.Body, "case", "version")
	if err != nil {
		return err
	}
	actual, ok := value.(float64)
	if !ok {
		return fmt.Errorf("case.version is not a number: %v", value)
	}
	if int(actual) != expected {
		return fmt.Errorf("case.version = %d, expected %d", int(actual), expected)
	}
	return nil
}

// race fires the same request concurrently and counts committed requests
// (201 Created) against rejected ones (409 Conflict).
func (h *harness) race(attempts int, fire func(index int) (apiResponse, error)) (successes int, conflicts int, err error) {
	type outcome struct {
		response apiResponse
		err      error
	}
	results := make(chan outcome, attempts)
	var wait sync.WaitGroup
	for index := 0; index < attempts; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			response, err := fire(index)
			results <- outcome{response: response, err: err}
		}(index)
	}
	wait.Wait()
	close(results)

	for item := range results {
		if item.err != nil {
			return 0, 0, item.err
		}
		switch item.response.Status {
		case http.StatusCreated:
			successes++
		case http.StatusConflict:
			conflicts++
		default:
			return 0, 0, fmt.Errorf("unexpected concurrent status %d: %s", item.response.Status, item.response.Raw)
		}
	}
	return successes, conflicts, nil
}

func expectErrorCode(response apiResponse, expected string) error {
	code, err := nestedString(response.Body, "error", "code")
	if err != nil {
		return err
	}
	if code != expected {
		return fmt.Errorf("error code = %q, expected %q", code, expected)
	}
	return nil
}

func nestedString(root map[string]any, path ...string) (string, error) {
	value, err := nestedValue(root, path...)
	if err != nil {
		return "", err
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s is not a string", strings.Join(path, "."))
	}
	return text, nil
}

func nestedBool(root map[string]any, path ...any) (bool, error) {
	value, err := nestedAny(root, path...)
	if err != nil {
		return false, err
	}
	flag, ok := value.(bool)
	if !ok {
		return false, fmt.Errorf("%v is not a boolean", path)
	}
	return flag, nil
}

func nestedSlice(root map[string]any, key string) ([]any, error) {
	value, ok := root[key]
	if !ok {
		return nil, fmt.Errorf("missing %s", key)
	}
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%s is not an array", key)
	}
	return items, nil
}

func hasEvent(root map[string]any, kind string) bool {
	items, err := nestedSlice(root, "events")
	if err != nil {
		return false
	}
	for _, item := range items {
		event, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if event["kind"] == kind {
			return true
		}
	}
	return false
}

func nestedValue(root map[string]any, path ...string) (any, error) {
	parts := make([]any, 0, len(path))
	for _, part := range path {
		parts = append(parts, part)
	}
	return nestedAny(root, parts...)
}

func nestedAny(root map[string]any, path ...any) (any, error) {
	var current any = root
	for _, part := range path {
		switch typed := part.(type) {
		case string:
			object, ok := current.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s is not an object", typed)
			}
			value, ok := object[typed]
			if !ok {
				return nil, fmt.Errorf("missing %s", typed)
			}
			current = value
		case int:
			items, ok := current.([]any)
			if !ok {
				return nil, fmt.Errorf("index %d is not inside an array", typed)
			}
			if typed < 0 || typed >= len(items) {
				return nil, fmt.Errorf("index %d is out of range", typed)
			}
			current = items[typed]
		default:
			return nil, fmt.Errorf("unsupported path element %T", part)
		}
	}
	return current, nil
}
