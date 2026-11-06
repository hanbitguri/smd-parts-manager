package main

import (
	"context"
	"database/sql"
	"embed"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xuri/excelize/v2"
	_ "modernc.org/sqlite"
)

//go:embed static/index.html data/smd_parts.db
var assets embed.FS

// Shipped inventory. Tests lock these figures so a reseed cannot slip through.
const (
	expectedParts = 103
	seedDBPath    = "data/smd_parts.db"
)

type Part struct {
	ID           int64   `json:"id"`
	Category     string  `json:"category"`
	PackageSize  string  `json:"package_size"`
	Value        string  `json:"value"`
	Tolerance    string  `json:"tolerance"`
	Voltage      string  `json:"voltage"`
	Dielectric   string  `json:"dielectric"`
	PartNumber   string  `json:"part_number"`
	Qty          int64   `json:"qty"`
	UnitPrice    float64 `json:"unit_price"`
	SupplyAmount float64 `json:"supply_amount"`
	TaxIncluded  float64 `json:"tax_included"`
	OriginalSpec string  `json:"original_spec"`
	Notes        string  `json:"notes"`
	CreatedAt    string  `json:"created_at"`
	UpdatedAt    string  `json:"updated_at"`
}

type App struct {
	db *sql.DB
	mu sync.Mutex
}

const cols = `id,category,package_size,value,tolerance,voltage,dielectric,part_number,qty,unit_price,supply_amount,tax_included,original_spec,notes,created_at,updated_at`

var allowedSort = map[string]bool{
	"id": true, "category": true, "package_size": true, "value": true,
	"part_number": true, "qty": true, "unit_price": true,
	"supply_amount": true, "tax_included": true, "updated_at": true,
}

var categoryOrder = []string{"칩 저항", "칩캡(MLCC)", "탄탈 커패시터", "코일", "다이오드"}

func main() {
	path := dbPath()
	copied, err := ensureDBFile(path)
	if err != nil {
		log.Fatal(err)
	}
	app, err := openApp(path)
	if err != nil {
		log.Fatal(err)
	}
	defer app.db.Close()

	n, summary, err := app.inventorySummary()
	if err != nil {
		log.Fatal(err)
	}
	if copied {
		log.Printf("시드 데이터베이스를 처음 복사했습니다: %s", path)
	} else {
		log.Printf("기존 데이터베이스를 그대로 엽니다: %s", path)
	}
	log.Printf("부품 %d개 (%s). seed/parts.json 은 읽지 않습니다.", n, summary)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", app.page)
	mux.HandleFunc("GET /favicon.ico", favicon)
	mux.HandleFunc("GET /healthz", app.health)
	mux.HandleFunc("GET /api/parts", app.listParts)
	mux.HandleFunc("POST /api/parts", app.createPart)
	mux.HandleFunc("GET /api/parts/{id}", app.getPart)
	mux.HandleFunc("PUT /api/parts/{id}", app.updatePart)
	mux.HandleFunc("DELETE /api/parts/{id}", app.deletePart)
	mux.HandleFunc("GET /api/stats", app.stats)
	mux.HandleFunc("GET /api/facets", app.facets)
	mux.HandleFunc("GET /api/export", app.export)
	mux.HandleFunc("POST /api/import", app.importFile)

	addr := listenAddr()
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	log.Printf("SMD 부품 관리 서버: http://0.0.0.0:%s", port)

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       120 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       180 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), shutdownSignals()...)
	defer stop()
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut)
	}()
	if decideBrowser(os.Getenv, runtime.GOOS) {
		go openBrowser("http://127.0.0.1:" + port)
	} else {
		log.Printf("브라우저는 자동으로 열지 않습니다.")
	}
	if err = srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

func dbPath() string {
	if p := strings.TrimSpace(os.Getenv("SMD_DB")); p != "" {
		return p
	}
	return "./data/smd_parts.db"
}

