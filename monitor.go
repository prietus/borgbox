package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Alert defines a stale-backup watcher for a single repo. The monitor ticks
// every 5 minutes; when a repo's most recent data/** mtime is older than
// StaleAfterHours the alert fires via HTTP POST to WebhookURL. Transitions
// are edge-triggered (ok→stale, stale→ok). Set RenotifyHours > 0 to also
// re-fire on stale→stale after that many hours have elapsed since the last
// delivery.
//
// LastWrite is derived from filesystem mtimes so the monitor works on
// encrypted repos without ever seeing a passphrase.
//
// Secret is write-only from the API's perspective: create/patch accept it,
// but MarshalJSON strips it and reports has_secret instead. Disk persistence
// goes through persistedAlert to preserve the real value across restarts.
type Alert struct {
	ID              string  `json:"id"`
	Repo            string  `json:"repo"`
	StaleAfterHours int     `json:"stale_after_hours"`
	WebhookURL      string  `json:"webhook_url"`
	Enabled         bool    `json:"enabled"`
	RenotifyHours   int     `json:"renotify_hours"`
	Secret          string  `json:"secret,omitempty"`
	CreatedAt       string  `json:"created_at"`
	LastCheckAt     string  `json:"last_check_at"`
	LastState       *string `json:"last_state"`
	LastWrite       string  `json:"last_write"`
	LastAlertedAt   string  `json:"last_alerted_at"`
	LastError       string  `json:"last_error"`
}

// MarshalJSON strips Secret from API responses and exposes has_secret instead.
// Disk persistence bypasses this via persistedAlert so the secret round-trips
// across daemon restarts.
func (a Alert) MarshalJSON() ([]byte, error) {
	hasSecret := a.Secret != ""
	a.Secret = ""
	type shadow Alert
	return json.Marshal(struct {
		shadow
		HasSecret bool `json:"has_secret"`
	}{shadow: shadow(a), HasSecret: hasSecret})
}

// persistedAlert is Alert without the MarshalJSON override, used only when
// writing alerts.json to disk. Secret must round-trip.
type persistedAlert Alert

type alertPatch struct {
	StaleAfterHours *int    `json:"stale_after_hours"`
	WebhookURL      *string `json:"webhook_url"`
	Enabled         *bool   `json:"enabled"`
	RenotifyHours   *int    `json:"renotify_hours"`
	// Secret semantics: nil = leave unchanged, "" = clear, non-empty = set.
	Secret *string `json:"secret"`
}

type alertsFile struct {
	Alerts []*Alert `json:"alerts"`
}

type webhookPayload struct {
	Event               string `json:"event"`
	Repo                string `json:"repo"`
	Hostname            string `json:"hostname"`
	LastWrite           string `json:"last_write"`
	StaleAfterHours     int    `json:"stale_after_hours"`
	HoursSinceLastWrite int64  `json:"hours_since_last_write"`
}

type Monitor struct {
	mu       sync.Mutex
	path     string
	repoRoot string
	hostname string
	alerts   map[string]*Alert
	client   *http.Client
}

func newMonitor(path, repoRoot string) (*Monitor, error) {
	host, _ := os.Hostname()
	m := &Monitor{
		path:     path,
		repoRoot: repoRoot,
		hostname: host,
		alerts:   map[string]*Alert{},
		client:   &http.Client{Timeout: 10 * time.Second},
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Monitor) load() error {
	b, err := os.ReadFile(m.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var f alertsFile
	if err := json.Unmarshal(b, &f); err != nil {
		return fmt.Errorf("parse %s: %w", m.path, err)
	}
	for _, a := range f.Alerts {
		m.alerts[a.ID] = a
	}
	return nil
}

func (m *Monitor) saveLocked() error {
	// Use persistedAlert (no MarshalJSON override) so the secret is written to
	// disk. The API-facing MarshalJSON strips it, which is exactly what we
	// want for HTTP responses but not for persistence.
	list := make([]*persistedAlert, 0, len(m.alerts))
	for _, a := range m.alerts {
		pa := persistedAlert(*a)
		list = append(list, &pa)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt < list[j].CreatedAt })
	type diskFile struct {
		Alerts []*persistedAlert `json:"alerts"`
	}
	data, err := json.MarshalIndent(diskFile{Alerts: list}, "", "  ")
	if err != nil {
		return err
	}
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.path)
}

func (m *Monitor) list() []*Alert {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Alert, 0, len(m.alerts))
	for _, a := range m.alerts {
		clone := *a
		out = append(out, &clone)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
}

func (m *Monitor) listByRepo(repo string) []*Alert {
	all := m.list()
	out := make([]*Alert, 0, len(all))
	for _, a := range all {
		if a.Repo == repo {
			out = append(out, a)
		}
	}
	return out
}

func (m *Monitor) get(id string) (*Alert, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.alerts[id]
	if !ok {
		return nil, false
	}
	clone := *a
	return &clone, true
}

