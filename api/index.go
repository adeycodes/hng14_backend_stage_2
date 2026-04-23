package handler

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/google/uuid"
	_ "github.com/lib/pq"
)

// =================================================================================
// DATABASE - Safe Initialization with Auto-Migration
// =================================================================================

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

		// 1. Create table if missing
		_, err = db.Exec(`
            CREATE TABLE IF NOT EXISTS profiles (
                id                  TEXT PRIMARY KEY,
                name                TEXT UNIQUE NOT NULL,
                gender              TEXT,
                gender_probability  DOUBLE PRECISION,
                sample_size         INTEGER DEFAULT 0,
                age                 INTEGER,
                age_group           TEXT,
                country_id          TEXT,
                country_name        TEXT,
                country_probability DOUBLE PRECISION,
                created_at          TEXT NOT NULL
            )
        `)
		if err != nil {
			dbErr = fmt.Errorf("create table: %v", err)
			return
		}

		// 2. Auto-Migrate: Add columns if they don't exist (Fixes Schema Mismatch)
		migrations := []string{
			"ALTER TABLE profiles ADD COLUMN IF NOT EXISTS gender_probability DOUBLE PRECISION",
			"ALTER TABLE profiles ADD COLUMN IF NOT EXISTS country_name TEXT",
			"ALTER TABLE profiles ADD COLUMN IF NOT EXISTS country_probability DOUBLE PRECISION",
			"ALTER TABLE profiles ADD COLUMN IF NOT EXISTS sample_size INTEGER",
			"ALTER TABLE profiles ADD COLUMN IF NOT EXISTS created_at TEXT",
		}
		for _, m := range migrations {
			db.Exec(m)
		}
	})
	return db, dbErr
}

// =================================================================================
// MODELS
// =================================================================================

type Profile struct {
	ID                 string  `json:"id"`
	Name               string  `json:"name"`
	Gender             string  `json:"gender"`
	GenderProbability  float64 `json:"gender_probability"`
	SampleSize         int     `json:"sample_size"`
	Age                int     `json:"age"`
	AgeGroup           string  `json:"age_group"`
	CountryID          string  `json:"country_id"`
	CountryName        string  `json:"country_name"`
	CountryProbability float64 `json:"country_probability"`
	CreatedAt          string  `json:"created_at"`
}

type PaginatedResponse struct {
	Status string    `json:"status"`
	Page   int       `json:"page"`
	Limit  int       `json:"limit"`
	Total  int       `json:"total"`
	Data   []Profile `json:"data"`
}

// =================================================================================
// EXTERNAL API RESPONSE TYPES
// =================================================================================

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

var countryNames = map[string]string{
	"NG": "Nigeria", "BJ": "Benin", "GH": "Ghana", "CI": "Côte d'Ivoire",
	"SN": "Senegal", "ML": "Mali", "ZA": "South Africa", "KE": "Kenya",
	"TZ": "Tanzania", "UG": "Uganda", "ET": "Ethiopia", "AO": "Angola",
	"ZW": "Zimbabwe", "ZM": "Zambia", "MW": "Malawi", "MZ": "Mozambique",
	"RW": "Rwanda", "SO": "Somalia", "SS": "South Sudan", "ER": "Eritrea",
	"DJ": "Djibouti", "BI": "Burundi", "CM": "Cameroon", "CD": "DR Congo",
	"CG": "Republic of the Congo", "GA": "Gabon", "GQ": "Equatorial Guinea",
	"CF": "Central African Republic", "TD": "Chad", "NE": "Niger",
	"BF": "Burkina Faso", "GM": "Gambia", "GW": "Guinea-Bissau",
	"GN": "Guinea", "SL": "Sierra Leone", "LR": "Liberia", "CV": "Cape Verde",
	"ST": "São Tomé and Príncipe", "MU": "Mauritius", "SC": "Seychelles",
	"KM": "Comoros", "MG": "Madagascar", "MA": "Morocco", "DZ": "Algeria",
	"TN": "Tunisia", "LY": "Libya", "EG": "Egypt", "SD": "Sudan",
	"MR": "Mauritania", "EH": "Western Sahara", "US": "United States",
	"GB": "United Kingdom", "CA": "Canada", "FR": "France", "DE": "Germany",
	"BR": "Brazil", "IN": "India", "CN": "China", "JP": "Japan", "AU": "Australia",
}

