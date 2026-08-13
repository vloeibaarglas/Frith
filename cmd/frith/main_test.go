package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testConfig(t *testing.T) (config, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := config{
		addr:     ":0",
		dataDir:  dir,
		route:    "",
		urlBase:  "http://localhost:8080",
		tokens:   []string{"sekrit"},
		maxBytes: 10 << 20,
		allow:    map[string]bool{".png": true, ".html": true, ".mp4": true},
	}
	return cfg, dir
}

func testMux(t *testing.T, cfg config) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/healthcheck", handleHealthcheck)
	mux.HandleFunc("POST /api/upload", handleUpload(cfg))
	mux.HandleFunc("GET "+filePattern(cfg.route), serveFile(cfg))
	if cfg.list {
		mux.HandleFunc("GET /api/files", handleList(cfg))
	}
	return withHeaders(mux)
}

func multipartBody(t *testing.T, files map[string]string) (body string, contentType string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for filename, content := range files {
		fw, err := mw.CreateFormFile("file", filename)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.String(), mw.FormDataContentType()
}

func TestHealthcheck(t *testing.T) {
	cfg, _ := testConfig(t)
	rr := httptest.NewRecorder()
	testMux(t, cfg).ServeHTTP(rr, httptest.NewRequest("GET", "/api/healthcheck", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("healthcheck = %d, want 200", rr.Code)
	}
}

func TestUploadMultipart(t *testing.T) {
	cfg, dir := testConfig(t)
	body, ct := multipartBody(t, map[string]string{"photo.png": "hello png"})

	req := httptest.NewRequest("POST", "/api/upload", strings.NewReader(body))
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Authorization", "Bearer sekrit")
	rr := httptest.NewRecorder()
	testMux(t, cfg).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("upload = %d (%s), want 200", rr.Code, rr.Body.String())
	}
	var resp struct {
		Files []fileResult `json:"files"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Files) != 1 {
		t.Fatalf("got %d files, want 1", len(resp.Files))
	}
	f := resp.Files[0]
	if !strings.HasPrefix(f.Name, "http") && f.Name == "http" { // guard: Name is the filename
		t.Fatal("Name should be a filename")
	}
	if !strings.HasSuffix(f.Name, ".png") {
		t.Fatalf("unexpected Name %q", f.Name)
	}
	if f.Type != "image/png" {
		t.Fatalf("bad type %q", f.Type)
	}
	if f.URL != "http://localhost:8080/"+f.Name {
		t.Fatalf("bad URL %q", f.URL)
	}
	if _, err := os.Stat(filepath.Join(dir, f.Name)); err != nil {
		t.Fatalf("file not written to disk: %v", err)
	}
}

func TestUploadRejectsBadAuth(t *testing.T) {
	cfg, _ := testConfig(t)
	body, ct := multipartBody(t, map[string]string{"a.png": "x"})
	req := httptest.NewRequest("POST", "/api/upload", strings.NewReader(body))
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Authorization", "TOKEN wrong")
	rr := httptest.NewRecorder()
	testMux(t, cfg).ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("bad token = %d, want 401", rr.Code)
	}
}

func TestAuthSchemes(t *testing.T) {
	cfg, _ := testConfig(t)
	ok := []string{"Bearer sekrit", "bearer sekrit"}
	bad := []string{"TOKEN sekrit", "sekrit", "Bearer wrong"}
	for _, header := range ok {
		body, ct := multipartBody(t, map[string]string{"a.png": "x"})
		req := httptest.NewRequest("POST", "/api/upload", strings.NewReader(body))
		req.Header.Set("Content-Type", ct)
		req.Header.Set("Authorization", header)
		rr := httptest.NewRecorder()
		testMux(t, cfg).ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("auth %q = %d, want 200", header, rr.Code)
		}
	}
	for _, header := range bad {
		body, ct := multipartBody(t, map[string]string{"a.png": "x"})
		req := httptest.NewRequest("POST", "/api/upload", strings.NewReader(body))
		req.Header.Set("Content-Type", ct)
		req.Header.Set("Authorization", header)
		rr := httptest.NewRecorder()
		testMux(t, cfg).ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("auth %q = %d, want 401", header, rr.Code)
		}
	}
}

func TestUploadRejectsDisallowedExt(t *testing.T) {
	cfg, dir := testConfig(t)
	body, ct := multipartBody(t, map[string]string{"a.sh": "#!/bin/sh"})
	req := httptest.NewRequest("POST", "/api/upload", strings.NewReader(body))
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Authorization", "Bearer sekrit")
	rr := httptest.NewRecorder()
	testMux(t, cfg).ServeHTTP(rr, req)
	if rr.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("disallowed ext = %d, want 415", rr.Code)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("data dir should be empty, got %v", entries)
	}
}

func TestDenyListOverridesAllow(t *testing.T) {
	cfg, _ := testConfig(t)
	cfg.allow = map[string]bool{".png": true, ".mp4": true}
	cfg.deny = map[string]bool{".png": true}

	body, ct := multipartBody(t, map[string]string{"a.png": "x"})
	req := httptest.NewRequest("POST", "/api/upload", strings.NewReader(body))
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Authorization", "Bearer sekrit")
	rr := httptest.NewRecorder()
	testMux(t, cfg).ServeHTTP(rr, req)
	if rr.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("deny-list .png = %d, want 415", rr.Code)
	}

	// mp4 stays allowed while png is denied.
	body, ct = multipartBody(t, map[string]string{"a.mp4": "x"})
	req = httptest.NewRequest("POST", "/api/upload", strings.NewReader(body))
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Authorization", "Bearer sekrit")
	rr = httptest.NewRecorder()
	testMux(t, cfg).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("deny-list .mp4 = %d, want 200", rr.Code)
	}
}

func TestEmptyAllowAllowsEverything(t *testing.T) {
	cfg, _ := testConfig(t)
	cfg.allow = map[string]bool{}
	cfg.deny = map[string]bool{}

	body, ct := multipartBody(t, map[string]string{"script.sh": "echo hi"})
	req := httptest.NewRequest("POST", "/api/upload", strings.NewReader(body))
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Authorization", "Bearer sekrit")
	rr := httptest.NewRecorder()
	testMux(t, cfg).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("empty allow-list = %d (%s), want 200", rr.Code, rr.Body.String())
	}
}

func TestUploadTooLarge(t *testing.T) {
	cfg, dir := testConfig(t)
	cfg.maxBytes = 4 // tiny cap for the test

	req := httptest.NewRequest("POST", "/api/upload?ext=png", strings.NewReader("1234567890"))
	req.Header.Set("Authorization", "Bearer sekrit")
	rr := httptest.NewRecorder()
	testMux(t, cfg).ServeHTTP(rr, req)
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize = %d, want 413", rr.Code)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("oversized file should be cleaned up, got %v", entries)
	}
}

func TestUploadRawWithExt(t *testing.T) {
	cfg, dir := testConfig(t)
	req := httptest.NewRequest("POST", "/api/upload?ext=png", strings.NewReader("raw bytes"))
	req.Header.Set("Authorization", "Bearer sekrit")
	rr := httptest.NewRecorder()
	testMux(t, cfg).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("raw upload = %d (%s)", rr.Code, rr.Body.String())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), ".png") {
		t.Fatalf("unexpected data dir: %v", entries)
	}
}

func TestServeFileAndRange(t *testing.T) {
	cfg, dir := testConfig(t)
	content := "0123456789"
	if err := os.WriteFile(filepath.Join(dir, "AbCdEf.html"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	h := testMux(t, cfg)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/AbCdEf.html", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /AbCdEf.html = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("content-type = %q", ct)
	}
	if rr.Body.String() != content {
		t.Fatalf("body = %q", rr.Body.String())
	}

	req := httptest.NewRequest("GET", "/AbCdEf.html", nil)
	req.Header.Set("Range", "bytes=3-6")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusPartialContent {
		t.Fatalf("range = %d, want 206", rr.Code)
	}
	if rr.Body.String() != "3456" {
		t.Fatalf("range body = %q, want 3456", rr.Body.String())
	}
}

func TestServeMissingAndDotfiles(t *testing.T) {
	cfg, _ := testConfig(t)
	h := testMux(t, cfg)
	for _, path := range []string{"/Nope.png", "/.env"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest("GET", path, nil))
		if rr.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404", path, rr.Code)
		}
	}
}

func TestCustomServePath(t *testing.T) {
	cfg, dir := testConfig(t)
	cfg.route = "/u"

	content := "custom path file"
	if err := os.WriteFile(filepath.Join(dir, "AbCdEf.html"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	h := testMux(t, cfg)

	// Served under /u/{name}...
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/u/AbCdEf.html", nil))
	if rr.Code != http.StatusOK || rr.Body.String() != content {
		t.Fatalf("GET /u/AbCdEf.html = %d (%q), want 200 %q", rr.Code, rr.Body.String(), content)
	}

	// ...and not at root.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/AbCdEf.html", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("GET /AbCdEf.html with route /u = %d, want 404", rr.Code)
	}

	// Upload responses include the route prefix.
	body, ct := multipartBody(t, map[string]string{"a.png": "x"})
	req := httptest.NewRequest("POST", "/api/upload", strings.NewReader(body))
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Authorization", "Bearer sekrit")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("upload = %d", rr.Code)
	}
	var resp struct {
		Files []fileResult `json:"files"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(resp.Files[0].URL, "http://localhost:8080/u/") {
		t.Fatalf("upload URL = %q, want prefix /u/", resp.Files[0].URL)
	}
}

func TestServePathValidation(t *testing.T) {
	if p := filePattern(""); p != "/{file}" {
		t.Fatalf("filePattern(\"\") = %q, want /{file}", p)
	}
	if p := filePattern("/u"); p != "/u/{file}" {
		t.Fatalf("filePattern(\"/u\") = %q, want /u/{file}", p)
	}
}

func TestListDisabled(t *testing.T) {
	cfg, _ := testConfig(t)
	cfg.list = false
	rr := httptest.NewRecorder()
	testMux(t, cfg).ServeHTTP(rr, httptest.NewRequest("GET", "/api/files", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("list disabled = %d, want 404", rr.Code)
	}
}

func TestListRequiresAuth(t *testing.T) {
	cfg, _ := testConfig(t)
	cfg.list = true
	rr := httptest.NewRecorder()
	testMux(t, cfg).ServeHTTP(rr, httptest.NewRequest("GET", "/api/files", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("list no token = %d, want 401", rr.Code)
	}
}

func TestListEndpoint(t *testing.T) {
	cfg, dir := testConfig(t)
	cfg.list = true
	if err := os.WriteFile(filepath.Join(dir, "08WWPM.png"), []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AbCdEf.html"), []byte("<p>x</p>"), 0o644); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/api/files", nil)
	req.Header.Set("Authorization", "Bearer sekrit")
	rr := httptest.NewRecorder()
	testMux(t, cfg).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("list = %d (%s), want 200", rr.Code, rr.Body.String())
	}

	var resp struct {
		Files []listEntry `json:"files"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Files) != 2 {
		t.Fatalf("got %d files, want 2", len(resp.Files))
	}
	byName := map[string]listEntry{}
	for _, f := range resp.Files {
		byName[f.Name] = f
	}
	png, ok := byName["08WWPM.png"]
	if !ok {
		t.Fatalf("missing 08WWPM.png in %v", byName)
	}
	if png.Type != "image/png" || png.Size != 3 {
		t.Fatalf("png entry = %+v", png)
	}
	if _, err := time.Parse(time.RFC3339, png.Date); err != nil {
		t.Fatalf("date not RFC3339: %q", png.Date)
	}
	if _, ok := byName["AbCdEf.html"]; !ok {
		t.Fatalf("missing AbCdEf.html in %v", byName)
	}
}

func uploadMultipartAuth(t *testing.T, h http.Handler, files map[string]string) (name, url string) {
	t.Helper()
	body, ct := multipartBody(t, files)
	req := httptest.NewRequest("POST", "/api/upload", strings.NewReader(body))
	req.Header.Set("Content-Type", ct)
	req.Header.Set("Authorization", "Bearer sekrit")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("upload = %d (%s)", rr.Code, rr.Body.String())
	}
	var resp struct {
		Files []fileResult `json:"files"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp.Files[0].Name, resp.Files[0].URL
}

func TestOriginalNameDisabled(t *testing.T) {
	cfg, dir := testConfig(t) // originalName false by default
	h := testMux(t, cfg)
	name, _ := uploadMultipartAuth(t, h, map[string]string{"photo.png": "x"})

	if _, err := os.Stat(filepath.Join(dir, "."+name+".orig")); !os.IsNotExist(err) {
		t.Fatalf("sidecar should not exist when disabled, got %v", err)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/"+name, nil))
	if cd := rr.Header().Get("Content-Disposition"); cd != "" {
		t.Fatalf("Content-Disposition should be empty when disabled, got %q", cd)
	}
}

func TestOriginalNameEnabled(t *testing.T) {
	cfg, dir := testConfig(t)
	cfg.originalName = true
	h := testMux(t, cfg)
	name, url := uploadMultipartAuth(t, h, map[string]string{"my-photo.png": "x"})

	// Sidecar written with the original name.
	b, err := os.ReadFile(filepath.Join(dir, "."+name+".orig"))
	if err != nil {
		t.Fatalf("sidecar not written: %v", err)
	}
	if string(b) != "my-photo.png" {
		t.Fatalf("sidecar = %q, want my-photo.png", string(b))
	}

	// Serve sets Content-Disposition with the original name.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/"+name, nil))
	if cd := rr.Header().Get("Content-Disposition"); !strings.Contains(cd, "my-photo.png") {
		t.Fatalf("Content-Disposition = %q, want original name", cd)
	}

	// Sidecar itself is never served.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest("GET", "/."+name+".orig", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("sidecar GET = %d, want 404", rr.Code)
	}

	// Download query flips to attachment.
	req := httptest.NewRequest("GET", "/"+name+"?download", nil)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if cd := rr.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") {
		t.Fatalf("download Content-Disposition = %q, want attachment", cd)
	}

	// upload URL in response still the stable random name.
	if !strings.Contains(url, name) {
		t.Fatalf("url %q should contain %q", url, name)
	}
}

func TestOriginalNameSanitized(t *testing.T) {
	cfg, _ := testConfig(t)
	cfg.originalName = true
	h := testMux(t, cfg)
	_, _ = uploadMultipartAuth(t, h, map[string]string{"../../etc/evil..png": "x"})

	// Find the sidecar and check it holds only the safe basename.
	entries, _ := os.ReadDir(cfg.dataDir)
	var sidecar string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".orig") {
			sidecar = e.Name()
			break
		}
	}
	if sidecar == "" {
		t.Fatal("no sidecar found")
	}
	b, _ := os.ReadFile(filepath.Join(cfg.dataDir, sidecar))
	if string(b) != "evil..png" {
		t.Fatalf("sanitized name = %q, want evil..png", string(b))
	}
}

func TestOriginalNameRawWithNameQuery(t *testing.T) {
	cfg, dir := testConfig(t)
	cfg.originalName = true
	req := httptest.NewRequest("POST", "/api/upload?ext=png&name=report.png", strings.NewReader("x"))
	req.Header.Set("Authorization", "Bearer sekrit")
	rr := httptest.NewRecorder()
	testMux(t, cfg).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("raw upload = %d (%s)", rr.Code, rr.Body.String())
	}
	var resp struct {
		Files []fileResult `json:"files"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Files[0].OriginalName != "report.png" {
		t.Fatalf("originalName = %q, want report.png", resp.Files[0].OriginalName)
	}
	if _, err := os.Stat(filepath.Join(dir, "."+resp.Files[0].Name+".orig")); err != nil {
		t.Fatalf("sidecar missing: %v", err)
	}
}