func listenAddr() string {
	port := strings.TrimSpace(os.Getenv("PORT"))
	if port == "" {
		port = "8080"
	}
	port = strings.TrimPrefix(port, ":")
	if _, err := strconv.Atoi(port); err != nil {
		log.Fatalf("PORT 값이 잘못되었습니다: %q", port)
	}
	return "0.0.0.0:" + port
}

func cloudEnv(getenv func(string) string) bool {
	keys := []string{
		"RENDER", "RENDER_SERVICE_ID", "RENDER_EXTERNAL_URL",
		"FLY_APP_NAME", "K_SERVICE", "DYNO", "RAILWAY_ENVIRONMENT",
		"CF_INSTANCE_GUID", "WEBSITE_INSTANCE_ID", "AWS_EXECUTION_ENV",
		"NOMAD_ALLOC_ID", "KUBERNETES_SERVICE_HOST",
	}
	for _, k := range keys {
		if strings.TrimSpace(getenv(k)) != "" {
			return true
		}
	}
	return false
}

func decideBrowser(getenv func(string) string, goos string) bool {
	if cloudEnv(getenv) {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(getenv("SMD_OPEN_BROWSER"))) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	}
	return goos == "windows" || goos == "darwin"
}

func openBrowser(u string) {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		c = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	case "darwin":
		c = exec.Command("open", u)
	default:
		c = exec.Command("xdg-open", u)
	}
	_ = c.Start()
}

// ensureDBFile copies the shipped SQLite database only when the target is
// missing or not a SQLite file. An existing database is never overwritten,
// and seed/parts.json is never read.
func ensureDBFile(path string) (copied bool, err error) {
	if sqliteFile(path) {
		return false, nil
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	b, err := seedBytes()
	if err != nil {
		return false, err
	}
	if err = writeAtomic(path, b); err != nil {
		return false, err
	}
	return true, nil
}

func seedBytes() ([]byte, error) {
	if p := strings.TrimSpace(os.Getenv("SMD_SEED_DB")); p != "" {
		b, err := os.ReadFile(p)
		if err == nil && len(b) > 0 {
			return b, nil
		}
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	b, err := assets.ReadFile(seedDBPath)
	if err != nil {
		return nil, fmt.Errorf("시드 데이터베이스를 찾을 수 없습니다: %w", err)
	}
	return b, nil
}

func writeAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func sqliteFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	buf := make([]byte, 16)
	n, _ := io.ReadFull(f, buf)
	return n >= 15 && string(buf[:15]) == "SQLite format 3"
}

func sqliteDSN(path string) string {
	return path + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)"
}

func openApp(path string) (*App, error) {
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	app := &App{db: db}
	if err = app.init(); err != nil {
		db.Close()
		return nil, err
	}
	return app, nil
}

func (a *App) init() error {
	_, err := a.db.Exec(`
CREATE TABLE IF NOT EXISTS parts(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  category TEXT NOT NULL DEFAULT '',
  package_size TEXT NOT NULL DEFAULT '',
  value TEXT NOT NULL DEFAULT '',
  tolerance TEXT NOT NULL DEFAULT '',
  voltage TEXT NOT NULL DEFAULT '',
  dielectric TEXT NOT NULL DEFAULT '',
  part_number TEXT NOT NULL UNIQUE,
  qty INTEGER NOT NULL DEFAULT 0,
  unit_price REAL NOT NULL DEFAULT 0,
  supply_amount REAL NOT NULL DEFAULT 0,
  tax_included REAL NOT NULL DEFAULT 0,
  original_spec TEXT NOT NULL DEFAULT '',
  notes TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS meta(k TEXT PRIMARY KEY, v TEXT);`)
	return err
}

func (a *App) inventorySummary() (int, string, error) {
	var n int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM parts").Scan(&n); err != nil {
		return 0, "", err
	}
	rows, err := a.db.Query("SELECT category, COUNT(*) FROM parts GROUP BY category")
	if err != nil {
		return n, "", err
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var c string
		var k int
		if err = rows.Scan(&c, &k); err != nil {
			return n, "", err
		}
		counts[c] = k
	}
	names := make([]string, 0, len(counts))
	for c := range counts {
		names = append(names, c)
	}
	names = sortCategories(names)
	parts := make([]string, 0, len(names))
	for _, c := range names {
		parts = append(parts, fmt.Sprintf("%s %d", c, counts[c]))
	}
	return n, strings.Join(parts, ", "), rows.Err()
}

