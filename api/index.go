// Single Vercel Go handler — routes all /api/* requests internally.
// Everything lives here: DB, models, helpers, all 4 endpoints.
// This avoids Vercel's inability to resolve local internal packages.

package handler

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

// ─────────────────────────────────────────────────────────────────────────────
// DATABASE — lazy singleton, safe for Vercel serverless cold starts
// ─────────────────────────────────────────────────────────────────────────────

var (
	db     *sql.DB
	dbErr  error
	dbOnce sync.Once
)

func getDB() (*sql.DB, error) {
	dbOnce.Do(func() {
		dsn := os.Getenv("DATABASE_URL")
		if dsn == "" {
			dbErr = fmt.Errorf("DATABASE_URL is not set")
			return
		}
		var err error
		db, err = sql.Open("postgres", dsn)
		if err != nil {
			dbErr = fmt.Errorf("DB open: %v", err)
			return
		}
		db.SetMaxOpenConns(5)
		db.SetMaxIdleConns(2)
		db.SetConnMaxLifetime(5 * time.Minute)
		if err = db.Ping(); err != nil {
			dbErr = fmt.Errorf("DB ping: %v", err)
			return
		}
		_, err = db.Exec(`
			CREATE TABLE IF NOT EXISTS profiles (
				id                  TEXT PRIMARY KEY,
				name                TEXT UNIQUE NOT NULL,
				gender              TEXT,
				gender_probability  DOUBLE PRECISION,
				sample_size         INTEGER,
				age                 INTEGER,
				age_group           TEXT,
				country_id          TEXT,
				country_probability DOUBLE PRECISION,
				created_at          TEXT NOT NULL
			)
		`)
		if err != nil {
			dbErr = fmt.Errorf("create table: %v", err)
		}
	})
	return db, dbErr
}

// ─────────────────────────────────────────────────────────────────────────────
// MODELS
// ─────────────────────────────────────────────────────────────────────────────

type Profile struct {
	ID                 string  `json:"id"`
	Name               string  `json:"name"`
	Gender             string  `json:"gender"`
	GenderProbability  float64 `json:"gender_probability"`
	SampleSize         int     `json:"sample_size"`
	Age                int     `json:"age"`
	AgeGroup           string  `json:"age_group"`
	CountryID          string  `json:"country_id"`
	CountryProbability float64 `json:"country_probability"`
	CreatedAt          string  `json:"created_at"`
}

type ProfileSummary struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Gender    string `json:"gender"`
	Age       int    `json:"age"`
	AgeGroup  string `json:"age_group"`
	CountryID string `json:"country_id"`
}

// ─────────────────────────────────────────────────────────────────────────────
// EXTERNAL API RESPONSE TYPES
// ─────────────────────────────────────────────────────────────────────────────

type genderizeResp struct {
	Gender      *string `json:"gender"`
	Probability float64 `json:"probability"`
	Count       int     `json:"count"`
}

type agifyResp struct {
	Age *int `json:"age"`
}

type nationalizeCountry struct {
	CountryID   string  `json:"country_id"`
	Probability float64 `json:"probability"`
}

type nationalizeResp struct {
	Country []nationalizeCountry `json:"country"`
}

// ─────────────────────────────────────────────────────────────────────────────
// HELPERS
// ─────────────────────────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func errJSON(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"status": "error", "message": message})
}

func errUpstream(w http.ResponseWriter, apiName string) {
	writeJSON(w, http.StatusBadGateway, map[string]string{
		"status":  "502",
		"message": apiName + " returned an invalid response",
	})
}

func classifyAge(age int) string {
	switch {
	case age <= 12:
		return "child"
	case age <= 19:
		return "teenager"
	case age <= 59:
		return "adult"
	default:
		return "senior"
	}
}

func isValidName(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}

func fetchJSON(url string, v any) error {
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read failed: %w", err)
	}
	return json.Unmarshal(body, v)
}

// ─────────────────────────────────────────────────────────────────────────────
// CONCURRENT ENRICHMENT — calls all 3 APIs in parallel
// ─────────────────────────────────────────────────────────────────────────────

type enrichResult struct {
	genderize *genderizeResp
	agify     *agifyResp
	national  *nationalizeResp
	errors    map[string]error
}

