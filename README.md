# Insighta Labs Intelligence Query Engine API

## Overview

A production-ready demographic intelligence API that enables advanced filtering, sorting, pagination, and natural language querying of profile data.

## Base URL

```
https://hng-stage-2-beta-murex.vercel.app/api
```

---

## Endpoints

### 1. Create Profile

**POST** `/profiles`

Creates a new profile by fetching data from external APIs (Genderize, Agify, Nationalize).

**Request Body:**

```json
{
  "name": "John Doe"
}
```

**Response:**

```json
{
  "status": "success",
  "data": {
    "id": "uuid-v7",
    "name": "John Doe",
    "gender": "male",
    "gender_probability": 0.95,
    "sample_size": 1000,
    "age": 35,
    "age_group": "adult",
    "country_id": "US",
    "country_name": "United States",
    "country_probability": 0.72,
    "created_at": "2024-01-01T00:00:00Z"
  }
}
```

---

### 2. List Profiles with Advanced Filtering

**GET** `/profiles`

**Query Parameters:**

| Parameter               | Type    | Description                                      |
|-------------------------|---------|--------------------------------------------------|
| `gender`                | string  | Filter by gender (`male` / `female`)             |
| `age_group`             | string  | Filter by age group (`child` / `teenager` / `adult` / `senior`) |
| `country_id`            | string  | Filter by country ISO code (e.g. `NG`, `KE`, `US`) |
| `min_age`               | integer | Minimum age filter                               |
| `max_age`               | integer | Maximum age filter                               |
| `min_gender_probability`| float   | Minimum gender probability (0–1)                 |
| `min_country_probability`| float  | Minimum country probability (0–1)                |
| `sort_by`               | string  | Sort field (`age` / `created_at` / `gender_probability`) |
| `order`                 | string  | Sort order (`asc` / `desc`)                      |
| `page`                  | integer | Page number (default: `1`)                       |
| `limit`                 | integer | Items per page, max `50` (default: `10`)         |

**Example Request:**

```
GET /profiles?gender=male&country_id=NG&min_age=25&sort_by=age&order=desc&page=1&limit=10
```

**Response:**

```json
{
  "status": "success",
  "page": 1,
  "limit": 10,
  "total": 2026,
  "data": [
    {
      "id": "uuid",
      "name": "John Doe",
      "gender": "male",
      "age": 35,
      "age_group": "adult",
      "country_id": "NG"
    }
  ]
}
```

---

### 3. Natural Language Search

**GET** `/profiles/search`

Converts plain English queries into database filters.

**Query Parameters:**

| Parameter | Description                              |
|-----------|------------------------------------------|
| `q`       | Natural language query string            |
| `page`    | Page number (default: `1`)               |
| `limit`   | Items per page, max `50` (default: `10`) |
| `sort_by` | Sort field (`age` / `created_at` / `gender_probability`) |
| `order`   | Sort order (`asc` / `desc`)              |

**Example Requests:**

```
GET /profiles/search?q=young males from nigeria
GET /profiles/search?q=females above 30
GET /profiles/search?q=adult males from kenya
GET /profiles/search?q=male and female teenagers above 17
```

**Supported Natural Language Patterns:**

- **Gender:** `male`, `female`, `women`, `females`
- **Age groups:** `child`, `teenager`, `adult`, `senior`
- **Young:** maps to ages 16–24
- **Age ranges:** `above 30`, `over 18`, `below 20`, `under 25`
- **Countries:** Nigeria, Kenya, Ghana, South Africa, etc.

**Response:**

```json
{
  "status": "success",
  "page": 1,
  "limit": 10,
  "total": 45,
  "data": [...]
}
```

---

### 4. Get Profile by ID

**GET** `/profiles/{id}`

**Response:**

```json
{
  "status": "success",
  "data": {
    "id": "uuid",
    "name": "John Doe",
    "gender": "male",
    "gender_probability": 0.95,
    "sample_size": 1000,
    "age": 35,
    "age_group": "adult",
    "country_id": "US",
    "country_name": "United States",
    "country_probability": 0.72,
    "created_at": "2024-01-01T00:00:00Z"
  }
}
```

---

### 5. Delete Profile

**DELETE** `/profiles/{id}`

**Response:** `204 No Content`

---

## Error Responses

All errors follow this structure:

```json
{
  "status": "error",
  "message": "Error description"
}
```

