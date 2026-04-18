package main

import (
	"bufio"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const version = "0.5.0"

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
var jobIDRe = regexp.MustCompile(`^j_[a-f0-9]{1,32}$`)
var borgboxTagRe = regexp.MustCompile(`# borgbox:([a-z0-9][a-z0-9-]*)`)
var commandRe = regexp.MustCompile(`command="([^"]+)"`)

var errKeyExists = errors.New("repo already registered; delete first or use a different name")
var errKeyNotFound = errors.New("repo has no borgbox-managed key")

type Config struct {
	Addr          string
	RepoRoot      string
	AuthKeys      string
	Token         string
	SSHHost       string
	SSHUser       string
	SSHPort       int
	SchedulesPath string
	AlertsPath    string
}

type Server struct {
	cfg       Config
	start     time.Time
	mu        sync.Mutex
	jobs      *jobManager
	scheduler *Scheduler
	monitor   *Monitor
}

type createRepoReq struct {
	Name       string `json:"name"`
	Pubkey     string `json:"pubkey"`
	AppendOnly bool   `json:"append_only"`
}

type patchRepoReq struct {
	AppendOnly *bool `json:"append_only"`
}

type repoKeyState struct {
	Registered bool
	AppendOnly bool
}

type repoInfo struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	SSHURL      string `json:"ssh_url"`
	Registered  bool   `json:"registered"`
	AppendOnly  bool   `json:"append_only"`
	SizeBytes   int64  `json:"size_bytes"`
	Initialized bool   `json:"initialized"`
	ModifiedAt  string `json:"modified_at"`
}

type repoDetail struct {
	repoInfo
	ID         string `json:"id"`
	Encryption string `json:"encryption"`
	Segments   int    `json:"segments"`
	LastWrite  string `json:"last_write"`
	Version    string `json:"version"`
}

type infoResp struct {
	Version     string `json:"version"`
	RepoRoot    string `json:"repo_root"`
	FreeBytes   uint64 `json:"free_bytes"`
	TotalBytes  uint64 `json:"total_bytes"`
	UptimeSec   int64  `json:"uptime_sec"`
	BorgVersion string `json:"borg_version"`
	GoVersion   string `json:"go_version"`
}

type archiveInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Hostname string `json:"hostname"`
	Username string `json:"username"`
	Time     string `json:"time"`
	Comment  string `json:"comment"`
}

type storageInfo struct {
	Path       string `json:"path"`
	TotalBytes uint64 `json:"total_bytes"`
	UsedBytes  uint64 `json:"used_bytes"`
	FreeBytes  uint64 `json:"free_bytes"`
}

type systemStatsResp struct {
	Hostname       string      `json:"hostname"`
	UptimeSeconds  int64       `json:"uptime_seconds"`
	Load           [3]float64  `json:"load"`
	Storage        storageInfo `json:"storage"`
	BorgboxVersion string      `json:"borgbox_version"`
	BorgVersion    string      `json:"borg_version"`
}

type sessionInfo struct {
	PID       int    `json:"pid"`
	Repo      string `json:"repo"`
	RepoPath  string `json:"repo_path"`
	StartedAt string `json:"started_at"`
	RSSBytes  int64  `json:"rss_bytes"`
}

type jobRec struct {
	ID         string   `json:"id"`
	Repo       string   `json:"repo"`
	Kind       string   `json:"kind"`
	Status     string   `json:"status"`
	StartedAt  string   `json:"started_at"`
	FinishedAt string   `json:"finished_at"`
	ExitCode   int      `json:"exit_code"`
	DryRun     bool     `json:"dry_run"`
	LogTail    []string `json:"log_tail"`
	subs       []chan jobEvent
}

type jobEvent struct {
	Type     string `json:"type"`
	Line     string `json:"line"`
	Status   string `json:"status"`
	ExitCode int    `json:"exit_code"`
}

type checkReq struct {
	Repair     bool   `json:"repair"`
	VerifyData bool   `json:"verify_data"`
	Passphrase string `json:"passphrase"`
}

type pruneReq struct {
	KeepDaily   int    `json:"keep_daily"`
	KeepWeekly  int    `json:"keep_weekly"`
	KeepMonthly int    `json:"keep_monthly"`
	KeepYearly  int    `json:"keep_yearly"`
	DryRun      bool   `json:"dry_run"`
	Passphrase  string `json:"passphrase"`
}

type compactReq struct {
	Passphrase string `json:"passphrase"`
}

const (
	maxLogLines = 200
	maxJobs     = 200
)

type jobManager struct {
	mu   sync.Mutex
	jobs map[string]*jobRec
}

func newJobManager() *jobManager {
	return &jobManager{jobs: map[string]*jobRec{}}
}