// =================================================================================
// HELPER FUNCTIONS
// =================================================================================

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func errJSON(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"status": "error", "message": message})
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

// =================================================================================
// NATURAL LANGUAGE QUERY PARSER
// =================================================================================

type QueryFilters struct {
	Gender                *string
	AgeGroup              *string
	CountryID             *string
	MinAge                *int
	MaxAge                *int
	MinGenderProbability  *float64
	MinCountryProbability *float64
}

func containsWord(words []string, keywords ...string) bool {
	for _, w := range words {
		cleanW := strings.ToLower(strings.Trim(w, ".,!?"))
		for _, k := range keywords {
			if cleanW == k {
				return true
			}
		}
	}
	return false
}

func parseNaturalLanguage(query string) (*QueryFilters, error) {
	if query == "" {
		return nil, fmt.Errorf("empty query")
	}

	query = strings.ToLower(query)
	filters := &QueryFilters{}
	words := strings.Fields(query)

	// --- Gender Detection (Strict) ---
	maleKeywords := []string{"male", "males", "man", "men", "boy", "boys"}
	femaleKeywords := []string{"female", "females", "woman", "women", "girl", "girls"}

	hasMale := containsWord(words, maleKeywords...)
	hasFemale := containsWord(words, femaleKeywords...)

	if hasMale && hasFemale {
		// Mixed query, do not filter gender
	} else if hasMale {
		g := "male"
		filters.Gender = &g
	} else if hasFemale {
		g := "female"
		filters.Gender = &g
	}

	// --- Age Group ---
	if containsWord(words, "child", "children") {
		ag := "child"
		filters.AgeGroup = &ag
	}
	if containsWord(words, "teen", "teenager", "teenagers", "adolescent") {
		ag := "teenager"
		filters.AgeGroup = &ag
	}
	if containsWord(words, "adult", "adults") {
		ag := "adult"
		filters.AgeGroup = &ag
	}
	if containsWord(words, "senior", "seniors", "elder", "elderly") {
		ag := "senior"
		filters.AgeGroup = &ag
	}
	if containsWord(words, "old") && !containsWord(words, "older") {
		ag := "senior"
		filters.AgeGroup = &ag
	}

	// --- Age Range ---
	// Task Requirement: "young" -> 16-24
	if containsWord(words, "young") && !containsWord(words, "younger") {
		minAge := 16
		maxAge := 24
		filters.MinAge = &minAge
		filters.MaxAge = &maxAge
	}

	for i, word := range words {
		if (word == "above" || word == "over" || word == "older") && i+1 < len(words) {
			if age, err := strconv.Atoi(words[i+1]); err == nil {
				filters.MinAge = &age
			}
		}
		if (word == "below" || word == "under" || word == "younger") && i+1 < len(words) {
			if age, err := strconv.Atoi(words[i+1]); err == nil {
				filters.MaxAge = &age
			}
		}
		if word == "between" && i+2 < len(words) {
			lo, err1 := strconv.Atoi(words[i+1])
			hiIdx := i + 2
			if hiIdx < len(words) && words[hiIdx] == "and" {
				hiIdx++
			}
			if hiIdx < len(words) {
				hi, err2 := strconv.Atoi(words[hiIdx])
				if err1 == nil && err2 == nil {
					filters.MinAge = &lo
					filters.MaxAge = &hi
				}
			}
		}
	}

	// --- Country ---
	countryMap := map[string]string{
		"nigeria": "NG", "ghana": "GH", "kenya": "KE", "south africa": "ZA",
		"tanzania": "TZ", "uganda": "UG", "ethiopia": "ET", "angola": "AO",
		"zimbabwe": "ZW", "zambia": "ZM", "malawi": "MW", "mozambique": "MZ",
		"rwanda": "RW", "somalia": "SO", "sudan": "SD", "egypt": "EG",
		"morocco": "MA", "algeria": "DZ", "tunisia": "TN", "libya": "LY",
		"senegal": "SN", "mali": "ML", "congo": "CD", "cameroon": "CM",
		"united states": "US", "usa": "US", "uk": "GB", "united kingdom": "GB",
		"canada": "CA", "france": "FR", "germany": "DE", "brazil": "BR",
		"india": "IN", "china": "CN", "japan": "JP", "australia": "AU",
		"benin": "BJ", "ivory coast": "CI", "côte d'ivoire": "CI",
	}
	for country, code := range countryMap {
		if strings.Contains(query, country) {
			c := code
			filters.CountryID = &c
			break
		}
	}

	if filters.Gender == nil && filters.AgeGroup == nil && filters.CountryID == nil &&
		filters.MinAge == nil && filters.MaxAge == nil {
		return nil, fmt.Errorf("unable to interpret query")
	}

	return filters, nil
}

