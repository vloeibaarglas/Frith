// frith is a minimal, dependency-free self-hosted media server.
//
// It serves any file from a data directory (images, html, gif, mp4, pdf, ...)
// with correct MIME types, byte-range support (so video seeks work) and
// token-authenticated uploads. No database, no runtime, no external modules —
// everything is the Go standard library.
//
// Only three routes exist:
//
//	GET  {path}/{name}    serve an uploaded file (path defaults to /, e.g. /u)
//	POST /api/upload      upload one or more files (Authorization: Bearer <key>)
//	GET  /api/healthcheck liveness probe
//
// Configuration is via env vars or flags:
//
//	UPLOAD_ADDR   listen address                (default :8080)
//	UPLOAD_DIR    data directory                (default ./uploads)
//	UPLOAD_TOKEN  comma-separated upload keys   (required)
//	PUBLIC_URL    base URL used in responses    (default: request host)
//	UPLOAD_PATH   URL prefix for serving files  (default / = root; e.g. /u)
//	UPLOAD_MAX_MB max upload size in MB         (default 500)
//	UPLOAD_ALLOW   comma-separated allow-list of extensions (default: safe media set;
//	              empty string allows everything subject to UPLOAD_DENY)
//	UPLOAD_DENY    comma-separated deny-list of extensions (default: none)
//	UPLOAD_LIST    enable GET /api/files (token-authenticated) (default false)
//	UPLOAD_ORIGINAL_NAME  capture and serve original filenames (default false)
package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const charset = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// mimeByExt covers the types we care about plus common extras. Anything not
// listed falls back to mime.TypeByExtension and then application/octet-stream.
var mimeByExt = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".svg":  "image/svg+xml",
	".webp": "image/webp",
	".avif": "image/avif",
	".mp4":  "video/mp4",
	".webm": "video/webm",
	".mov":  "video/quicktime",
	".m4v":  "video/x-m4v",
	".pdf":  "application/pdf",
	".html": "text/html; charset=utf-8",
	".htm":  "text/html; charset=utf-8",
	".txt":  "text/plain; charset=utf-8",
	".md":   "text/markdown; charset=utf-8",
	".json": "application/json",
	".csv":  "text/csv; charset=utf-8",
	".css":  "text/css; charset=utf-8",
	".js":   "text/javascript",
	".xml":  "text/xml",
	".zip":  "application/zip",
	".gz":   "application/gzip",
}

// cacheableExt gets a long cache lifetime when served; everything else is
// no-store so edits to html/text show up immediately.
var cacheableExt = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".svg": true,
	".webp": true, ".avif": true, ".mp4": true, ".webm": true, ".mov": true,
	".m4v": true, ".pdf": true,
}

var defaultAllow = []string{
	"png", "jpg", "jpeg", "gif", "svg", "webp", "avif",
	"mp4", "webm", "mov", "m4v",
	"pdf", "html", "htm", "txt", "md", "json", "csv", "css", "js", "xml",
	"zip", "gz",
}

var (
	errTooLarge      = errors.New("file exceeds the maximum upload size")
	errExtNotAllowed = errors.New("file extension is not allowed")
)

type config struct {
	addr         string
	dataDir      string
	route        string // URL prefix where files are served; "" = root, else e.g. "/u"
	urlBase      string
	tokens       []string
	maxBytes     int64
	allow        map[string]bool
	deny         map[string]bool
	list         bool // expose GET /api/files (token-authenticated)
	originalName bool // capture + serve original filenames
}

type fileResult struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Type         string `json:"type"`
	Size         int64  `json:"size"`
	URL          string `json:"url"`
	OriginalName string `json:"originalName,omitempty"`
}