func validateAlert(a *Alert) error {
	if a.StaleAfterHours <= 0 {
		return errors.New("stale_after_hours must be > 0")
	}
	if a.RenotifyHours < 0 {
		return errors.New("renotify_hours must be >= 0")
	}
	u, err := url.Parse(a.WebhookURL)
	if err != nil || u.Host == "" {
		return errors.New("webhook_url must be an absolute http(s) URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("webhook_url scheme must be http or https")
	}
	return nil
}

func (m *Monitor) create(in Alert) (*Alert, error) {
	if err := validateAlert(&in); err != nil {
		return nil, err
	}
	in.ID = newAlertID()
	in.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	in.LastCheckAt = ""
	in.LastState = nil
	in.LastWrite = ""
	in.LastAlertedAt = ""
	in.LastError = ""
	m.mu.Lock()
	defer m.mu.Unlock()
	m.alerts[in.ID] = &in
	if err := m.saveLocked(); err != nil {
		delete(m.alerts, in.ID)
		return nil, err
	}
	clone := in
	return &clone, nil
}

var errAlertNotFound = errors.New("alert not found")

func (m *Monitor) update(id string, patch alertPatch) (*Alert, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.alerts[id]
	if !ok {
		return nil, errAlertNotFound
	}
	updated := *a
	if patch.StaleAfterHours != nil {
		updated.StaleAfterHours = *patch.StaleAfterHours
	}
	if patch.WebhookURL != nil {
		updated.WebhookURL = *patch.WebhookURL
	}
	if patch.Enabled != nil {
		updated.Enabled = *patch.Enabled
	}
	if patch.RenotifyHours != nil {
		updated.RenotifyHours = *patch.RenotifyHours
	}
	if patch.Secret != nil {
		// Empty string clears the secret; non-empty replaces it.
		updated.Secret = *patch.Secret
	}
	if err := validateAlert(&updated); err != nil {
		return nil, err
	}
	*a = updated
	if err := m.saveLocked(); err != nil {
		return nil, err
	}
	clone := *a
	return &clone, nil
}

func (m *Monitor) delete(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.alerts[id]; !ok {
		return false
	}
	delete(m.alerts, id)
	_ = m.saveLocked()
	return true
}

// deleteByRepo drops every alert attached to the given repo. Called from the
// repo delete handler so alerts don't outlive their target as orphaned rows
// that will fail every tick with "repo not found" in the log.
func (m *Monitor) deleteByRepo(repo string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	for id, a := range m.alerts {
		if a.Repo == repo {
			ids = append(ids, id)
		}
	}
	for _, id := range ids {
		delete(m.alerts, id)
	}
	if len(ids) > 0 {
		_ = m.saveLocked()
	}
	return len(ids)
}

func newAlertID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "alert_" + hex.EncodeToString(b[:])
}

// start runs the evaluation loop forever. Call in a goroutine on server
// startup. On startup we fire a single catchup pass so that any transition
// that happened while the daemon was down is delivered once.
func (m *Monitor) start() {
	m.evaluate(time.Now())
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for now := range t.C {
		m.evaluate(now)
	}
}

func (m *Monitor) evaluate(now time.Time) {
	m.mu.Lock()
	snaps := make([]Alert, 0, len(m.alerts))
	for _, a := range m.alerts {
		if !a.Enabled {
			continue
		}
		snaps = append(snaps, *a)
	}
	m.mu.Unlock()
	for _, snap := range snaps {
		m.check(snap, now)
	}
}

