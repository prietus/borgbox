package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Schedule describes a recurring borg maintenance job for a single repo.
// v0.4.0 only supports kinds that never need a passphrase: "check" (run
// as `borg check --repository-only`, which verifies segment integrity
// without touching encrypted content) and "compact".
type Schedule struct {
	ID         string `json:"id"`
	Repo       string `json:"repo"`
	Kind       string `json:"kind"`
	Cadence    string `json:"cadence"`
	Hour       int    `json:"hour"`
	Minute     int    `json:"minute"`
	Weekday    int    `json:"weekday"`
	Day        int    `json:"day"`
	Enabled    bool   `json:"enabled"`
	CreatedAt  string `json:"created_at"`
	LastRun    string `json:"last_run"`
	LastStatus string `json:"last_status"`
	LastJobID  string `json:"last_job_id"`
	NextRun    string `json:"next_run"`
}

type schedulePatch struct {
	Kind    *string `json:"kind"`
	Cadence *string `json:"cadence"`
	Hour    *int    `json:"hour"`
	Minute  *int    `json:"minute"`
	Weekday *int    `json:"weekday"`
	Day     *int    `json:"day"`
	Enabled *bool   `json:"enabled"`
}

type schedulesFile struct {
	Schedules []*Schedule `json:"schedules"`
}

type Scheduler struct {
	mu        sync.Mutex
	path      string
	repoRoot  string
	schedules map[string]*Schedule
	jm        *jobManager
}

func newScheduler(path, repoRoot string, jm *jobManager) (*Scheduler, error) {
	s := &Scheduler{
		path:      path,
		repoRoot:  repoRoot,
		schedules: map[string]*Schedule{},
		jm:        jm,
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Scheduler) load() error {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var f schedulesFile
	if err := json.Unmarshal(b, &f); err != nil {
		return fmt.Errorf("parse %s: %w", s.path, err)
	}
	now := time.Now()
	for _, sch := range f.Schedules {
		if sch.NextRun == "" {
			sch.NextRun = computeNextRun(*sch, now).UTC().Format(time.RFC3339)
		}
		s.schedules[sch.ID] = sch
	}
	return nil
}

func (s *Scheduler) saveLocked() error {
	list := make([]*Schedule, 0, len(s.schedules))
	for _, sch := range s.schedules {
		list = append(list, sch)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].CreatedAt < list[j].CreatedAt })
	data, err := json.MarshalIndent(schedulesFile{Schedules: list}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Scheduler) list() []*Schedule {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Schedule, 0, len(s.schedules))
	for _, sch := range s.schedules {
		clone := *sch
		out = append(out, &clone)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt < out[j].CreatedAt })
	return out
}

func (s *Scheduler) listByRepo(repo string) []*Schedule {
	all := s.list()
	out := make([]*Schedule, 0, len(all))
	for _, sch := range all {
		if sch.Repo == repo {
			out = append(out, sch)
		}
	}
	return out
}

func (s *Scheduler) get(id string) (*Schedule, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sch, ok := s.schedules[id]
	if !ok {
		return nil, false
	}
	clone := *sch
	return &clone, true
}

func validateSchedule(sch *Schedule) error {
	switch sch.Kind {
	case "check", "compact":
	default:
		return errors.New("kind must be check or compact")
	}
	switch sch.Cadence {
	case "daily":
	case "weekly":
		if sch.Weekday < 0 || sch.Weekday > 6 {
			return errors.New("weekday must be 0..6 for weekly")
		}
	case "monthly":
		if sch.Day < 1 || sch.Day > 28 {
			return errors.New("day must be 1..28 for monthly")
		}
	default:
		return errors.New("cadence must be daily, weekly or monthly")
	}
	if sch.Hour < 0 || sch.Hour > 23 {
		return errors.New("hour must be 0..23")
	}
	if sch.Minute < 0 || sch.Minute > 59 {
		return errors.New("minute must be 0..59")
	}
	return nil
}