func enrichName(name string) enrichResult {
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		result enrichResult
	)
	result.errors = make(map[string]error)

	wg.Add(1)
	go func() {
		defer wg.Done()
		var g genderizeResp
		err := fetchJSON("https://api.genderize.io?name="+name, &g)
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			result.errors["Genderize"] = err
		} else {
			result.genderize = &g
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		var a agifyResp
		err := fetchJSON("https://api.agify.io?name="+name, &a)
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			result.errors["Agify"] = err
		} else {
			result.agify = &a
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		var n nationalizeResp
		err := fetchJSON("https://api.nationalize.io?name="+name, &n)
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			result.errors["Nationalize"] = err
		} else {
			result.national = &n
		}
	}()

	wg.Wait()
	return result
}

// ─────────────────────────────────────────────────────────────────────────────
// CORS MIDDLEWARE
// ─────────────────────────────────────────────────────────────────────────────

func withCORS(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// VERCEL ENTRYPOINT — routes all requests based on method + path
// ─────────────────────────────────────────────────────────────────────────────

// Handler is called by Vercel for every request to /api/index
func Handler(w http.ResponseWriter, r *http.Request) {
	withCORS(router)(w, r)
}

func router(w http.ResponseWriter, r *http.Request) {
	// Vercel preserves the original request path even after rewriting.
	// We check both r.URL.Path and the id query param Vercel injects
	// for dynamic segments like /api/profiles/:id
	path := strings.TrimRight(r.URL.Path, "/")

	// When Vercel rewrites /api/profiles/:id → /api/index,
	// it injects the :id segment as query param "id"
	idFromQuery := r.URL.Query().Get("id")

	switch {
	// /api/profiles — list or create
	case path == "/api/profiles" || path == "/api/index" && idFromQuery == "":
		switch r.Method {
		case http.MethodPost:
			handleCreate(w, r)
		case http.MethodGet:
			handleList(w, r)
		default:
			errJSON(w, http.StatusMethodNotAllowed, "Method not allowed")
		}

	// /api/profiles/{id} — Vercel injects id as query param
	case idFromQuery != "":
		switch r.Method {
		case http.MethodGet:
			handleGetByID(w, r, idFromQuery)
		case http.MethodDelete:
			handleDelete(w, r, idFromQuery)
		default:
			errJSON(w, http.StatusMethodNotAllowed, "Method not allowed")
		}

	// /api/profiles/{id} — path-based fallback
	case strings.HasPrefix(path, "/api/profiles/"):
		id := strings.TrimPrefix(path, "/api/profiles/")
		switch r.Method {
		case http.MethodGet:
			handleGetByID(w, r, id)
		case http.MethodDelete:
			handleDelete(w, r, id)
		default:
			errJSON(w, http.StatusMethodNotAllowed, "Method not allowed")
		}

	default:
		errJSON(w, http.StatusNotFound, "Route not found")
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// HANDLERS
// ─────────────────────────────────────────────────────────────────────────────

// POST /api/profiles
func handleCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		errJSON(w, http.StatusBadRequest, "Invalid JSON body")
		return
	}

	name := strings.TrimSpace(body.Name)
	if name == "" {
		errJSON(w, http.StatusBadRequest, "Missing or empty 'name' field")
		return
	}
	if !isValidName(name) {
		errJSON(w, http.StatusUnprocessableEntity, "Invalid 'name': must contain alphabetic characters")
		return
	}

	db, err := getDB()
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "Database unavailable: "+err.Error())
		return
	}

	// Idempotency — return existing record if name already stored
	var existing Profile
	err = db.QueryRow(`
		SELECT id, name, gender, gender_probability, sample_size,
		       age, age_group, country_id, country_probability, created_at
		FROM profiles WHERE LOWER(name) = LOWER($1)
	`, name).Scan(
		&existing.ID, &existing.Name, &existing.Gender, &existing.GenderProbability,
		&existing.SampleSize, &existing.Age, &existing.AgeGroup,
		&existing.CountryID, &existing.CountryProbability, &existing.CreatedAt,
	)
	if err == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"status":  "success",
			"message": "Profile already exists",
			"data":    existing,
		})
		return
	}
	if err != sql.ErrNoRows {
		errJSON(w, http.StatusInternalServerError, "Database lookup failed")
		return
	}

	// Call all 3 APIs concurrently
	enriched := enrichName(name)

	for apiName, apiErr := range enriched.errors {
		if apiErr != nil {
			errUpstream(w, apiName)
			return
		}
	}

	// Validate — do not store null/empty responses
	g := enriched.genderize
	if g == nil || g.Gender == nil || g.Count == 0 {
		errUpstream(w, "Genderize")
		return
	}
	a := enriched.agify
	if a == nil || a.Age == nil {
		errUpstream(w, "Agify")
		return
	}
	n := enriched.national
	if n == nil || len(n.Country) == 0 {
		errUpstream(w, "Nationalize")
		return
	}

	// Pick country with highest probability
	top := n.Country[0]
	for _, c := range n.Country {
		if c.Probability > top.Probability {
			top = c
		}
	}

	profile := Profile{
		ID:                 uuid.Must(uuid.NewV7()).String(),
		Name:               name,
		Gender:             *g.Gender,
		GenderProbability:  g.Probability,
		SampleSize:         g.Count,
		Age:                *a.Age,
		AgeGroup:           classifyAge(*a.Age),
		CountryID:          top.CountryID,
		CountryProbability: top.Probability,
		CreatedAt:          time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}

	_, err = db.Exec(`
		INSERT INTO profiles
			(id, name, gender, gender_probability, sample_size,
			 age, age_group, country_id, country_probability, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
	`,
		profile.ID, profile.Name, profile.Gender, profile.GenderProbability,
		profile.SampleSize, profile.Age, profile.AgeGroup,
		profile.CountryID, profile.CountryProbability, profile.CreatedAt,
	)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "Failed to store profile")
		return
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"status": "success",
		"data":   profile,
	})
}