func (jm *jobManager) create(repo, kind string, dryRun bool) *jobRec {
	jm.mu.Lock()
	defer jm.mu.Unlock()
	jm.gcLocked()
	id := fmt.Sprintf("j_%x", time.Now().UnixNano())
	j := &jobRec{
		ID:        id,
		Repo:      repo,
		Kind:      kind,
		Status:    "queued",
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		ExitCode:  -1,
		DryRun:    dryRun,
		LogTail:   []string{},
	}
	jm.jobs[id] = j
	return j
}

func (jm *jobManager) gcLocked() {
	if len(jm.jobs) < maxJobs {
		return
	}
	type ref struct {
		id   string
		when string
	}
	finished := make([]ref, 0, len(jm.jobs))
	for id, j := range jm.jobs {
		if j.Status == "done" || j.Status == "error" {
			finished = append(finished, ref{id, j.StartedAt})
		}
	}
	sort.Slice(finished, func(i, j int) bool { return finished[i].when < finished[j].when })
	for _, r := range finished {
		if len(jm.jobs) < maxJobs {
			break
		}
		delete(jm.jobs, r.id)
	}
}

func (jm *jobManager) get(id string) (*jobRec, bool) {
	jm.mu.Lock()
	defer jm.mu.Unlock()
	j, ok := jm.jobs[id]
	if !ok {
		return nil, false
	}
	clone := *j
	clone.LogTail = make([]string, len(j.LogTail))
	copy(clone.LogTail, j.LogTail)
	return &clone, true
}

func (jm *jobManager) list() []*jobRec {
	jm.mu.Lock()
	defer jm.mu.Unlock()
	out := make([]*jobRec, 0, len(jm.jobs))
	for _, j := range jm.jobs {
		clone := *j
		clone.LogTail = append([]string(nil), j.LogTail...)
		out = append(out, &clone)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt > out[j].StartedAt })
	return out
}

func (jm *jobManager) appendLog(id, line string) {
	jm.mu.Lock()
	defer jm.mu.Unlock()
	j, ok := jm.jobs[id]
	if !ok {
		return
	}
	if len(j.LogTail) >= maxLogLines {
		j.LogTail = j.LogTail[len(j.LogTail)-maxLogLines+1:]
	}
	j.LogTail = append(j.LogTail, line)
	ev := jobEvent{Type: "log", Line: line}
	for _, ch := range j.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (jm *jobManager) setStatus(id, status string, exit int) {
	jm.mu.Lock()
	defer jm.mu.Unlock()
	j, ok := jm.jobs[id]
	if !ok {
		return
	}
	j.Status = status
	if status == "done" || status == "error" {
		j.FinishedAt = time.Now().UTC().Format(time.RFC3339)
		j.ExitCode = exit
	}
	ev := jobEvent{Type: "status", Status: status, ExitCode: exit}
	for _, ch := range j.subs {
		select {
		case ch <- ev:
		default:
		}
	}
	if status == "done" || status == "error" {
		for _, ch := range j.subs {
			close(ch)
		}
		j.subs = nil
	}
}

func (jm *jobManager) subscribe(id string) (<-chan jobEvent, func(), bool) {
	jm.mu.Lock()
	defer jm.mu.Unlock()
	j, ok := jm.jobs[id]
	if !ok {
		return nil, nil, false
	}
	ch := make(chan jobEvent, maxLogLines*2)
	for _, line := range j.LogTail {
		ch <- jobEvent{Type: "log", Line: line}
	}
	if j.Status == "done" || j.Status == "error" {
		ch <- jobEvent{Type: "status", Status: j.Status, ExitCode: j.ExitCode}
		close(ch)
		return ch, func() {}, true
	}
	ch <- jobEvent{Type: "status", Status: j.Status, ExitCode: -1}
	j.subs = append(j.subs, ch)
	unsub := func() {
		jm.mu.Lock()
		defer jm.mu.Unlock()
		jj, ok := jm.jobs[id]
		if !ok {
			return
		}
		for i, c := range jj.subs {
			if c == ch {
				jj.subs = append(jj.subs[:i], jj.subs[i+1:]...)
				return
			}
		}
	}
	return ch, unsub, true
}

func (jm *jobManager) run(id string, cmd *exec.Cmd) {
	jm.setStatus(id, "running", -1)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		jm.appendLog(id, "stdout pipe: "+err.Error())
		jm.setStatus(id, "error", -2)
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		jm.appendLog(id, "stderr pipe: "+err.Error())
		jm.setStatus(id, "error", -2)
		return
	}
	if err := cmd.Start(); err != nil {
		jm.appendLog(id, "start: "+err.Error())
		jm.setStatus(id, "error", -2)
		return
	}
	var wg sync.WaitGroup
	wg.Add(2)
	scan := func(r io.Reader) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			jm.appendLog(id, sc.Text())
		}
	}
	go scan(stdout)
	go scan(stderr)
	wg.Wait()
	if err := cmd.Wait(); err != nil {
		code := -2
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		jm.setStatus(id, "error", code)
		return
	}
	jm.setStatus(id, "done", 0)
}