func (m *Monitor) check(snap Alert, now time.Time) {
	repoPath := filepath.Join(m.repoRoot, snap.Repo)
	checkAt := now.UTC().Format(time.RFC3339)

	if !fileExists(filepath.Join(repoPath, "config")) {
		m.mu.Lock()
		if a, ok := m.alerts[snap.ID]; ok {
			a.LastCheckAt = checkAt
			_ = m.saveLocked()
		}
		m.mu.Unlock()
		return
	}

	_, lastWriteStr := scanDataDir(filepath.Join(repoPath, "data"))
	var lastWrite time.Time
	if lastWriteStr != "" {
		lastWrite, _ = time.Parse(time.RFC3339, lastWriteStr)
	}

	newState := "ok"
	var hoursSince int64
	if !lastWrite.IsZero() {
		hoursSince = int64(now.Sub(lastWrite).Hours())
		if hoursSince >= int64(snap.StaleAfterHours) {
			newState = "stale"
		}
	}

	prevState := ""
	if snap.LastState != nil {
		prevState = *snap.LastState
	}

	fire := false
	event := ""
	switch {
	case newState == "stale" && prevState != "stale":
		fire, event = true, "stale"
	case newState == "ok" && prevState == "stale":
		fire, event = true, "recovered"
	case newState == "stale" && prevState == "stale" && snap.RenotifyHours > 0 && snap.LastAlertedAt != "":
		if la, err := time.Parse(time.RFC3339, snap.LastAlertedAt); err == nil {
			if now.Sub(la).Hours() >= float64(snap.RenotifyHours) {
				fire, event = true, "stale"
			}
		}
	}

	var deliveryErr string
	if fire {
		payload := webhookPayload{
			Event:               event,
			Repo:                snap.Repo,
			Hostname:            m.hostname,
			LastWrite:           lastWriteStr,
			StaleAfterHours:     snap.StaleAfterHours,
			HoursSinceLastWrite: hoursSince,
		}
		if err := m.deliverWithRetry(snap.WebhookURL, snap.Secret, payload); err != nil {
			deliveryErr = err.Error()
			log.Printf("monitor: deliver %s failed: %v", snap.ID, err)
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.alerts[snap.ID]
	if !ok {
		return
	}
	a.LastCheckAt = checkAt
	ns := newState
	a.LastState = &ns
	a.LastWrite = lastWriteStr
	if fire {
		a.LastAlertedAt = checkAt
		a.LastError = deliveryErr
	} else {
		a.LastError = ""
	}
	_ = m.saveLocked()
}

// httpError is returned by deliver() when the server answered with a non-2xx
// status. Callers use errors.As to decide whether the failure is retryable.
// Transport errors (DNS, connection refused, timeouts) never produce this
// type — they're returned as the underlying error and always count as
// retryable by isRetryable.
type httpError struct {
	statusCode int
}

func (e *httpError) Error() string {
	return fmt.Sprintf("webhook returned %d", e.statusCode)
}

// isRetryable decides whether a delivery error is worth trying again. 5xx,
// 408 (request timeout) and 429 (rate limit) are retryable; every other 4xx
// means the webhook is misconfigured and repeating won't help. Transport
// errors (nil httpError) are always retryable.
func isRetryable(err error) bool {
	var he *httpError
	if errors.As(err, &he) {
		return he.statusCode == 408 || he.statusCode == 429 || he.statusCode >= 500
	}
	return err != nil
}

// deliver sends the webhook once. If secret is non-empty, the body is signed
// and a `X-BorgBox-Signature: sha256=<hex>` header is attached so the receiver
// can authenticate the call (HMAC-SHA256 over the raw JSON body).
func (m *Monitor) deliver(webhookURL, secret string, payload webhookPayload) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", webhookURL, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "borgbox/"+version)
	if secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(b)
		req.Header.Set("X-BorgBox-Signature", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode >= 300 {
		return &httpError{statusCode: resp.StatusCode}
	}
	return nil
}

// deliverWithRetry runs up to 3 attempts with 2s/10s back-off between them,
// but only for retryable failures. Used by the scheduled stale→ok/stale→stale
// flow. The testAlert handler uses deliver() directly so the user gets a
// single, fast yes/no in the UI.
func (m *Monitor) deliverWithRetry(webhookURL, secret string, payload webhookPayload) error {
	backoffs := []time.Duration{2 * time.Second, 10 * time.Second}
	var lastErr error
	for i := 0; i <= len(backoffs); i++ {
		err := m.deliver(webhookURL, secret, payload)
		if err == nil {
			return nil
		}
		lastErr = err
		if !isRetryable(err) {
			return err
		}
		if i < len(backoffs) {
			time.Sleep(backoffs[i])
		}
	}
	return lastErr
}

// ---- HTTP handlers ----

func (s *Server) listAlerts(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.monitor.list())
}

func (s *Server) listRepoAlerts(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !nameRe.MatchString(name) {
		writeErr(w, http.StatusBadRequest, "invalid name")
		return
	}
	writeJSON(w, http.StatusOK, s.monitor.listByRepo(name))
}

func (s *Server) createAlert(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !nameRe.MatchString(name) {
		writeErr(w, http.StatusBadRequest, "invalid name")
		return
	}
	if !fileExists(filepath.Join(s.cfg.RepoRoot, name)) {
		writeErr(w, http.StatusNotFound, "repo not found")
		return
	}
	var in Alert
	if err := json.NewDecoder(io.LimitReader(r.Body, 16*1024)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	in.Repo = name
	a, err := s.monitor.create(in)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

func (s *Server) getAlert(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, ok := s.monitor.get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "alert not found")
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) patchAlert(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var p alertPatch
	if err := json.NewDecoder(io.LimitReader(r.Body, 16*1024)).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	a, err := s.monitor.update(id, p)
	if err != nil {
		if errors.Is(err, errAlertNotFound) {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, a)
}

func (s *Server) deleteAlert(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.monitor.delete(id) {
		writeErr(w, http.StatusNotFound, "alert not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) testAlert(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a, ok := s.monitor.get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "alert not found")
		return
	}
	payload := webhookPayload{
		Event:           "test",
		Repo:            a.Repo,
		Hostname:        s.monitor.hostname,
		StaleAfterHours: a.StaleAfterHours,
	}
	if err := s.monitor.deliver(a.WebhookURL, a.Secret, payload); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