// GET /api/profiles
func handleList(w http.ResponseWriter, r *http.Request) {
	db, err := getDB()
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "Database unavailable: "+err.Error())
		return
	}

	query := `SELECT id, name, gender, age, age_group, country_id FROM profiles WHERE 1=1`
	args := []any{}
	i := 1

	if v := r.URL.Query().Get("gender"); v != "" {
		query += fmt.Sprintf(" AND LOWER(gender) = LOWER($%d)", i)
		args = append(args, v)
		i++
	}
	if v := r.URL.Query().Get("country_id"); v != "" {
		query += fmt.Sprintf(" AND LOWER(country_id) = LOWER($%d)", i)
		args = append(args, v)
		i++
	}
	if v := r.URL.Query().Get("age_group"); v != "" {
		query += fmt.Sprintf(" AND LOWER(age_group) = LOWER($%d)", i)
		args = append(args, v)
		i++
	}

	query += " ORDER BY created_at DESC"

	rows, err := db.Query(query, args...)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "Database query failed")
		return
	}
	defer rows.Close()

	profiles := []ProfileSummary{}
	for rows.Next() {
		var p ProfileSummary
		if err := rows.Scan(&p.ID, &p.Name, &p.Gender, &p.Age, &p.AgeGroup, &p.CountryID); err != nil {
			errJSON(w, http.StatusInternalServerError, "Failed to read profiles")
			return
		}
		profiles = append(profiles, p)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status": "success",
		"count":  len(profiles),
		"data":   profiles,
	})
}

// GET /api/profiles/{id}
func handleGetByID(w http.ResponseWriter, r *http.Request, id string) {
	if id == "" {
		errJSON(w, http.StatusBadRequest, "Missing profile ID")
		return
	}

	db, err := getDB()
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "Database unavailable: "+err.Error())
		return
	}

	var p Profile
	err = db.QueryRow(`
		SELECT id, name, gender, gender_probability, sample_size,
		       age, age_group, country_id, country_probability, created_at
		FROM profiles WHERE id = $1
	`, id).Scan(
		&p.ID, &p.Name, &p.Gender, &p.GenderProbability,
		&p.SampleSize, &p.Age, &p.AgeGroup,
		&p.CountryID, &p.CountryProbability, &p.CreatedAt,
	)
	if err == sql.ErrNoRows {
		errJSON(w, http.StatusNotFound, "Profile not found")
		return
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "Database error")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status": "success",
		"data":   p,
	})
}

// DELETE /api/profiles/{id}
func handleDelete(w http.ResponseWriter, r *http.Request, id string) {
	if id == "" {
		errJSON(w, http.StatusBadRequest, "Missing profile ID")
		return
	}

	db, err := getDB()
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "Database unavailable: "+err.Error())
		return
	}

	result, err := db.Exec(`DELETE FROM profiles WHERE id = $1`, id)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "Database error")
		return
	}

	rows, _ := result.RowsAffected()
	if rows == 0 {
		errJSON(w, http.StatusNotFound, "Profile not found")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