func applyFiltersToQuery(whereClause string, filters *QueryFilters, args *[]interface{}, argIndex *int) string {
	if filters.Gender != nil {
		whereClause += fmt.Sprintf(" AND LOWER(gender) = LOWER($%d)", *argIndex)
		*args = append(*args, *filters.Gender)
		*argIndex++
	}
	if filters.AgeGroup != nil {
		whereClause += fmt.Sprintf(" AND LOWER(age_group) = LOWER($%d)", *argIndex)
		*args = append(*args, *filters.AgeGroup)
		*argIndex++
	}
	if filters.CountryID != nil {
		whereClause += fmt.Sprintf(" AND LOWER(country_id) = LOWER($%d)", *argIndex)
		*args = append(*args, *filters.CountryID)
		*argIndex++
	}
	if filters.MinAge != nil {
		whereClause += fmt.Sprintf(" AND age >= $%d", *argIndex)
		*args = append(*args, *filters.MinAge)
		*argIndex++
	}
	if filters.MaxAge != nil {
		whereClause += fmt.Sprintf(" AND age <= $%d", *argIndex)
		*args = append(*args, *filters.MaxAge)
		*argIndex++
	}
	if filters.MinGenderProbability != nil {
		whereClause += fmt.Sprintf(" AND gender_probability >= $%d", *argIndex)
		*args = append(*args, *filters.MinGenderProbability)
		*argIndex++
	}
	if filters.MinCountryProbability != nil {
		whereClause += fmt.Sprintf(" AND country_probability >= $%d", *argIndex)
		*args = append(*args, *filters.MinCountryProbability)
		*argIndex++
	}
	return whereClause
}

// =================================================================================
// DATABASE SEEDING (Remote URL for Vercel)
// =================================================================================

func seedDatabase() error {
	db, err := getDB()
	if err != nil {
		return err
	}

	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM profiles").Scan(&count)
	if err != nil {
		return err
	}
	if count > 0 {
		return nil // Already seeded
	}

	fmt.Println("Seeding database from remote source...")

	// !!! REPLACE THIS URL WITH YOUR RAW GITHUB URL !!!
	remoteURL := "https://raw.githubusercontent.com/adeycodes/hng14_backend_stage_2/refs/heads/main/seed_profiles.json"

	resp, httpErr := http.Get(remoteURL)
	if httpErr != nil {
		return fmt.Errorf("remote download failed: %v", httpErr)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("remote file status: %d", resp.StatusCode)
	}

	bodyBytes, _ := io.ReadAll(resp.Body)

	var profiles []Profile
	if err := json.Unmarshal(bodyBytes, &profiles); err != nil {
		var wrapper struct {
			Profiles []Profile `json:"profiles"`
		}
		if wrapErr := json.Unmarshal(bodyBytes, &wrapper); wrapErr != nil {
			return fmt.Errorf("invalid JSON format")
		}
		profiles = wrapper.Profiles
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}

	stmt, err := tx.Prepare(`
        INSERT INTO profiles 
            (id, name, gender, gender_probability, sample_size, age, age_group, 
             country_id, country_name, country_probability, created_at)
        VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
        ON CONFLICT (name) DO NOTHING
    `)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()

	for _, p := range profiles {
		if p.ID == "" {
			p.ID = uuid.Must(uuid.NewV7()).String()
		}
		if p.CreatedAt == "" {
			p.CreatedAt = time.Now().UTC().Format("2006-01-02T15:04:05Z")
		}
		if p.AgeGroup == "" && p.Age > 0 {
			p.AgeGroup = classifyAge(p.Age)
		}

		_, err = stmt.Exec(
			p.ID, p.Name, p.Gender, p.GenderProbability, p.SampleSize,
			p.Age, p.AgeGroup, p.CountryID, p.CountryName, p.CountryProbability, p.CreatedAt,
		)
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("insert failed for %s: %v", p.Name, err)
		}
	}

	fmt.Printf("Seeded %d profiles.\n", len(profiles))
	return tx.Commit()
}