func main() {
	cfg := parseConfig()
	if err := os.MkdirAll(cfg.dataDir, 0o755); err != nil {
		log.Fatalf("cannot create data dir %s: %v", cfg.dataDir, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/healthcheck", handleHealthcheck)
	mux.HandleFunc("POST /api/upload", handleUpload(cfg))
	mux.HandleFunc("GET "+filePattern(cfg.route), serveFile(cfg))
	if cfg.list {
		mux.HandleFunc("GET /api/files", handleList(cfg))
	}

	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           withHeaders(mux),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.Printf("frith listening on %s (data: %s, %d tokens, max %dMB)",
		cfg.addr, cfg.dataDir, len(cfg.tokens), cfg.maxBytes>>20)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func parseConfig() config {
	addr := flag.String("addr", envOr("UPLOAD_ADDR", ":8080"), "listen address")
	dataDir := flag.String("data", envOr("UPLOAD_DIR", "./uploads"), "data directory")
	urlBase := flag.String("url-base", envOr("PUBLIC_URL", ""), "public base URL for returned links (defaults to request host)")
	path := flag.String("path", envOr("UPLOAD_PATH", "/"), "URL prefix for serving files (/ = root, e.g. /u)")
	maxMB := flag.Int64("max-mb", envInt64("UPLOAD_MAX_MB", 500), "max upload size in MB")
	allow := flag.String("allow", envOr("UPLOAD_ALLOW", strings.Join(defaultAllow, ",")), "comma-separated allowed extensions (empty = allow all)")
	deny := flag.String("deny", envOr("UPLOAD_DENY", ""), "comma-separated denied extensions")
	list := flag.Bool("list", envBool("UPLOAD_LIST", false), "enable GET /api/files listing (token-authenticated)")
	originalName := flag.Bool("original-name", envBool("UPLOAD_ORIGINAL_NAME", false), "capture and serve original filenames via Content-Disposition")
	flag.Parse()

	tokenStr := strings.TrimSpace(envOr("UPLOAD_TOKEN", ""))
	if tokenStr == "" {
		log.Fatal("UPLOAD_TOKEN must be set (clients send 'Authorization: Bearer <key>')")
	}
	var tokens []string
	for _, t := range strings.Split(tokenStr, ",") {
		if t = strings.TrimSpace(t); t != "" {
			tokens = append(tokens, t)
		}
	}
	if len(tokens) == 0 {
		log.Fatal("UPLOAD_TOKEN must contain at least one non-empty key")
	}

	// UPLOAD_ALLOW is read via LookupEnv so that an *empty* value means "allow
	// everything" rather than "use the default". Absent env + absent flag keeps
	// the safe default media set.
	allowVal := *allow
	if env, ok := os.LookupEnv("UPLOAD_ALLOW"); ok {
		allowVal = env
	}
	denyVal := *deny
	if env, ok := os.LookupEnv("UPLOAD_DENY"); ok {
		denyVal = env
	}

	route := strings.TrimSpace(*path)
	if !strings.HasPrefix(route, "/") {
		log.Fatal("UPLOAD_PATH must start with '/'")
	}
	route = strings.TrimRight(route, "/")

	return config{
		addr:         *addr,
		dataDir:      *dataDir,
		route:        route,
		urlBase:      strings.TrimRight(*urlBase, "/"),
		tokens:       tokens,
		maxBytes:     *maxMB << 20,
		allow:        parseExtList(allowVal),
		deny:         parseExtList(denyVal),
		list:         *list,
		originalName: *originalName,
	}
}

// filePattern returns the ServeMux pattern that serves a single file segment
// under the configured route ("" for root, else the route prefix).
func filePattern(route string) string {
	return route + "/{file}"
}

// parseExtList converts a comma-separated extension list into a map keyed by
// ".ext". An empty string yields an empty (allows-everything) map.
func parseExtList(list string) map[string]bool {
	set := map[string]bool{}
	for _, e := range strings.Split(list, ",") {
		if e = strings.TrimSpace(e); e != "" {
			set["."+strings.ToLower(strings.TrimPrefix(e, "."))] = true
		}
	}
	return set
}

func withHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

// serveFile streams a stored file. ServeContent handles Range/Accept-Ranges so
// mp4/video seeking works, plus Last-Modified/If-None-Match for cheap 304s.
func serveFile(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("file")
		if name == "" || strings.HasPrefix(name, ".") {
			http.NotFound(w, r)
			return
		}
		f, err := os.Open(filepath.Join(cfg.dataDir, name))
		if err != nil {
			if os.IsNotExist(err) {
				http.NotFound(w, r)
				return
			}
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		defer f.Close()

		st, err := f.Stat()
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if st.IsDir() {
			http.NotFound(w, r)
			return
		}

		ext := filepath.Ext(name)
		w.Header().Set("Content-Type", mimeType(ext))
		if cacheableExt[ext] {
			w.Header().Set("Cache-Control", "public, max-age=2592000") // 30 days
		} else {
			w.Header().Set("Cache-Control", "no-store")
		}
		if cfg.originalName {
			if b, err := os.ReadFile(filepath.Join(cfg.dataDir, "."+name+".orig")); err == nil {
				if orig := sanitizeName(string(b)); orig != "" {
					disp := "inline"
					if _, ok := r.URL.Query()["download"]; ok {
						disp = "attachment"
					}
					w.Header().Set("Content-Disposition",
						disp+`; filename*=utf-8''`+encodeFilename(orig))
				}
			}
		}
		http.ServeContent(w, r, name, st.ModTime(), f)
	}
}

// handleUpload authenticates and stores uploaded files.
func handleUpload(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(r, cfg.tokens) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		base := cfg.urlBase
		if base == "" {
			scheme := "http"
			if r.Header.Get("X-Forwarded-Proto") == "https" {
				scheme = "https"
			}
			base = scheme + "://" + r.Host
		}
		cfg.urlBase = base

		var (
			results []fileResult
			err     error
		)
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			results, err = uploadMultipart(r, cfg)
		} else {
			results, err = uploadRaw(r, cfg)
		}
		if err != nil {
			var code int
			switch {
			case errors.Is(err, errTooLarge):
				code = http.StatusRequestEntityTooLarge
			case errors.Is(err, errExtNotAllowed):
				code = http.StatusUnsupportedMediaType
			default:
				code = http.StatusInternalServerError
			}
			http.Error(w, err.Error(), code)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"files": results})
	}
}

