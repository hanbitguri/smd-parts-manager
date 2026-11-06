package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
	_ "modernc.org/sqlite"
)

const liveDBHash = "1e7e3aaa65c75fdd5cdb1e72233d5c34e6ae40c20a052b90490c57abbc058145"

func TestEmbeddedDBIsTheLiveInventory(t *testing.T) {
	b, err := assets.ReadFile(seedDBPath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != liveDBHash {
		t.Fatalf("data/smd_parts.db hash = %x", sum)
	}
	_, err = assets.ReadFile("seed/parts.json")
	if err == nil {
		t.Fatal("seed/parts.json must not be embedded")
	}
}

func TestEnsureCopiesOnceAndDoesNotOverwrite(t *testing.T) {
	t.Setenv("SMD_SEED_DB", filepath.Join(t.TempDir(), "missing.db"))
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "smd_parts.db")
	copied, err := ensureDBFile(path)
	if err != nil || !copied {
		t.Fatalf("copy: copied=%v err=%v", copied, err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE parts SET notes='keep-me' WHERE part_number=?`, "HK160827NJ-T"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	copied, err = ensureDBFile(path)
	if err != nil || copied {
		t.Fatalf("second ensure copied=%v err=%v", copied, err)
	}
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var notes string
	if err = db.QueryRow(`SELECT notes FROM parts WHERE part_number=?`, "HK160827NJ-T").Scan(&notes); err != nil {
		t.Fatal(err)
	}
	if notes != "keep-me" {
		t.Fatalf("existing row was replaced: %q", notes)
	}

	bad := filepath.Join(dir, "bad.db")
	if err = os.WriteFile(bad, []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err = ensureDBFile(bad); err != nil {
		t.Fatal(err)
	}
	db2, err := sql.Open("sqlite", bad)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	var n int
	if err = db2.QueryRow("SELECT COUNT(*) FROM parts").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != expectedParts {
		t.Fatalf("replaced invalid file count=%d", n)
	}
}

func testApp(t *testing.T) *App {
	t.Helper()
	t.Setenv("SMD_SEED_DB", "")
	path := filepath.Join(t.TempDir(), "smd_parts.db")
	if _, err := ensureDBFile(path); err != nil {
		t.Fatal(err)
	}
	app, err := openApp(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.db.Close() })
	return app
}

func TestLiveInventoryUnchangedOnOpen(t *testing.T) {
	app := testApp(t)
	n, summary, err := app.inventorySummary()
	if err != nil {
		t.Fatal(err)
	}
	if n != 103 {
		t.Fatalf("count=%d summary=%s", n, summary)
	}
	want := map[string]int{"칩 저항": 69, "칩캡(MLCC)": 26, "탄탈 커패시터": 5, "코일": 2, "다이오드": 1}
	for cat, skus := range want {
		rr := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/parts", nil)
		q := req.URL.Query()
		q.Set("category", cat)
		req.URL.RawQuery = q.Encode()
		app.listParts(rr, req)
		var ps []Part
		if err = json.Unmarshal(rr.Body.Bytes(), &ps); err != nil {
			t.Fatal(err)
		}
		if len(ps) != skus {
			t.Fatalf("%s: got %d want %d", cat, len(ps), skus)
		}
	}
	var pn, created string
	err = app.db.QueryRow("SELECT part_number, created_at FROM parts WHERE id=1").Scan(&pn, &created)
	if err != nil {
		t.Fatal(err)
	}
	if pn != "0402N180J500CT" || created != "2026-11-06T16:14:08+09:00" {
		t.Fatalf("first row changed: %s %s", pn, created)
	}
	for _, pn := range []string{"HK160827NJ-T", "MLG10055R27J", "없음"} {
		var id int
		err = app.db.QueryRow("SELECT id FROM parts WHERE part_number=?", pn).Scan(&id)
		if err != nil {
			t.Fatalf("missing live part %s: %v", pn, err)
		}
	}
	var seeded string
	if err = app.db.QueryRow("SELECT v FROM meta WHERE k='seeded'").Scan(&seeded); err != nil || seeded != "1" {
		t.Fatalf("meta seeded=%q err=%v", seeded, err)
	}
}

func TestHealthFiltersAndCRUD(t *testing.T) {
	app := testApp(t)
	rr := httptest.NewRecorder()
	app.health(rr, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var health struct {
		OK    bool `json:"ok"`
		Count int  `json:"count"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &health); err != nil || !health.OK || health.Count != 103 {
		t.Fatalf("health %d %s", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	app.listParts(rr, httptest.NewRequest(http.MethodGet, "/api/parts?search="+"HK160827NJ-T", nil))
	var found []Part
	if err := json.Unmarshal(rr.Body.Bytes(), &found); err != nil || len(found) != 1 || found[0].Category != "코일" {
		t.Fatalf("search coil: %s", rr.Body.String())
	}

	body := `{"category":"코일","package_size":"1005","value":"1uH","part_number":"__test_part__","qty":10,"unit_price":2.5,"notes":"temp"}`
	rr = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/parts", strings.NewReader(body))
	app.createPart(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatal(rr.Body.String())
	}
	var created Part
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.SupplyAmount != 25 || created.TaxIncluded != 28 {
		t.Fatalf("auto calc supply=%v tax=%v", created.SupplyAmount, created.TaxIncluded)
	}
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPut, "/api/parts/"+strconv.FormatInt(created.ID, 10), strings.NewReader(`{"category":"코일","package_size":"1005","value":"1uH","part_number":"__test_part__","qty":10,"unit_price":2.5,"supply_amount":30,"tax_included":33,"notes":"edited"}`))
	req.SetPathValue("id", strconv.FormatInt(created.ID, 10))
	app.updatePart(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatal(rr.Body.String())
	}
	var updated Part
	if err := json.Unmarshal(rr.Body.Bytes(), &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Notes != "edited" || updated.SupplyAmount != 30 || updated.CreatedAt == "" {
		t.Fatalf("update %+v", updated)
	}
	rr = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/api/parts/"+strconv.FormatInt(created.ID, 10), nil)
	req.SetPathValue("id", strconv.FormatInt(created.ID, 10))
	app.deletePart(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatal(rr.Body.String())
	}
	var n int
	if err := app.db.QueryRow("SELECT COUNT(*) FROM parts").Scan(&n); err != nil || n != 103 {
		t.Fatalf("count after delete=%d err=%v", n, err)
	}
}

func TestExportAndImportRoundTrip(t *testing.T) {
	app := testApp(t)
	rr := httptest.NewRecorder()
	app.export(rr, httptest.NewRequest(http.MethodGet, "/api/export?format=json", nil))
	ps, err := parseJSON(bytes.NewReader(rr.Body.Bytes()))
	if err != nil || len(ps) != 103 {
		t.Fatalf("json export %d %v", len(ps), err)
	}
	rr = httptest.NewRecorder()
	app.export(rr, httptest.NewRequest(http.MethodGet, "/api/export?format=csv", nil))
	csvParts, err := parseCSV(bytes.NewReader(rr.Body.Bytes()))
	if err != nil || len(csvParts) != 103 {
		t.Fatalf("csv export %d %v", len(csvParts), err)
	}
	rr = httptest.NewRecorder()
	app.export(rr, httptest.NewRequest(http.MethodGet, "/api/export?format=xlsx", nil))
	f, err := excelize.OpenReader(bytes.NewReader(rr.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := f.GetRows(f.GetSheetList()[0])
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	sheetParts := parseSheet(rows)
	if len(sheetParts) != 103 {
		t.Fatalf("xlsx rows=%d", len(sheetParts))
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("file", "smd_parts.json")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(ps)
	_, _ = fw.Write(raw)
	_ = w.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/import", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	rr = httptest.NewRecorder()
	app.importFile(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"imported":103`) {
		t.Fatalf("import %d %s", rr.Code, rr.Body.String())
	}
	var n int
	if err = app.db.QueryRow("SELECT COUNT(*) FROM parts").Scan(&n); err != nil || n != 103 {
		t.Fatalf("count after import=%d", n)
	}
}

func TestDecideBrowser(t *testing.T) {
	env := map[string]string{}
	get := func(k string) string { return env[k] }
	if decideBrowser(get, "linux") {
		t.Fatal("linux should not auto-open")
	}
	if !decideBrowser(get, "windows") || !decideBrowser(get, "darwin") {
		t.Fatal("desktop should auto-open when unset")
	}
	env["RENDER"] = "true"
	if decideBrowser(get, "windows") {
		t.Fatal("cloud must not open a browser")
	}
	env["SMD_OPEN_BROWSER"] = "1"
	if decideBrowser(get, "windows") {
		t.Fatal("cloud ignores browser opt-in")
	}
	env = map[string]string{"FLY_APP_NAME": "smd"}
	if decideBrowser(get, "darwin") {
		t.Fatal("fly must not open a browser")
	}
	env = map[string]string{"SMD_OPEN_BROWSER": "0"}
	if decideBrowser(get, "windows") {
		t.Fatal("explicit off")
	}
	env = map[string]string{"SMD_OPEN_BROWSER": "yes"}
	if !decideBrowser(get, "linux") {
		t.Fatal("explicit on for local linux")
	}
}

func TestPageContainsCategories(t *testing.T) {
	app := testApp(t)
	rr := httptest.NewRecorder()
	app.page(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rr.Body.String()
	for _, s := range []string{"코일", "다이오드", "칩캡(MLCC)", "탄탈 커패시터", "4000", "xlsx", "csv", "json"} {
		if !strings.Contains(body, s) {
			t.Fatalf("page missing %s", s)
		}
	}
}