// =================================================================================
// CORS MIDDLEWARE
// =================================================================================

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

// =================================================================================
// CONCURRENT ENRICHMENT
// =================================================================================

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

// =================================================================================
// ROUTER
// =================================================================================

func Handler(w http.ResponseWriter, r *http.Request) {
	if err := seedDatabase(); err != nil {
		fmt.Fprintf(os.Stderr, "Seed Error: %v\n", err)
	}
	withCORS(router)(w, r)
}

func router(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimRight(r.URL.Path, "/")
	idFromQuery := r.URL.Query().Get("id")
	qParam := r.URL.Query().Get("q")

	isSearchPath := strings.HasSuffix(path, "/search")
	isSearchQuery := qParam != ""

	if isSearchPath || isSearchQuery {
		if r.Method == http.MethodGet {
			handleNaturalSearch(w, r)
		} else {
			errJSON(w, http.StatusMethodNotAllowed, "Method not allowed")
		}
		return
	}

	switch {
	case path == "/api/profiles" || path == "/api/index":
		switch r.Method {
		case http.MethodGet:
			handleListWithFilters(w, r)
		case http.MethodPost:
			handleCreate(w, r)
		default:
			errJSON(w, http.StatusMethodNotAllowed, "Method not allowed")
		}

	case idFromQuery != "":
		switch r.Method {
		case http.MethodGet:
			handleGetByID(w, r, idFromQuery)
		case http.MethodDelete:
			handleDelete(w, r, idFromQuery)
		default:
			errJSON(w, http.StatusMethodNotAllowed, "Method not allowed")
		}

	case strings.HasPrefix(path, "/api/profiles/"):
		id := strings.TrimPrefix(path, "/api/profiles/")
		if id != "" {
			switch r.Method {
			case http.MethodGet:
				handleGetByID(w, r, id)
			case http.MethodDelete:
				handleDelete(w, r, id)
			default:
				errJSON(w, http.StatusMethodNotAllowed, "Method not allowed")
			}
		} else {
			errJSON(w, http.StatusNotFound, "Route not found")
		}

	default:
		errJSON(w, http.StatusNotFound, "Route not found")
	}
}