func main() {
	cfg := Config{}
	flag.StringVar(&cfg.Addr, "addr", env("BORGBOX_ADDR", ":9999"), "listen address")
	flag.StringVar(&cfg.RepoRoot, "repo-root", env("BORGBOX_REPO_ROOT", ""), "directory where repos live")
	flag.StringVar(&cfg.AuthKeys, "authorized-keys", env("BORGBOX_AUTH_KEYS", ""), "authorized_keys path to manage")
	flag.StringVar(&cfg.Token, "token", env("BORGBOX_TOKEN", ""), "bearer token (use @/path to read from file)")
	flag.StringVar(&cfg.SSHHost, "ssh-host", env("BORGBOX_SSH_HOST", ""), "hostname used to build ssh_url in repo responses")
	flag.StringVar(&cfg.SSHUser, "ssh-user", env("BORGBOX_SSH_USER", ""), "user used to build ssh_url (default: current user)")
	flag.StringVar(&cfg.SchedulesPath, "schedules", env("BORGBOX_SCHEDULES", ""), "path to schedules.json (default: <home>/schedules.json)")
	flag.StringVar(&cfg.AlertsPath, "alerts", env("BORGBOX_ALERTS", ""), "path to alerts.json (default: <home>/alerts.json)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("borgbox", version)
		return
	}

	if cfg.RepoRoot == "" {
		if u, err := user.Current(); err == nil {
			cfg.RepoRoot = filepath.Join(u.HomeDir, "repos")
		}
	}
	if cfg.AuthKeys == "" {
		if u, err := user.Current(); err == nil {
			cfg.AuthKeys = filepath.Join(u.HomeDir, ".ssh", "authorized_keys")
		}
	}
	if cfg.SSHHost == "" {
		cfg.SSHHost, _ = os.Hostname()
	}
	if cfg.SSHUser == "" {
		if u, err := user.Current(); err == nil {
			cfg.SSHUser = u.Username
		}
	}
	if p := env("BORGBOX_SSH_PORT", ""); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			cfg.SSHPort = n
		}
	}
	if cfg.SSHPort == 0 {
		cfg.SSHPort = 22
	}
	if cfg.SchedulesPath == "" {
		if u, err := user.Current(); err == nil {
			cfg.SchedulesPath = filepath.Join(u.HomeDir, "schedules.json")
		}
	}
	if cfg.AlertsPath == "" {
		if u, err := user.Current(); err == nil {
			cfg.AlertsPath = filepath.Join(u.HomeDir, "alerts.json")
		}
	}
	if strings.HasPrefix(cfg.Token, "@") {
		b, err := os.ReadFile(strings.TrimPrefix(cfg.Token, "@"))
		if err != nil {
			log.Fatalf("read token file: %v", err)
		}
		cfg.Token = strings.TrimSpace(string(b))
	}
	if cfg.Token == "" {
		log.Fatal("token required (set BORGBOX_TOKEN or -token)")
	}
	if err := os.MkdirAll(cfg.RepoRoot, 0o700); err != nil {
		log.Fatalf("mkdir repo root %q: %v", cfg.RepoRoot, err)
	}

	s := &Server{cfg: cfg, start: time.Now(), jobs: newJobManager()}
	sched, err := newScheduler(cfg.SchedulesPath, cfg.RepoRoot, s.jobs)
	if err != nil {
		log.Fatalf("scheduler init: %v", err)
	}
	s.scheduler = sched
	go s.scheduler.start()

	mon, err := newMonitor(cfg.AlertsPath, cfg.RepoRoot)
	if err != nil {
		log.Fatalf("monitor init: %v", err)
	}
	s.monitor = mon
	go s.monitor.start()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", s.health)
	mux.HandleFunc("GET /api/v1/info", s.auth(s.info))
	mux.HandleFunc("GET /api/v1/system/stats", s.auth(s.systemStats))
	mux.HandleFunc("GET /api/v1/sessions", s.auth(s.listSessions))
	mux.HandleFunc("GET /api/v1/repos", s.auth(s.listRepos))
	mux.HandleFunc("POST /api/v1/repos", s.auth(s.createRepo))
	mux.HandleFunc("POST /api/v1/repos/import", s.auth(s.importRepo))
	mux.HandleFunc("GET /api/v1/repos/{name}", s.auth(s.getRepo))
	mux.HandleFunc("PATCH /api/v1/repos/{name}", s.auth(s.patchRepo))
	mux.HandleFunc("DELETE /api/v1/repos/{name}", s.auth(s.deleteRepo))
	mux.HandleFunc("GET /api/v1/repos/{name}/archives", s.auth(s.listArchives))
	mux.HandleFunc("POST /api/v1/repos/{name}/break-lock", s.auth(s.breakLock))
	mux.HandleFunc("POST /api/v1/repos/{name}/check", s.auth(s.checkRepo))
	mux.HandleFunc("POST /api/v1/repos/{name}/prune", s.auth(s.pruneRepo))
	mux.HandleFunc("POST /api/v1/repos/{name}/compact", s.auth(s.compactRepo))
	mux.HandleFunc("GET /api/v1/jobs", s.auth(s.listJobs))
	mux.HandleFunc("GET /api/v1/jobs/{id}", s.auth(s.getJob))
	mux.HandleFunc("GET /api/v1/jobs/{id}/stream", s.auth(s.streamJob))
	mux.HandleFunc("GET /api/v1/schedules", s.auth(s.listSchedules))
	mux.HandleFunc("GET /api/v1/schedules/{id}", s.auth(s.getSchedule))
	mux.HandleFunc("PATCH /api/v1/schedules/{id}", s.auth(s.patchSchedule))
	mux.HandleFunc("DELETE /api/v1/schedules/{id}", s.auth(s.deleteSchedule))
	mux.HandleFunc("GET /api/v1/repos/{name}/schedules", s.auth(s.listRepoSchedules))
	mux.HandleFunc("POST /api/v1/repos/{name}/schedules", s.auth(s.createSchedule))
	mux.HandleFunc("GET /api/v1/alerts", s.auth(s.listAlerts))
	mux.HandleFunc("GET /api/v1/alerts/{id}", s.auth(s.getAlert))
	mux.HandleFunc("PATCH /api/v1/alerts/{id}", s.auth(s.patchAlert))
	mux.HandleFunc("DELETE /api/v1/alerts/{id}", s.auth(s.deleteAlert))
	mux.HandleFunc("POST /api/v1/alerts/{id}/test", s.auth(s.testAlert))
	mux.HandleFunc("GET /api/v1/repos/{name}/alerts", s.auth(s.listRepoAlerts))
	mux.HandleFunc("POST /api/v1/repos/{name}/alerts", s.auth(s.createAlert))

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           logMiddleware(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("borgbox %s listening on %s repo_root=%s", version, cfg.Addr, cfg.RepoRoot)
	log.Fatal(srv.ListenAndServe())
}