type saver interface {
	Exec(string, ...any) (sql.Result, error)
}

func save(s saver, p *Part, update bool) error {
	p.Category = strings.TrimSpace(p.Category)
	p.PackageSize = strings.TrimSpace(p.PackageSize)
	p.Value = strings.TrimSpace(p.Value)
	p.Tolerance = strings.TrimSpace(p.Tolerance)
	p.Voltage = strings.TrimSpace(p.Voltage)
	p.Dielectric = strings.TrimSpace(p.Dielectric)
	p.PartNumber = strings.TrimSpace(p.PartNumber)
	p.OriginalSpec = strings.TrimSpace(p.OriginalSpec)
	p.Notes = strings.TrimSpace(p.Notes)
	if p.PartNumber == "" {
		return fmt.Errorf("품번은 필수입니다.")
	}
	if p.Qty < 0 || p.UnitPrice < 0 || p.SupplyAmount < 0 || p.TaxIncluded < 0 {
		return fmt.Errorf("수량과 금액은 0 이상이어야 합니다.")
	}
	if p.SupplyAmount == 0 {
		p.SupplyAmount = float64(p.Qty) * p.UnitPrice
	}
	p.SupplyAmount = math.Round(p.SupplyAmount*100) / 100
	if p.TaxIncluded == 0 {
		p.TaxIncluded = math.Round(p.SupplyAmount * 1.1)
	}
	now := time.Now().Format(time.RFC3339)
	if strings.TrimSpace(p.CreatedAt) == "" {
		p.CreatedAt = now
	}
	p.UpdatedAt = now
	q := `INSERT INTO parts(category,package_size,value,tolerance,voltage,dielectric,part_number,qty,unit_price,supply_amount,tax_included,original_spec,notes,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`
	args := []any{p.Category, p.PackageSize, p.Value, p.Tolerance, p.Voltage, p.Dielectric, p.PartNumber, p.Qty, p.UnitPrice, p.SupplyAmount, p.TaxIncluded, p.OriginalSpec, p.Notes, p.CreatedAt, p.UpdatedAt}
	if update {
		q = `UPDATE parts SET category=?,package_size=?,value=?,tolerance=?,voltage=?,dielectric=?,part_number=?,qty=?,unit_price=?,supply_amount=?,tax_included=?,original_spec=?,notes=?,created_at=?,updated_at=? WHERE id=?`
		args = append(args, p.ID)
	}
	res, err := s.Exec(q, args...)
	if err != nil {
		return friendly(err, p.PartNumber)
	}
	if !update {
		if id, err := res.LastInsertId(); err == nil {
			p.ID = id
		}
	}
	return nil
}

func friendly(err error, partNumber string) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "UNIQUE") {
		return fmt.Errorf("이미 등록된 품번입니다: %s", partNumber)
	}
	return err
}