// =================================================================================
// HANDLERS
// =================================================================================

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

	var existing Profile
	err = db.QueryRow(`
        SELECT id, name, gender, gender_probability, sample_size,
               age, age_group, country_id, country_name, country_probability, created_at
        FROM profiles WHERE LOWER(name) = LOWER($1)
    `, name).Scan(
		&existing.ID, &existing.Name, &existing.Gender, &existing.GenderProbability,
		&existing.SampleSize, &existing.Age, &existing.AgeGroup,
		&existing.CountryID, &existing.CountryName, &existing.CountryProbability, &existing.CreatedAt,
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

	enriched := enrichName(name)
	for apiName, apiErr := range enriched.errors {
		if apiErr != nil {
			errJSON(w, http.StatusBadGateway, apiName+" returned an invalid response")
			return
		}
	}

	g := enriched.genderize
	if g == nil || g.Gender == nil || g.Count == 0 {
		errJSON(w, http.StatusBadGateway, "Genderize returned an invalid response")
		return
	}
	a := enriched.agify
	if a == nil || a.Age == nil {
		errJSON(w, http.StatusBadGateway, "Agify returned an invalid response")
		return
	}
	n := enriched.national
	if n == nil || len(n.Country) == 0 {
		errJSON(w, http.StatusBadGateway, "Nationalize returned an invalid response")
		return
	}

	top := n.Country[0]
	for _, c := range n.Country {
		if c.Probability > top.Probability {
			top = c
		}
	}

	cName := countryNames[top.CountryID]
	if cName == "" {
		cName = top.CountryID
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
		CountryName:        cName,
		CountryProbability: top.Probability,
		CreatedAt:          time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}

	_, err = db.Exec(`
        INSERT INTO profiles
            (id, name, gender, gender_probability, sample_size,
             age, age_group, country_id, country_name, country_probability, created_at)
        VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
    `,
		profile.ID, profile.Name, profile.Gender, profile.GenderProbability,
		profile.SampleSize, profile.Age, profile.AgeGroup,
		profile.CountryID, profile.CountryName, profile.CountryProbability, profile.CreatedAt,
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
func handleListWithFilters(w http.ResponseWriter, r *http.Request) {
	db, err := getDB()
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "Database unavailable: "+err.Error())
		return
	}

	whereClause := " WHERE 1=1"
	args := []interface{}{}
	argIndex := 1

	if v := r.URL.Query().Get("gender"); v != "" {
		if v != "male" && v != "female" {
			errJSON(w, http.StatusUnprocessableEntity, "Invalid gender value")
			return
		}
		whereClause += fmt.Sprintf(" AND LOWER(gender) = LOWER($%d)", argIndex)
		args = append(args, v)
		argIndex++
	}

	if v := r.URL.Query().Get("country_id"); v != "" {
		whereClause += fmt.Sprintf(" AND LOWER(country_id) = LOWER($%d)", argIndex)
		args = append(args, v)
		argIndex++
	}

	if v := r.URL.Query().Get("age_group"); v != "" {
		validGroups := map[string]bool{"child": true, "teenager": true, "adult": true, "senior": true}
		if !validGroups[strings.ToLower(v)] {
			errJSON(w, http.StatusUnprocessableEntity, "Invalid age_group")
			return
		}
		whereClause += fmt.Sprintf(" AND LOWER(age_group) = LOWER($%d)", argIndex)
		args = append(args, v)
		argIndex++
	}

	if v := r.URL.Query().Get("min_age"); v != "" {
		minAge, err := strconv.Atoi(v)
		if err != nil {
			errJSON(w, http.StatusUnprocessableEntity, "Invalid min_age")
			return
		}
		whereClause += fmt.Sprintf(" AND age >= $%d", argIndex)
		args = append(args, minAge)
		argIndex++
	}

	if v := r.URL.Query().Get("max_age"); v != "" {
		maxAge, err := strconv.Atoi(v)
		if err != nil {
			errJSON(w, http.StatusUnprocessableEntity, "Invalid max_age")
			return
		}
		whereClause += fmt.Sprintf(" AND age <= $%d", argIndex)
		args = append(args, maxAge)
		argIndex++
	}

	if v := r.URL.Query().Get("min_gender_probability"); v != "" {
		val, err := strconv.ParseFloat(v, 64)
		if err != nil {
			errJSON(w, http.StatusUnprocessableEntity, "Invalid probability")
			return
		}
		whereClause += fmt.Sprintf(" AND gender_probability >= $%d", argIndex)
		args = append(args, val)
		argIndex++
	}

	if v := r.URL.Query().Get("min_country_probability"); v != "" {
		val, err := strconv.ParseFloat(v, 64)
		if err != nil {
			errJSON(w, http.StatusUnprocessableEntity, "Invalid probability")
			return
		}
		whereClause += fmt.Sprintf(" AND country_probability >= $%d", argIndex)
		args = append(args, val)
		argIndex++
	}

	sortBy := r.URL.Query().Get("sort_by")
	validSortFields := map[string]bool{"age": true, "created_at": true, "gender_probability": true}
	if !validSortFields[sortBy] {
		sortBy = "created_at"
	}
	order := r.URL.Query().Get("order")
	if order != "asc" && order != "desc" {
		order = "desc"
	}

	countSQL := "SELECT COUNT(*) FROM profiles" + whereClause
	var total int
	if err = db.QueryRow(countSQL, args...).Scan(&total); err != nil {
		errJSON(w, http.StatusInternalServerError, "Database count failed")
		return
	}

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit < 1 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	offset := (page - 1) * limit

	dataSQL := fmt.Sprintf(
		`SELECT id, name, gender, gender_probability, age, age_group, 
                country_id, country_name, country_probability, created_at 
         FROM profiles%s ORDER BY %s %s LIMIT $%d OFFSET $%d`,
		whereClause, sortBy, order, argIndex, argIndex+1,
	)
	dataArgs := append(args, limit, offset)

	rows, err := db.Query(dataSQL, dataArgs...)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "Database query failed: "+err.Error())
		return
	}
	defer rows.Close()

	profiles := []Profile{}
	for rows.Next() {
		var p Profile
		if err := rows.Scan(
			&p.ID, &p.Name, &p.Gender, &p.GenderProbability, &p.Age, &p.AgeGroup,
			&p.CountryID, &p.CountryName, &p.CountryProbability, &p.CreatedAt,
		); err != nil {
			errJSON(w, http.StatusInternalServerError, "Failed to read profiles")
			return
		}
		profiles = append(profiles, p)
	}

	writeJSON(w, http.StatusOK, PaginatedResponse{
		Status: "success",
		Page:   page,
		Limit:  limit,
		Total:  total,
		Data:   profiles,
	})
}