func uploadMultipart(r *http.Request, cfg config) ([]fileResult, error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, err
	}
	var results []fileResult
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if part.FileName() == "" {
			continue // plain form field, skip
		}
		res, err := saveStream(part, part.FileName(), cfg)
		if err != nil {
			return nil, err
		}
		results = append(results, res)
	}
	if len(results) == 0 {
		return nil, errors.New("no file parts found in multipart body")
	}
	return results, nil
}

func uploadRaw(r *http.Request, cfg config) ([]fileResult, error) {
	ext := strings.TrimPrefix(strings.ToLower(r.URL.Query().Get("ext")), ".")
	if ext == "" {
		ext = extFromContentType(r.Header.Get("Content-Type"))
	}
	if ext == "" {
		return nil, errors.New("cannot determine file extension: pass ?ext=png or a recognized Content-Type")
	}
	fname := r.URL.Query().Get("name")
	if fname == "" {
		fname = "upload." + ext
	}
	res, err := saveStream(r.Body, fname, cfg)
	if err != nil {
		return nil, err
	}
	return []fileResult{res}, nil
}

// saveStream copies a reader to disk under a freshly generated name and
// enforces the extension allow/deny lists and size limit.
func saveStream(r io.Reader, filename string, cfg config) (fileResult, error) {
	ext := strings.ToLower(filepath.Ext(filename))
	if cfg.deny[ext] || (len(cfg.allow) > 0 && !cfg.allow[ext]) {
		return fileResult{}, fmt.Errorf("%w: .%s", errExtNotAllowed, strings.TrimPrefix(ext, "."))
	}

	name, err := newName(cfg.dataDir, ext)
	if err != nil {
		return fileResult{}, err
	}
	path := filepath.Join(cfg.dataDir, name)

	dst, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fileResult{}, err
	}
	n, err := io.Copy(dst, io.LimitReader(r, cfg.maxBytes+1))
	if err == nil {
		err = dst.Close()
	}
	if err != nil {
		_ = os.Remove(path)
		return fileResult{}, err
	}
	if n > cfg.maxBytes {
		_ = os.Remove(path)
		return fileResult{}, errTooLarge
	}
	if n == 0 {
		_ = os.Remove(path)
		return fileResult{}, errors.New("empty file")
	}

	res := fileResult{
		ID:   name,
		Name: name,
		Type: mimeType(ext),
		Size: n,
		URL:  cfg.urlBase + cfg.route + "/" + name,
	}
	if cfg.originalName {
		if orig := sanitizeName(filename); orig != "" {
			if err := os.WriteFile(filepath.Join(cfg.dataDir, "."+name+".orig"), []byte(orig), 0o644); err != nil {
				_ = os.Remove(path)
				return fileResult{}, err
			}
			res.OriginalName = orig
		}
	}
	return res, nil
}