func jsonOut(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func errOut(w http.ResponseWriter, err error) {
	jsonOut(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
}

func favicon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32"><rect width="32" height="32" rx="6" fill="#1d3f86"/><text x="16" y="22" text-anchor="middle" font-size="14" fill="white" font-family="sans-serif">S</text></svg>`))
}

func (a *App) page(w http.ResponseWriter, r *http.Request) {
	b, err := assets.ReadFile("static/index.html")
	if err != nil {
		http.Error(w, "화면을 불러오지 못했습니다.", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
}

func (a *App) health(w http.ResponseWriter, r *http.Request) {
	var n int
	if err := a.db.QueryRow("SELECT COUNT(*) FROM parts").Scan(&n); err != nil {
		jsonOut(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{"ok": true, "count": n})
}

type listQuery struct {
	search, category, size, sort, dir string
}

func (a *App) queryParts(q listQuery) ([]Part, error) {
	where := []string{}
	args := []any{}
	if q.category != "" {
		where = append(where, "category=?")
		args = append(args, q.category)
	}
	if q.size != "" {
		where = append(where, "package_size=?")
		args = append(args, q.size)
	}
	if s := strings.TrimSpace(q.search); s != "" {
		like := "%" + escapeLike(s) + "%"
		where = append(where, `(part_number LIKE ? ESCAPE '\' OR value LIKE ? ESCAPE '\' OR original_spec LIKE ? ESCAPE '\' OR category LIKE ? ESCAPE '\' OR notes LIKE ? ESCAPE '\' OR package_size LIKE ? ESCAPE '\' OR dielectric LIKE ? ESCAPE '\')`)
		for i := 0; i < 7; i++ {
			args = append(args, like)
		}
	}
	sqlq := "SELECT " + cols + " FROM parts"
	if len(where) > 0 {
		sqlq += " WHERE " + strings.Join(where, " AND ")
	}
	col := q.sort
	if !allowedSort[col] {
		col = "id"
	}
	dir := "ASC"
	if strings.EqualFold(q.dir, "desc") {
		dir = "DESC"
	}
	sqlq += " ORDER BY " + col + " " + dir + ", id ASC"
	rows, err := a.db.Query(sqlq, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Part{}
	for rows.Next() {
		p, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

func scan(s interface{ Scan(...any) error }) (Part, error) {
	var p Part
	err := s.Scan(&p.ID, &p.Category, &p.PackageSize, &p.Value, &p.Tolerance, &p.Voltage, &p.Dielectric, &p.PartNumber, &p.Qty, &p.UnitPrice, &p.SupplyAmount, &p.TaxIncluded, &p.OriginalSpec, &p.Notes, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

func (a *App) listParts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ps, err := a.queryParts(listQuery{
		search:   q.Get("search"),
		category: q.Get("category"),
		size:     q.Get("package_size"),
		sort:     q.Get("sort"),
		dir:      q.Get("dir"),
	})
	if err != nil {
		errOut(w, err)
		return
	}
	jsonOut(w, http.StatusOK, ps)
}

func (a *App) createPart(w http.ResponseWriter, r *http.Request) {
	p, err := decodePart(r)
	if err != nil {
		errOut(w, err)
		return
	}
	a.mu.Lock()
	err = save(a.db, &p, false)
	a.mu.Unlock()
	if err != nil {
		errOut(w, err)
		return
	}
	jsonOut(w, http.StatusCreated, p)
}

func (a *App) partID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("잘못된 ID")
	}
	return id, nil
}

func (a *App) getPart(w http.ResponseWriter, r *http.Request) {
	id, err := a.partID(r)
	if err != nil {
		errOut(w, err)
		return
	}
	p, err := scan(a.db.QueryRow("SELECT "+cols+" FROM parts WHERE id=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		jsonOut(w, http.StatusNotFound, map[string]string{"error": "부품을 찾을 수 없습니다."})
		return
	}
	if err != nil {
		errOut(w, err)
		return
	}
	jsonOut(w, http.StatusOK, p)
}

func (a *App) updatePart(w http.ResponseWriter, r *http.Request) {
	id, err := a.partID(r)
	if err != nil {
		errOut(w, err)
		return
	}
	p, err := decodePart(r)
	if err != nil {
		errOut(w, err)
		return
	}
	old, err := scan(a.db.QueryRow("SELECT "+cols+" FROM parts WHERE id=?", id))
	if errors.Is(err, sql.ErrNoRows) {
		jsonOut(w, http.StatusNotFound, map[string]string{"error": "부품을 찾을 수 없습니다."})
		return
	}
	if err != nil {
		errOut(w, err)
		return
	}
	p.ID = id
	p.CreatedAt = old.CreatedAt
	a.mu.Lock()
	err = save(a.db, &p, true)
	a.mu.Unlock()
	if err != nil {
		errOut(w, err)
		return
	}
	jsonOut(w, http.StatusOK, p)
}

func (a *App) deletePart(w http.ResponseWriter, r *http.Request) {
	id, err := a.partID(r)
	if err != nil {
		errOut(w, err)
		return
	}
	a.mu.Lock()
	res, err := a.db.Exec("DELETE FROM parts WHERE id=?", id)
	a.mu.Unlock()
	if err != nil {
		errOut(w, err)
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		jsonOut(w, http.StatusNotFound, map[string]string{"error": "부품을 찾을 수 없습니다."})
		return
	}
	jsonOut(w, http.StatusOK, map[string]bool{"ok": true})
}

func decodePart(r *http.Request) (Part, error) {
	defer r.Body.Close()
	var p Part
	err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&p)
	if err != nil {
		return Part{}, fmt.Errorf("요청 본문을 해석하지 못했습니다.")
	}
	return p, nil
}

func (a *App) stats(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query(`SELECT category, COUNT(*), COALESCE(SUM(qty),0), COALESCE(SUM(supply_amount),0) FROM parts GROUP BY category`)
	if err != nil {
		errOut(w, err)
		return
	}
	defer rows.Close()
	type row struct {
		Category string  `json:"category"`
		SKUs     int64   `json:"skus"`
		Qty      int64   `json:"qty"`
		Amount   float64 `json:"amount"`
	}
	out := []row{}
	for rows.Next() {
		var s row
		if err = rows.Scan(&s.Category, &s.SKUs, &s.Qty, &s.Amount); err != nil {
			errOut(w, err)
			return
		}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return categoryRank(out[i].Category) < categoryRank(out[j].Category)
	})
	jsonOut(w, http.StatusOK, out)
}

func (a *App) facets(w http.ResponseWriter, r *http.Request) {
	cats, err := a.distinct("category")
	if err != nil {
		errOut(w, err)
		return
	}
	sizes, err := a.distinct("package_size")
	if err != nil {
		errOut(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]any{
		"categories":    sortCategories(cats),
		"package_sizes": sizes,
	})
}

func (a *App) distinct(col string) ([]string, error) {
	switch col {
	case "category", "package_size":
	default:
		return nil, fmt.Errorf("잘못된 필드")
	}
	rows, err := a.db.Query("SELECT DISTINCT " + col + " FROM parts WHERE " + col + " != '' ORDER BY " + col)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s string
		if err = rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func categoryRank(c string) int {
	for i, name := range categoryOrder {
		if c == name {
			return i
		}
	}
	return len(categoryOrder) + 1
}

func sortCategories(in []string) []string {
	sort.SliceStable(in, func(i, j int) bool {
		ri, rj := categoryRank(in[i]), categoryRank(in[j])
		if ri != rj {
			return ri < rj
		}
		return in[i] < in[j]
	})
	return in
}

func (a *App) export(w http.ResponseWriter, r *http.Request) {
	ps, err := a.queryParts(listQuery{sort: "id", dir: "asc"})
	if err != nil {
		errOut(w, err)
		return
	}
	format := strings.ToLower(r.URL.Query().Get("format"))
	switch format {
	case "", "csv":
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", "attachment; filename=smd_parts.csv")
		w.Write([]byte{0xEF, 0xBB, 0xBF})
		cw := csv.NewWriter(w)
		_ = cw.Write([]string{"id", "분류", "크기/케이스", "값", "허용오차", "정격전압", "유전체/재질", "품번", "수량", "개당단가", "공급가액", "부가세포함", "원문규격", "비고", "생성일", "수정일"})
		for _, p := range ps {
			_ = cw.Write([]string{
				strconv.FormatInt(p.ID, 10), p.Category, p.PackageSize, p.Value, p.Tolerance, p.Voltage, p.Dielectric, p.PartNumber,
				strconv.FormatInt(p.Qty, 10), fmt.Sprint(p.UnitPrice), fmt.Sprint(p.SupplyAmount), fmt.Sprint(p.TaxIncluded),
				p.OriginalSpec, p.Notes, p.CreatedAt, p.UpdatedAt,
			})
		}
		cw.Flush()
	case "json":
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", "attachment; filename=smd_parts.json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(ps)
	case "xlsx", "excel":
		w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
		w.Header().Set("Content-Disposition", "attachment; filename=smd_parts.xlsx")
		if err = writeXLSX(w, ps); err != nil {
			errOut(w, err)
		}
	default:
		errOut(w, fmt.Errorf("format 은 csv, xlsx, json 만 지원합니다."))
	}
}

func writeXLSX(w io.Writer, parts []Part) error {
	f := excelize.NewFile()
	defer f.Close()
	const sheet = "부품"
	if err := f.SetSheetName("Sheet1", sheet); err != nil {
		return err
	}
	headers := []string{"분류", "크기/케이스", "값", "허용오차", "정격전압", "유전체/재질", "품번", "수량", "개당단가", "공급가액", "부가세포함", "원문규격", "비고"}
	for i, h := range headers {
		cell, _ := excelize.CoordinatesToCellName(i+1, 1)
		if err := f.SetCellValue(sheet, cell, h); err != nil {
			return err
		}
	}
	for r, p := range parts {
		vals := []any{p.Category, p.PackageSize, p.Value, p.Tolerance, p.Voltage, p.Dielectric, p.PartNumber, p.Qty, p.UnitPrice, p.SupplyAmount, p.TaxIncluded, p.OriginalSpec, p.Notes}
		for c, v := range vals {
			cell, _ := excelize.CoordinatesToCellName(c+1, r+2)
			if err := f.SetCellValue(sheet, cell, v); err != nil {
				return err
			}
		}
	}
	return f.Write(w)
}

func (a *App) importFile(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 32<<20)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		errOut(w, fmt.Errorf("파일을 읽지 못했습니다."))
		return
	}
	fh, hdr, err := r.FormFile("file")
	if err != nil {
		errOut(w, fmt.Errorf("file 항목이 필요합니다."))
		return
	}
	defer fh.Close()
	ext := strings.ToLower(filepath.Ext(hdr.Filename))
	var ps []Part
	switch ext {
	case ".xlsx":
		ps, err = parseXLSX(fh)
	case ".csv":
		ps, err = parseCSV(fh)
	case ".json":
		ps, err = parseJSON(fh)
	default:
		err = fmt.Errorf("XLSX, CSV, JSON 파일만 지원합니다.")
	}
	if err != nil {
		errOut(w, err)
		return
	}
	n, err := a.upsertAll(ps)
	if err != nil {
		errOut(w, err)
		return
	}
	jsonOut(w, http.StatusOK, map[string]int{"imported": n})
}

func (a *App) upsertAll(ps []Part) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	tx, err := a.db.Begin()
	if err != nil {
		return 0, err
	}
	n := 0
	for i := range ps {
		p := &ps[i]
		p.PartNumber = strings.TrimSpace(p.PartNumber)
		if p.PartNumber == "" {
			continue
		}
		old, err := scan(tx.QueryRow("SELECT "+cols+" FROM parts WHERE part_number=?", p.PartNumber))
		if err == nil {
			p.ID = old.ID
			p.CreatedAt = old.CreatedAt
			err = save(tx, p, true)
		} else if errors.Is(err, sql.ErrNoRows) {
			p.ID = 0
			p.CreatedAt = ""
			err = save(tx, p, false)
		}
		if err != nil {
			_ = tx.Rollback()
			return 0, fmt.Errorf("%s: %w", p.PartNumber, err)
		}
		n++
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

func parseXLSX(r io.Reader) ([]Part, error) {
	f, err := excelize.OpenReader(r)
	if err != nil {
		return nil, fmt.Errorf("엑셀 파일을 열지 못했습니다.")
	}
	defer f.Close()
	sheets := f.GetSheetList()
	if len(sheets) == 0 {
		return nil, fmt.Errorf("시트가 없습니다.")
	}
	name := "부품 규격 전체목록"
	if !contains(sheets, name) {
		name = sheets[0]
	}
	rows, err := f.GetRows(name)
	if err != nil {
		return nil, err
	}
	return parseSheet(rows), nil
}

func parseSheet(rows [][]string) []Part {
	header := -1
	start := 1
	switch {
	case len(rows) > 0 && strings.Contains(strings.Join(rows[0], " "), "품번"):
		header = 0
		start = 1
	case len(rows) > 4 && strings.Contains(strings.Join(rows[4], " "), "품번"):
		header = 4
		start = 5
	default:
		for i := 0; i < len(rows) && i < 6; i++ {
			if strings.Contains(strings.Join(rows[i], " "), "품번") {
				header = i
				start = i + 1
				break
			}
		}
	}
	notesAt := -1
	if header >= 0 {
		h := strings.TrimSpace(cell(rows[header], 12))
		if h == "비고" || strings.EqualFold(h, "notes") {
			notesAt = 12
		}
	}
	ps := []Part{}
	for i := start; i < len(rows); i++ {
		row := rows[i]
		if len(row) < 7 || strings.TrimSpace(cell(row, 6)) == "" {
			continue
		}
		p := rowPart(row)
		if notesAt >= 0 {
			p.Notes = cell(row, notesAt)
		}
		ps = append(ps, p)
	}
	return ps
}

func parseCSV(r io.Reader) ([]Part, error) {
	b, err := io.ReadAll(io.LimitReader(r, 32<<20))
	if err != nil {
		return nil, err
	}
	b = bytesTrimBOM(b)
	cr := csv.NewReader(strings.NewReader(string(b)))
	cr.FieldsPerRecord = -1
	rows, err := cr.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("CSV를 해석하지 못했습니다.")
	}
	ps := []Part{}
	for i, row := range rows {
		if i == 0 && strings.Contains(strings.Join(row, " "), "품번") {
			continue
		}
		if strings.TrimSpace(strings.Join(row, "")) == "" {
			continue
		}
		p := csvPart(row)
		if strings.TrimSpace(p.PartNumber) == "" {
			continue
		}
		ps = append(ps, p)
	}
	return ps, nil
}

func parseJSON(r io.Reader) ([]Part, error) {
	b, err := io.ReadAll(io.LimitReader(r, 32<<20))
	if err != nil {
		return nil, err
	}
	var ps []Part
	if err = json.Unmarshal(b, &ps); err != nil {
		return nil, fmt.Errorf("JSON 배열을 해석하지 못했습니다.")
	}
	return ps, nil
}

func bytesTrimBOM(b []byte) []byte {
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		return b[3:]
	}
	return b
}

func contains(a []string, s string) bool {
	for _, v := range a {
		if v == s {
			return true
		}
	}
	return false
}

func cell(r []string, i int) string {
	if i < 0 || i >= len(r) {
		return ""
	}
	return strings.TrimSpace(r[i])
}

func num(s string) float64 {
	v, _ := strconv.ParseFloat(strings.ReplaceAll(s, ",", ""), 64)
	return v
}

func rowPart(r []string) Part {
	return Part{
		Category: cell(r, 0), PackageSize: cell(r, 1), Value: cell(r, 2), Tolerance: cell(r, 3),
		Voltage: cell(r, 4), Dielectric: cell(r, 5), PartNumber: cell(r, 6),
		Qty: int64(num(cell(r, 7))), UnitPrice: num(cell(r, 8)), SupplyAmount: num(cell(r, 9)),
		TaxIncluded: num(cell(r, 10)), OriginalSpec: cell(r, 11),
	}
}

func csvPart(r []string) Part {
	offset := 0
	if len(r) >= 16 {
		offset = 1
	}
	return Part{
		Category: cell(r, offset), PackageSize: cell(r, 1+offset), Value: cell(r, 2+offset),
		Tolerance: cell(r, 3+offset), Voltage: cell(r, 4+offset), Dielectric: cell(r, 5+offset),
		PartNumber: cell(r, 6+offset), Qty: int64(num(cell(r, 7+offset))), UnitPrice: num(cell(r, 8+offset)),
		SupplyAmount: num(cell(r, 9+offset)), TaxIncluded: num(cell(r, 10+offset)),
		OriginalSpec: cell(r, 11+offset), Notes: cell(r, 12+offset),
	}
}