func (s *Scheduler) create(in Schedule) (*Schedule, error) {
	if err := validateSchedule(&in); err != nil {
		return nil, err
	}
	now := time.Now()
	in.ID = newScheduleID()
	in.CreatedAt = now.UTC().Format(time.RFC3339)
	in.LastRun = ""
	in.LastStatus = ""
	in.LastJobID = ""
	in.NextRun = computeNextRun(in, now).UTC().Format(time.RFC3339)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.schedules[in.ID] = &in
	if err := s.saveLocked(); err != nil {
		delete(s.schedules, in.ID)
		return nil, err
	}
	clone := in
	return &clone, nil
}

var errScheduleNotFound = errors.New("schedule not found")

func (s *Scheduler) update(id string, patch schedulePatch) (*Schedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sch, ok := s.schedules[id]
	if !ok {
		return nil, errScheduleNotFound
	}
	updated := *sch
	if patch.Kind != nil {
		updated.Kind = *patch.Kind
	}
	if patch.Cadence != nil {
		updated.Cadence = *patch.Cadence
	}
	if patch.Hour != nil {
		updated.Hour = *patch.Hour
	}
	if patch.Minute != nil {
		updated.Minute = *patch.Minute
	}
	if patch.Weekday != nil {
		updated.Weekday = *patch.Weekday
	}
	if patch.Day != nil {
		updated.Day = *patch.Day
	}
	if patch.Enabled != nil {
		updated.Enabled = *patch.Enabled
	}
	if err := validateSchedule(&updated); err != nil {
		return nil, err
	}
	if patch.Cadence != nil || patch.Hour != nil || patch.Minute != nil || patch.Weekday != nil || patch.Day != nil {
		updated.NextRun = computeNextRun(updated, time.Now()).UTC().Format(time.RFC3339)
	}
	*sch = updated
	if err := s.saveLocked(); err != nil {
		return nil, err
	}
	clone := *sch
	return &clone, nil
}

func (s *Scheduler) delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.schedules[id]; !ok {
		return false
	}
	delete(s.schedules, id)
	_ = s.saveLocked()
	return true
}

// start runs the tick loop forever. Call in a goroutine on server startup.
// On startup we fire a single catchup pass — if the daemon was down when
// a schedule was due, the missed run fires once (not N times for N missed
// days).
func (s *Scheduler) start() {
	s.tick(time.Now())
	t := time.NewTicker(60 * time.Second)
	defer t.Stop()
	for now := range t.C {
		s.tick(now)
	}
}

func (s *Scheduler) tick(now time.Time) {
	s.mu.Lock()
	due := make([]string, 0)
	for id, sch := range s.schedules {
		if !sch.Enabled {
			continue
		}
		next, err := time.Parse(time.RFC3339, sch.NextRun)
		if err != nil {
			next = computeNextRun(*sch, now)
			sch.NextRun = next.UTC().Format(time.RFC3339)
		}
		if !now.Before(next) {
			due = append(due, id)
		}
	}
	s.mu.Unlock()
	for _, id := range due {
		s.fire(id, now)
	}
}

func (s *Scheduler) fire(id string, now time.Time) {
	s.mu.Lock()
	sch, ok := s.schedules[id]
	if !ok {
		s.mu.Unlock()
		return
	}
	snap := *sch
	s.mu.Unlock()

	if s.isRepoBusy(snap.Repo) {
		// A job is already running for this repo — retry next tick.
		// Do not advance next_run so the next tick re-evaluates.
		log.Printf("scheduler: %s skipped, repo %s busy", snap.ID, snap.Repo)
		return
	}

	repoPath := filepath.Join(s.repoRoot, snap.Repo)
	if !fileExists(filepath.Join(repoPath, "config")) {
		s.markStarted(snap.ID, "", now)
		s.markFinished(snap.ID, "skipped")
		return
	}

	var cmd *exec.Cmd
	switch snap.Kind {
	case "check":
		cmd = exec.Command("borg", "check", "--info", "--repository-only", repoPath)
	case "compact":
		cmd = exec.Command("borg", "compact", "--info", repoPath)
	default:
		return
	}
	cmd.Env = borgEnv("")

	j := s.jm.create(snap.Repo, snap.Kind, false)
	s.markStarted(snap.ID, j.ID, now)
	log.Printf("scheduler: fire %s kind=%s repo=%s job=%s", snap.ID, snap.Kind, snap.Repo, j.ID)
	go func(schID, jobID string) {
		s.jm.run(jobID, cmd)
		if fin, ok := s.jm.get(jobID); ok {
			s.markFinished(schID, fin.Status)
		}
	}(snap.ID, j.ID)
}