func handleHealthcheck(w http.ResponseWriter, r *http.Request) {
	_, _ = w.Write([]byte("ok"))
}

type listEntry struct {
	Name         string `json:"name"`
	Type         string `json:"type"`
	Size         int64  `json:"size"`
	Date         string `json:"date"`
	OriginalName string `json:"originalName,omitempty"`
}

// handleList returns a JSON inventory of the data dir (name, mime type, size,
// mtime). Opt-in via UPLOAD_LIST and token-authenticated.
func handleList(cfg config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkAuth(r, cfg.tokens) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		entries, err := os.ReadDir(cfg.dataDir)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		files := make([]listEntry, 0, len(entries))
		for _, e := range entries {
			if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			entry := listEntry{
				Name: e.Name(),
				Type: mimeType(filepath.Ext(e.Name())),
				Size: info.Size(),
				Date: info.ModTime().UTC().Format(time.RFC3339),
			}
			if cfg.originalName {
				if b, err := os.ReadFile(filepath.Join(cfg.dataDir, "."+e.Name()+".orig")); err == nil {
					entry.OriginalName = sanitizeName(string(b))
				}
			}
			files = append(files, entry)
		}
		sort.Slice(files, func(i, j int) bool { return files[i].Date > files[j].Date })
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"files": files})
	}
}

// checkAuth validates the Authorization header. Only the standard
// "Bearer <key>" form (RFC 6750) is accepted; scheme matching is
// case-insensitive.
func checkAuth(r *http.Request, tokens []string) bool {
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	const prefix = "Bearer "
	if len(auth) <= len(prefix) || !strings.EqualFold(auth[:len(prefix)], prefix) {
		return false
	}
	key := strings.TrimSpace(auth[len(prefix):])
	if key == "" {
		return false
	}
	for _, tok := range tokens {
		if subtle.ConstantTimeCompare([]byte(key), []byte(tok)) == 1 {
			return true
		}
	}
	return false
}

// newName returns a random 6-char base62 name + extension that does not yet
// exist in the data dir.
func newName(dataDir, ext string) (string, error) {
	for i := 0; i < 100; i++ {
		base, err := randomName(6)
		if err != nil {
			return "", err
		}
		name := base + ext
		if _, err := os.Stat(filepath.Join(dataDir, name)); os.IsNotExist(err) {
			return name, nil
		}
	}
	return "", errors.New("could not generate a unique file name")
}

func randomName(n int) (string, error) {
	b := make([]byte, n)
	for {
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		ok := true
		for _, c := range b {
			if int(c) >= 256-(256%len(charset)) {
				ok = false
				break
			}
		}
		if !ok {
			continue
		}
		out := make([]byte, n)
		for i, c := range b {
			out[i] = charset[int(c)%len(charset)]
		}
		return string(out), nil
	}
}

func mimeType(ext string) string {
	if t, ok := mimeByExt[strings.ToLower(ext)]; ok {
		return t
	}
	if t := mime.TypeByExtension(strings.ToLower(ext)); t != "" {
		return t
	}
	return "application/octet-stream"
}

// extFromContentType maps a media type to a file extension (image/png -> png).
func extFromContentType(ct string) string {
	mt, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return ""
	}
	sub := mt
	if i := strings.IndexByte(mt, '/'); i >= 0 {
		sub = mt[i+1:]
	}
	if sub == "jpeg" {
		return "jpg"
	}
	return sub
}

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt64(key string, def int64) int64 {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func envBool(key string, def bool) bool {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

// sanitizeName reduces a client-supplied filename to a safe basename: no path
// separators, no control characters, no quotes, no surrounding whitespace.
func sanitizeName(name string) string {
	name = filepath.Base(name)
	name = strings.TrimSpace(name)
	name = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 || r == '"' {
			return -1
		}
		return r
	}, name)
	return name
}

// encodeFilename percent-encodes a filename for Content-Disposition's
// filename*=utf-8” form.
func encodeFilename(name string) string {
	return strings.ReplaceAll(url.QueryEscape(name), "+", "%20")
}