func env(k, d string) string {
	if v, ok := os.LookupEnv(k); ok {
		return v
	}
	return d
}

func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t := time.Now()
		rw := &statusRecorder{ResponseWriter: w, code: 200}
		next.ServeHTTP(rw, r)
		log.Printf("%s %s %d %s %s", r.Method, r.URL.Path, rw.code, time.Since(t), r.RemoteAddr)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(c int) {
	s.code = c
	s.ResponseWriter.WriteHeader(c)
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) auth(h http.HandlerFunc) http.HandlerFunc {
	expected := []byte("Bearer " + s.cfg.Token)
	return func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if len(got) != len(expected) || subtle.ConstantTimeCompare(got, expected) != 1 {
			writeErr(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) info(w http.ResponseWriter, r *http.Request) {
	var st syscall.Statfs_t
	free, total := uint64(0), uint64(0)
	if err := syscall.Statfs(s.cfg.RepoRoot, &st); err == nil {
		free = st.Bavail * uint64(st.Bsize)
		total = st.Blocks * uint64(st.Bsize)
	}
	writeJSON(w, http.StatusOK, infoResp{
		Version:     version,
		RepoRoot:    s.cfg.RepoRoot,
		FreeBytes:   free,
		TotalBytes:  total,
		UptimeSec:   int64(time.Since(s.start).Seconds()),
		BorgVersion: borgVersion(),
		GoVersion:   runtime.Version(),
	})
}

func (s *Server) listRepos(w http.ResponseWriter, r *http.Request) {
	entries, err := os.ReadDir(s.cfg.RepoRoot)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	states := s.scanAuthKeys()
	out := []repoInfo{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(s.cfg.RepoRoot, e.Name())
		info, _ := e.Info()
		out = append(out, s.buildRepoInfo(e.Name(), p, info, states))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) sshURL(repoPath string) string {
	if s.cfg.SSHHost == "" || s.cfg.SSHUser == "" {
		return ""
	}
	port := ""
	if s.cfg.SSHPort != 0 && s.cfg.SSHPort != 22 {
		port = fmt.Sprintf(":%d", s.cfg.SSHPort)
	}
	return fmt.Sprintf("ssh://%s@%s%s%s", s.cfg.SSHUser, s.cfg.SSHHost, port, repoPath)
}

func (s *Server) scanAuthKeys() map[string]repoKeyState {
	out := map[string]repoKeyState{}
	b, err := os.ReadFile(s.cfg.AuthKeys)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		tag := borgboxTagRe.FindStringSubmatch(line)
		if tag == nil {
			continue
		}
		st := repoKeyState{Registered: true}
		if cm := commandRe.FindStringSubmatch(line); cm != nil {
			if strings.Contains(cm[1], "--append-only") {
				st.AppendOnly = true
			}
		}
		out[tag[1]] = st
	}
	return out
}

func (s *Server) buildRepoInfo(name, path string, info os.FileInfo, states map[string]repoKeyState) repoInfo {
	st := states[name]
	ri := repoInfo{
		Name:        name,
		Path:        path,
		SSHURL:      s.sshURL(path),
		Registered:  st.Registered,
		AppendOnly:  st.AppendOnly,
		SizeBytes:   dirSize(path),
		Initialized: fileExists(filepath.Join(path, "config")),
	}
	if info != nil {
		ri.ModifiedAt = info.ModTime().UTC().Format(time.RFC3339)
	}
	return ri
}

func (s *Server) createRepo(w http.ResponseWriter, r *http.Request) {
	var req createRepoReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 16*1024)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Pubkey = strings.TrimSpace(req.Pubkey)
	if !nameRe.MatchString(req.Name) {
		writeErr(w, http.StatusBadRequest, "invalid name (lowercase alnum and dashes, max 63)")
		return
	}
	if !validPubkey(req.Pubkey) {
		writeErr(w, http.StatusBadRequest, "invalid ssh public key")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	repoPath := filepath.Join(s.cfg.RepoRoot, req.Name)
	if err := os.MkdirAll(repoPath, 0o700); err != nil {
		writeErr(w, http.StatusInternalServerError, "mkdir: "+err.Error())
		return
	}

	if err := s.addAuthorizedKey(req.Name, req.Pubkey, req.AppendOnly); err != nil {
		if errors.Is(err, errKeyExists) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, "authorized_keys: "+err.Error())
		return
	}

	st, _ := os.Stat(repoPath)
	ri := s.buildRepoInfo(req.Name, repoPath, st, map[string]repoKeyState{req.Name: {Registered: true, AppendOnly: req.AppendOnly}})
	writeJSON(w, http.StatusCreated, ri)
}

func (s *Server) importRepo(w http.ResponseWriter, r *http.Request) {
	var req createRepoReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 16*1024)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Pubkey = strings.TrimSpace(req.Pubkey)
	if !nameRe.MatchString(req.Name) {
		writeErr(w, http.StatusBadRequest, "invalid name (lowercase alnum and dashes, max 63)")
		return
	}
	if !validPubkey(req.Pubkey) {
		writeErr(w, http.StatusBadRequest, "invalid ssh public key")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	repoPath := filepath.Join(s.cfg.RepoRoot, req.Name)
	st, err := os.Stat(repoPath)
	if err != nil || !st.IsDir() {
		writeErr(w, http.StatusNotFound, "repo directory not found")
		return
	}
	if !fileExists(filepath.Join(repoPath, "config")) {
		writeErr(w, http.StatusBadRequest, "not a borg repo (missing config)")
		return
	}
	if err := s.addAuthorizedKey(req.Name, req.Pubkey, req.AppendOnly); err != nil {
		if errors.Is(err, errKeyExists) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, "authorized_keys: "+err.Error())
		return
	}
	ri := s.buildRepoInfo(req.Name, repoPath, st, map[string]repoKeyState{req.Name: {Registered: true, AppendOnly: req.AppendOnly}})
	writeJSON(w, http.StatusCreated, ri)
}