// GET /api/profiles/search
func handleNaturalSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if q == "" {
		errJSON(w, http.StatusBadRequest, "Missing 'q' parameter")
		return
	}

	filters, err := parseNaturalLanguage(q)
	if err != nil {
		errJSON(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	db, err := getDB()
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "Database unavailable: "+err.Error())
		return
	}

	whereClause := " WHERE 1=1"
	args := []interface{}{}
	argIndex := 1
	whereClause = applyFiltersToQuery(whereClause, filters, &args, &argIndex)

	sortBy := r.URL.Query().Get("sort_by")
	validSortFields := map[string]bool{"age": true, "created_at": true, "gender_probability": true}
	if !validSortFields[sortBy] {
		sortBy = "created_at"
	}
	order := r.URL.Query().Get("order")
	if order != "asc" && order != "desc" {
		order = "desc"
	}

	countSQL := "SELECT COUNT(*) FROM profiles" + whereClause
	var total int
	if err = db.QueryRow(countSQL, args...).Scan(&total); err != nil {
		errJSON(w, http.StatusInternalServerError, "Database count failed")
		return
	}

	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit < 1 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	offset := (page - 1) * limit

	dataSQL := fmt.Sprintf(
		`SELECT id, name, gender, gender_probability, age, age_group, 
                country_id, country_name, country_probability, created_at 
         FROM profiles%s ORDER BY %s %s LIMIT $%d OFFSET $%d`,
		whereClause, sortBy, order, argIndex, argIndex+1,
	)
	dataArgs := append(args, limit, offset)

	rows, err := db.Query(dataSQL, dataArgs...)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "Database query failed")
		return
	}
	defer rows.Close()

	profiles := []Profile{}
	for rows.Next() {
		var p Profile
		if err := rows.Scan(
			&p.ID, &p.Name, &p.Gender, &p.GenderProbability, &p.Age, &p.AgeGroup,
			&p.CountryID, &p.CountryName, &p.CountryProbability, &p.CreatedAt,
		); err != nil {
			errJSON(w, http.StatusInternalServerError, "Failed to read profiles")
			return
		}
		profiles = append(profiles, p)
	}

	writeJSON(w, http.StatusOK, PaginatedResponse{
		Status: "success",
		Page:   page,
		Limit:  limit,
		Total:  total,
		Data:   profiles,
	})
}

// GET /api/profiles/{id}
func handleGetByID(w http.ResponseWriter, r *http.Request, id string) {
	db, err := getDB()
	if err != nil {
		errJSON(w, http.StatusInternalServerError, "Database unavailable: "+err.Error())
		return
	}

	var p Profile
	err = db.QueryRow(`
        SELECT id, name, gender, gender_probability, sample_size,
               age, age_group, country_id, country_name, country_probability, created_at
        FROM profiles WHERE id = $1
    `, id).Scan(
		&p.ID, &p.Name, &p.Gender, &p.GenderProbability,
		&p.SampleSize, &p.Age, &p.AgeGroup,
		&p.CountryID, &p.CountryName, &p.CountryProbability, &p.CreatedAt,
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