**HTTP Status Codes:**

| Code | Meaning                                    |
|------|--------------------------------------------|
| 200  | Success                                    |
| 201  | Created                                    |
| 204  | No Content (successful delete)             |
| 400  | Bad Request (missing parameters)           |
| 404  | Not Found                                  |
| 422  | Unprocessable Entity (invalid param types) |
| 500  | Internal Server Error                      |
| 502  | Bad Gateway (external API failure)         |

---

## Setup Instructions

### Environment Variables

```env
DATABASE_URL=postgresql://user:password@host:5432/database
SEED_FILE_PATH=seed_profiles.json   # Optional path to seed file
```

### Local Development

```bash
# Install dependencies
go mod tidy

# Run locally
go run index.go

# Deploy to Vercel
vercel deploy
```

### Database Seeding

The system automatically seeds the database with 2,026 profiles from `seed_profiles.json`. The seed operation:

- Checks if profiles already exist
- Skips seeding if data is present (idempotent)
- Uses `ON CONFLICT (name) DO NOTHING` to prevent duplicates

---

## Architecture

### Database Schema

```sql
CREATE TABLE profiles (
  id                  TEXT PRIMARY KEY,
  name                TEXT UNIQUE NOT NULL,
  gender              TEXT,
  gender_probability  DOUBLE PRECISION,
  sample_size         INTEGER,
  age                 INTEGER,
  age_group           TEXT,
  country_id          TEXT,
  country_name        TEXT,
  country_probability DOUBLE PRECISION,
  created_at          TEXT NOT NULL
);
```

### Performance Optimizations

- Prepared statements for all queries
- Connection pooling (max 5 connections)
- Proper indexing on filtered fields
- Pagination to limit result sets
- Efficient natural language parsing without AI/LLM

---

## CORS

All endpoints support CORS with the following headers:

```
Access-Control-Allow-Origin: *
Access-Control-Allow-Methods: GET, POST, DELETE, OPTIONS
Access-Control-Allow-Headers: Content-Type
```

---

## Testing Examples

### Filtering

```bash
# Get Nigerian males aged 25+
curl "https://your-api.vercel.app/api/profiles?gender=male&country_id=NG&min_age=25"

# Get females with high confidence
curl "https://your-api.vercel.app/api/profiles?gender=female&min_gender_probability=0.9"
```

### Natural Language

```bash
# Find young males from Nigeria
curl "https://your-api.vercel.app/api/profiles/search?q=young males from nigeria"

# Find adult females above 30
curl "https://your-api.vercel.app/api/profiles/search?q=females above 30"
```

### Pagination & Sorting

```bash
# Page 2, 20 items, sorted by age descending
curl "https://your-api.vercel.app/api/profiles?page=2&limit=20&sort_by=age&order=desc"
```

---

## Deployment

### Vercel Configuration (`vercel.json`)

```json
{
  "functions": {
    "api/index.go": {
      "runtime": "go1.21",
      "includeFiles": "seed_profiles.json"
    }
  },
  "rewrites": [
    {
      "source": "/api/(.*)",
      "destination": "/api/index"
    }
  ]
}
```

---

## Summary of Changes

1. **Advanced Filtering** — Added support for `gender`, `age_group`, `country_id`, `min_age`, `max_age`, `min_gender_probability`, and `min_country_probability` filters with proper validation.

2. **Sorting** — Implemented `sort_by` (`age` / `created_at` / `gender_probability`) and `order` (`asc` / `desc`) parameters.

3. **Pagination** — Added `page` and `limit` parameters with a max limit of `50`; returns total count and proper pagination metadata.

4. **Natural Language Query** — Created a parser that converts plain English queries into filters without AI/LLM:
   - Gender detection (`male` / `female`)
   - Age group matching (`child`, `teenager`, `adult`, `senior`)
   - "Young" keyword mapping (ages 16–24)
   - Age range parsing (`above` / `below` / `over` / `under`)
   - Country name to ISO code mapping

5. **Database Seeding** — Added automatic seeding from `seed_profiles.json` with duplicate prevention.

6. **Table Schema Update** — Added `country_name` field to match requirements.

7. **Error Handling** — Proper HTTP status codes and consistent error response format.

8. **CORS** — Already implemented across all endpoints.

9. **Performance** — Efficient query building, prepared statements, and proper indexing.

---

## License

MIT