func (s *Server) getRepo(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !nameRe.MatchString(name) {
		writeErr(w, http.StatusBadRequest, "invalid name")
		return
	}
	repoPath := filepath.Join(s.cfg.RepoRoot, name)
	st, err := os.Stat(repoPath)
	if err != nil || !st.IsDir() {
		writeErr(w, http.StatusNotFound, "repo not found")
		return
	}
	states := s.scanAuthKeys()
	out := repoDetail{repoInfo: s.buildRepoInfo(name, repoPath, st, states)}
	if out.Initialized {
		parseRepoConfig(filepath.Join(repoPath, "config"), &out)
		out.Segments, out.LastWrite = scanDataDir(filepath.Join(repoPath, "data"))
	}
	writeJSON(w, http.StatusOK, out)
}

// parseRepoConfig reads a borg repo config file (INI-ish) and fills detail.
func parseRepoConfig(path string, d *repoDetail) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	section := ""
	for _, raw := range strings.Split(string(b), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(strings.Trim(line, "[]"))
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		if section == "repository" {
			switch key {
			case "id":
				d.ID = val
			case "version":
				d.Version = val
			}
		}
		if section == "encryption" && key == "type" {
			d.Encryption = val
		}
	}
}

// scanDataDir counts segment files and finds the most recent mtime.
func scanDataDir(path string) (int, string) {
	count := 0
	var latest time.Time
	_ = filepath.Walk(path, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		count++
		if info.ModTime().After(latest) {
			latest = info.ModTime()
		}
		return nil
	})
	if latest.IsZero() {
		return count, ""
	}
	return count, latest.UTC().Format(time.RFC3339)
}