func (s *Scheduler) isRepoBusy(repo string) bool {
	for _, j := range s.jm.list() {
		if j.Repo == repo && (j.Status == "queued" || j.Status == "running") {
			return true
		}
	}
	return false
}

func (s *Scheduler) markStarted(id, jobID string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sch, ok := s.schedules[id]
	if !ok {
		return
	}
	sch.LastRun = now.UTC().Format(time.RFC3339)
	if jobID != "" {
		sch.LastJobID = jobID
	}
	sch.LastStatus = "running"
	sch.NextRun = computeNextRun(*sch, now).UTC().Format(time.RFC3339)
	_ = s.saveLocked()
}

func (s *Scheduler) markFinished(id, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sch, ok := s.schedules[id]
	if !ok {
		return
	}
	sch.LastStatus = status
	_ = s.saveLocked()
}

// computeNextRun returns the next occurrence strictly after `from`,
// interpreted in the server's local timezone (cron convention). The
// returned time is still in UTC — callers that want to store it should
// format it as RFC3339.
func computeNextRun(sch Schedule, from time.Time) time.Time {
	loc := time.Local
	from = from.In(loc)
	base := time.Date(from.Year(), from.Month(), from.Day(), sch.Hour, sch.Minute, 0, 0, loc)
	switch sch.Cadence {
	case "daily":
		if !base.After(from) {
			base = base.AddDate(0, 0, 1)
		}
		return base
	case "weekly":
		diff := (sch.Weekday - int(base.Weekday()) + 7) % 7
		base = base.AddDate(0, 0, diff)
		if !base.After(from) {
			base = base.AddDate(0, 0, 7)
		}
		return base
	case "monthly":
		base = time.Date(from.Year(), from.Month(), sch.Day, sch.Hour, sch.Minute, 0, 0, loc)
		if !base.After(from) {
			base = base.AddDate(0, 1, 0)
		}
		return base
	}
	return from.Add(24 * time.Hour)
}

func newScheduleID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "sch_" + hex.EncodeToString(b[:])
}

// ---- HTTP handlers ----

func (s *Server) listSchedules(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.scheduler.list())
}

func (s *Server) listRepoSchedules(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !nameRe.MatchString(name) {
		writeErr(w, http.StatusBadRequest, "invalid name")
		return
	}
	writeJSON(w, http.StatusOK, s.scheduler.listByRepo(name))
}

func (s *Server) createSchedule(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !nameRe.MatchString(name) {
		writeErr(w, http.StatusBadRequest, "invalid name")
		return
	}
	if !fileExists(filepath.Join(s.cfg.RepoRoot, name)) {
		writeErr(w, http.StatusNotFound, "repo not found")
		return
	}
	var in Schedule
	if err := json.NewDecoder(io.LimitReader(r.Body, 16*1024)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	in.Repo = name
	sch, err := s.scheduler.create(in)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, sch)
}

func (s *Server) patchSchedule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var p schedulePatch
	if err := json.NewDecoder(io.LimitReader(r.Body, 16*1024)).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	sch, err := s.scheduler.update(id, p)
	if err != nil {
		if errors.Is(err, errScheduleNotFound) {
			writeErr(w, http.StatusNotFound, err.Error())
			return
		}
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, sch)
}

func (s *Server) deleteSchedule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.scheduler.delete(id) {
		writeErr(w, http.StatusNotFound, "schedule not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getSchedule(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sch, ok := s.scheduler.get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "schedule not found")
		return
	}
	writeJSON(w, http.StatusOK, sch)
}
