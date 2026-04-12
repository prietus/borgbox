package main

import (
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
	"strings"
	"sync"
	"syscall"
	"time"
)

const version = "0.1.0"

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

var errKeyExists = errors.New("repo already registered; delete first or use a different name")

type Config struct {
	Addr     string
	RepoRoot string
	AuthKeys string
	Token    string
}

type Server struct {
	cfg   Config
	start time.Time
	mu    sync.Mutex
}

type createRepoReq struct {
	Name   string `json:"name"`
	Pubkey string `json:"pubkey"`
}

type repoInfo struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
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
	Version      string `json:"version"`
	RepoRoot     string `json:"repo_root"`
	FreeBytes    uint64 `json:"free_bytes"`
	TotalBytes   uint64 `json:"total_bytes"`
	UptimeSec    int64  `json:"uptime_sec"`
	BorgVersion  string `json:"borg_version,omitempty"`
	GoVersion    string `json:"go_version"`
}

func main() {
	cfg := Config{}
	flag.StringVar(&cfg.Addr, "addr", env("BORGBOX_ADDR", ":9999"), "listen address")
	flag.StringVar(&cfg.RepoRoot, "repo-root", env("BORGBOX_REPO_ROOT", ""), "directory where repos live")
	flag.StringVar(&cfg.AuthKeys, "authorized-keys", env("BORGBOX_AUTH_KEYS", ""), "authorized_keys path to manage")
	flag.StringVar(&cfg.Token, "token", env("BORGBOX_TOKEN", ""), "bearer token (use @/path to read from file)")
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

	s := &Server{cfg: cfg, start: time.Now()}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/health", s.health)
	mux.HandleFunc("GET /api/v1/info", s.auth(s.info))
	mux.HandleFunc("GET /api/v1/repos", s.auth(s.listRepos))
	mux.HandleFunc("POST /api/v1/repos", s.auth(s.createRepo))
	mux.HandleFunc("GET /api/v1/repos/{name}", s.auth(s.getRepo))
	mux.HandleFunc("DELETE /api/v1/repos/{name}", s.auth(s.deleteRepo))

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
	out := []repoInfo{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(s.cfg.RepoRoot, e.Name())
		info, _ := e.Info()
		ri := repoInfo{
			Name:        e.Name(),
			Path:        p,
			Initialized: fileExists(filepath.Join(p, "config")),
			SizeBytes:   dirSize(p),
		}
		if info != nil {
			ri.ModifiedAt = info.ModTime().UTC().Format(time.RFC3339)
		}
		out = append(out, ri)
	}
	writeJSON(w, http.StatusOK, out)
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

	if err := s.addAuthorizedKey(req.Name, req.Pubkey); err != nil {
		if errors.Is(err, errKeyExists) {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		writeErr(w, http.StatusInternalServerError, "authorized_keys: "+err.Error())
		return
	}

	st, _ := os.Stat(repoPath)
	mt := ""
	if st != nil {
		mt = st.ModTime().UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusCreated, repoInfo{
		Name: req.Name, Path: repoPath, Initialized: false, ModifiedAt: mt,
	})
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
	out := repoDetail{repoInfo: repoInfo{
		Name:        name,
		Path:        repoPath,
		SizeBytes:   dirSize(repoPath),
		Initialized: fileExists(filepath.Join(repoPath, "config")),
		ModifiedAt:  st.ModTime().UTC().Format(time.RFC3339),
	}}
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
	w.WriteHeader(http.StatusNoContent)
}

// addAuthorizedKey appends a key with a forced borg serve command restricted
// to the repo's directory. The line is tagged so it can be removed later.
func (s *Server) addAuthorizedKey(name, pubkey string) error {
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
	line := fmt.Sprintf("command=\"borg serve --restrict-to-repository %s\",restrict %s %s\n",
		repoPath, strings.TrimSpace(pubkey), tag)

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