func (s *Server) patchRepo(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !nameRe.MatchString(name) {
		writeErr(w, http.StatusBadRequest, "invalid name")
		return
	}
	var req patchRepoReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 16*1024)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if req.AppendOnly == nil {
		writeErr(w, http.StatusBadRequest, "nothing to update")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	repoPath := filepath.Join(s.cfg.RepoRoot, name)
	st, err := os.Stat(repoPath)
	if err != nil || !st.IsDir() {
		writeErr(w, http.StatusNotFound, "repo not found")
		return
	}
	if err := s.setAppendOnly(name, *req.AppendOnly); err != nil {
		if errors.Is(err, errKeyNotFound) {
			writeErr(w, http.StatusConflict, "repo has no borgbox-managed key (import or create first)")
			return
		}
		writeErr(w, http.StatusInternalServerError, "authorized_keys: "+err.Error())
		return
	}
	ri := s.buildRepoInfo(name, repoPath, st, s.scanAuthKeys())
	writeJSON(w, http.StatusOK, ri)
}

func (s *Server) deleteRepo(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !nameRe.MatchString(name) {
		writeErr(w, http.StatusBadRequest, "invalid name")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	repoPath := filepath.Join(s.cfg.RepoRoot, name)
	if err := os.RemoveAll(repoPath); err != nil {
		writeErr(w, http.StatusInternalServerError, "remove: "+err.Error())
		return
	}
	if err := s.removeAuthorizedKey(name); err != nil {
		writeErr(w, http.StatusInternalServerError, "authorized_keys: "+err.Error())
		return
	}
	if s.monitor != nil {
		s.monitor.deleteByRepo(name)
	}
	w.WriteHeader(http.StatusNoContent)
}

// addAuthorizedKey appends a key with a forced borg serve command restricted
// to the repo's directory. The line is tagged so it can be removed later.
// When appendOnly is true, --append-only is added to the serve command so the
// client can create new archives but cannot delete them.
func (s *Server) addAuthorizedKey(name, pubkey string, appendOnly bool) error {
	if err := os.MkdirAll(filepath.Dir(s.cfg.AuthKeys), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(s.cfg.AuthKeys, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	existing, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	tag := fmt.Sprintf("# borgbox:%s", name)
	if strings.Contains(string(existing), tag) {
		return errKeyExists
	}

	repoPath := filepath.Join(s.cfg.RepoRoot, name)
	serveCmd := "borg serve --restrict-to-repository " + repoPath
	if appendOnly {
		serveCmd += " --append-only"
	}
	line := fmt.Sprintf("command=\"%s\",restrict %s %s\n",
		serveCmd, strings.TrimSpace(pubkey), tag)

	if len(existing) > 0 && existing[len(existing)-1] != '\n' {
		if _, err := f.Write([]byte("\n")); err != nil {
			return err
		}
	}
	if _, err := f.Write([]byte(line)); err != nil {
		return err
	}
	return f.Sync()
}

// setAppendOnly rewrites the command= portion of the borgbox-managed line in
// authorized_keys for `name`, adding or removing --append-only. Returns
// errKeyNotFound if no tagged line exists.
func (s *Server) setAppendOnly(name string, appendOnly bool) error {
	b, err := os.ReadFile(s.cfg.AuthKeys)
	if err != nil {
		return err
	}
	tag := fmt.Sprintf("# borgbox:%s", name)
	lines := strings.Split(string(b), "\n")
	found := false
	for i, line := range lines {
		if !strings.Contains(line, tag) {
			continue
		}
		found = true
		lines[i] = rewriteAppendOnlyFlag(line, appendOnly)
	}
	if !found {
		return errKeyNotFound
	}
	return os.WriteFile(s.cfg.AuthKeys, []byte(strings.Join(lines, "\n")), 0o600)
}

func rewriteAppendOnlyFlag(line string, appendOnly bool) string {
	return commandRe.ReplaceAllStringFunc(line, func(match string) string {
		inner := commandRe.FindStringSubmatch(match)[1]
		inner = strings.ReplaceAll(inner, " --append-only", "")
		if appendOnly {
			inner += " --append-only"
		}
		return `command="` + inner + `"`
	})
}

func (s *Server) removeAuthorizedKey(name string) error {
	b, err := os.ReadFile(s.cfg.AuthKeys)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	tag := fmt.Sprintf("# borgbox:%s", name)
	lines := strings.Split(string(b), "\n")
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if strings.Contains(l, tag) {
			continue
		}
		out = append(out, l)
	}
	return os.WriteFile(s.cfg.AuthKeys, []byte(strings.Join(out, "\n")), 0o600)
}

func validPubkey(s string) bool {
	parts := strings.Fields(s)
	if len(parts) < 2 {
		return false
	}
	switch parts[0] {
	case "ssh-ed25519", "ssh-rsa", "ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521":
		return len(parts[1]) >= 32
	}
	return false
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func dirSize(p string) int64 {
	var total int64
	_ = filepath.Walk(p, func(_ string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		total += info.Size()
		return nil
	})
	return total
}

func borgVersion() string {
	out, err := exec.Command("borg", "--version").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func borgEnv(passphrase string) []string {
	e := append([]string{}, os.Environ()...)
	e = append(e,
		"BORG_UNKNOWN_UNENCRYPTED_REPO_ACCESS_IS_OK=yes",
		"BORG_RELOCATED_REPO_ACCESS_IS_OK=yes",
	)
	if passphrase != "" {
		e = append(e, "BORG_PASSPHRASE="+passphrase)
	}
	return e
}

func hostUptime() int64 {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	parts := strings.Fields(string(b))
	if len(parts) == 0 {
		return 0
	}
	v, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return 0
	}
	return int64(v)
}

func loadAvg() [3]float64 {
	var l [3]float64
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return l
	}
	parts := strings.Fields(string(b))
	if len(parts) < 3 {
		return l
	}
	for i := 0; i < 3; i++ {
		v, _ := strconv.ParseFloat(parts[i], 64)
		l[i] = v
	}
	return l
}

func (s *Server) systemStats(w http.ResponseWriter, r *http.Request) {
	var st syscall.Statfs_t
	var total, free, used uint64
	if err := syscall.Statfs(s.cfg.RepoRoot, &st); err == nil {
		total = st.Blocks * uint64(st.Bsize)
		free = st.Bavail * uint64(st.Bsize)
		used = total - st.Bfree*uint64(st.Bsize)
	}
	host, _ := os.Hostname()
	writeJSON(w, http.StatusOK, systemStatsResp{
		Hostname:      host,
		UptimeSeconds: hostUptime(),
		Load:          loadAvg(),
		Storage: storageInfo{
			Path:       s.cfg.RepoRoot,
			TotalBytes: total,
			UsedBytes:  used,
			FreeBytes:  free,
		},
		BorgboxVersion: version,
		BorgVersion:    borgVersion(),
	})
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	out := []sessionInfo{}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		writeJSON(w, http.StatusOK, out)
		return
	}
	rootAbs, _ := filepath.Abs(s.cfg.RepoRoot)
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		cmdlineBytes, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err != nil || len(cmdlineBytes) == 0 {
			continue
		}
		args := strings.Split(strings.TrimRight(string(cmdlineBytes), "\x00"), "\x00")
		isBorgServe := false
		for i, a := range args {
			base := filepath.Base(a)
			if (base == "borg" || strings.HasPrefix(base, "borg")) && i+1 < len(args) && args[i+1] == "serve" {
				isBorgServe = true
				break
			}
		}
		if !isBorgServe {
			continue
		}
		repoPath := ""
		for i, a := range args {
			if a == "--restrict-to-repository" && i+1 < len(args) {
				repoPath = args[i+1]
				break
			}
		}
		if repoPath == "" {
			continue
		}
		repoAbs, _ := filepath.Abs(repoPath)
		if rootAbs != "" && !strings.HasPrefix(repoAbs, rootAbs) {
			continue
		}
		rss := int64(0)
		if sb, err := os.ReadFile("/proc/" + e.Name() + "/status"); err == nil {
			for _, line := range strings.Split(string(sb), "\n") {
				if strings.HasPrefix(line, "VmRSS:") {
					var kb int64
					fmt.Sscanf(line, "VmRSS: %d kB", &kb)
					rss = kb * 1024
					break
				}
			}
		}
		started := ""
		if st, err := os.Stat("/proc/" + e.Name()); err == nil {
			started = st.ModTime().UTC().Format(time.RFC3339)
		}
		out = append(out, sessionInfo{
			PID:       pid,
			Repo:      filepath.Base(repoPath),
			RepoPath:  repoPath,
			StartedAt: started,
			RSSBytes:  rss,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) listArchives(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !nameRe.MatchString(name) {
		writeErr(w, http.StatusBadRequest, "invalid name")
		return
	}
	repoPath := filepath.Join(s.cfg.RepoRoot, name)
	if !fileExists(filepath.Join(repoPath, "config")) {
		writeErr(w, http.StatusNotFound, "repo not initialized")
		return
	}
	cmd := exec.Command("borg", "list", "--json", repoPath)
	cmd.Env = borgEnv(r.Header.Get("X-Borg-Passphrase"))
	out, err := cmd.Output()
	if err != nil {
		msg := err.Error()
		if ee, ok := err.(*exec.ExitError); ok {
			if s := strings.TrimSpace(string(ee.Stderr)); s != "" {
				msg = s
			}
		}
		writeErr(w, http.StatusInternalServerError, "borg list: "+msg)
		return
	}
	var parsed struct {
		Archives []archiveInfo `json:"archives"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		writeErr(w, http.StatusInternalServerError, "parse: "+err.Error())
		return
	}
	if parsed.Archives == nil {
		parsed.Archives = []archiveInfo{}
	}
	writeJSON(w, http.StatusOK, parsed.Archives)
}

func (s *Server) breakLock(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !nameRe.MatchString(name) {
		writeErr(w, http.StatusBadRequest, "invalid name")
		return
	}
	repoPath := filepath.Join(s.cfg.RepoRoot, name)
	if !fileExists(filepath.Join(repoPath, "config")) {
		writeErr(w, http.StatusNotFound, "repo not initialized")
		return
	}
	cmd := exec.Command("borg", "break-lock", repoPath)
	cmd.Env = borgEnv("")
	if out, err := cmd.CombinedOutput(); err != nil {
		writeErr(w, http.StatusInternalServerError, "break-lock: "+strings.TrimSpace(string(out)))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func decodeOptionalJSON(r *http.Request, dst any) error {
	if r.ContentLength == 0 {
		return nil
	}
	return json.NewDecoder(io.LimitReader(r.Body, 16*1024)).Decode(dst)
}

func (s *Server) checkRepo(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !nameRe.MatchString(name) {
		writeErr(w, http.StatusBadRequest, "invalid name")
		return
	}
	var req checkReq
	if err := decodeOptionalJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	repoPath := filepath.Join(s.cfg.RepoRoot, name)
	if !fileExists(filepath.Join(repoPath, "config")) {
		writeErr(w, http.StatusNotFound, "repo not initialized")
		return
	}
	args := []string{"check", "--info"}
	if req.Repair {
		args = append(args, "--repair")
	}
	if req.VerifyData {
		args = append(args, "--verify-data")
	}
	args = append(args, repoPath)
	j := s.jobs.create(name, "check", false)
	cmd := exec.Command("borg", args...)
	cmd.Env = borgEnv(req.Passphrase)
	go s.jobs.run(j.ID, cmd)
	writeJSON(w, http.StatusAccepted, map[string]string{"job_id": j.ID, "status": j.Status})
}

func (s *Server) pruneRepo(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !nameRe.MatchString(name) {
		writeErr(w, http.StatusBadRequest, "invalid name")
		return
	}
	var req pruneReq
	if err := decodeOptionalJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	if req.KeepDaily <= 0 && req.KeepWeekly <= 0 && req.KeepMonthly <= 0 && req.KeepYearly <= 0 {
		writeErr(w, http.StatusBadRequest, "at least one keep_* must be > 0")
		return
	}
	repoPath := filepath.Join(s.cfg.RepoRoot, name)
	if !fileExists(filepath.Join(repoPath, "config")) {
		writeErr(w, http.StatusNotFound, "repo not initialized")
		return
	}
	args := []string{"prune", "--list"}
	if req.DryRun {
		args = append(args, "--dry-run")
	}
	if req.KeepDaily > 0 {
		args = append(args, "--keep-daily", strconv.Itoa(req.KeepDaily))
	}
	if req.KeepWeekly > 0 {
		args = append(args, "--keep-weekly", strconv.Itoa(req.KeepWeekly))
	}
	if req.KeepMonthly > 0 {
		args = append(args, "--keep-monthly", strconv.Itoa(req.KeepMonthly))
	}
	if req.KeepYearly > 0 {
		args = append(args, "--keep-yearly", strconv.Itoa(req.KeepYearly))
	}
	args = append(args, repoPath)
	j := s.jobs.create(name, "prune", req.DryRun)
	cmd := exec.Command("borg", args...)
	cmd.Env = borgEnv(req.Passphrase)
	go s.jobs.run(j.ID, cmd)
	writeJSON(w, http.StatusAccepted, map[string]string{"job_id": j.ID, "status": j.Status})
}

func (s *Server) compactRepo(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !nameRe.MatchString(name) {
		writeErr(w, http.StatusBadRequest, "invalid name")
		return
	}
	var req compactReq
	if err := decodeOptionalJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	repoPath := filepath.Join(s.cfg.RepoRoot, name)
	if !fileExists(filepath.Join(repoPath, "config")) {
		writeErr(w, http.StatusNotFound, "repo not initialized")
		return
	}
	j := s.jobs.create(name, "compact", false)
	cmd := exec.Command("borg", "compact", "--info", repoPath)
	cmd.Env = borgEnv(req.Passphrase)
	go s.jobs.run(j.ID, cmd)
	writeJSON(w, http.StatusAccepted, map[string]string{"job_id": j.ID, "status": j.Status})
}

func (s *Server) listJobs(w http.ResponseWriter, r *http.Request) {
	out := s.jobs.list()
	if out == nil {
		out = []*jobRec{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !jobIDRe.MatchString(id) {
		writeErr(w, http.StatusBadRequest, "invalid job id")
		return
	}
	j, ok := s.jobs.get(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	writeJSON(w, http.StatusOK, j)
}

func (s *Server) streamJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !jobIDRe.MatchString(id) {
		writeErr(w, http.StatusBadRequest, "invalid job id")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming not supported")
		return
	}
	ch, unsub, ok := s.jobs.subscribe(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "job not found")
		return
	}
	defer unsub()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ctx := r.Context()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case ev, open := <-ch:
			if !open {
				fmt.Fprint(w, "event: end\ndata: {}\n\n")
				flusher.Flush()
				return
			}
			b, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, b); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
