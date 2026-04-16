package db

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"strings"

	_ "github.com/mattn/go-sqlite3" // requires CGO_ENABLED=1, build with -tags fts5

	"github.com/KAVA-Leigh/addresstify/internal/gnaf"
)

// Store wraps a SQLite connection and provides address query methods.
type Store struct {
	db *sql.DB
}

// Open opens the SQLite database at path.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?cache=shared&_journal_mode=WAL", path)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)

	if err := db.PingContext(context.Background()); err != nil {
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}

	for _, pragma := range []string{
		`PRAGMA cache_size=-65536`,   // 64MB page cache
		`PRAGMA mmap_size=536870912`, // 512MB mmap
		`PRAGMA temp_store=MEMORY`,
	} {
		if _, err := db.ExecContext(context.Background(), pragma); err != nil {
			slog.Warn("pragma failed", "pragma", pragma, "error", err)
		}
	}

	if v := os.Getenv("GNAF_MEMORY_LIMIT"); v != "" {
		slog.Info("GNAF_MEMORY_LIMIT is not used with SQLite; adjust PRAGMA cache_size instead")
	}

	slog.Info("sqlite opened", "path", path)
	return &Store{db: db}, nil
}

// Close closes the underlying database connection.
func (s *Store) Close() error {
	return s.db.Close()
}

// Search performs a full-text search using SQLite FTS5.
//
// Uses a phrase prefix query ("term1 term2..."*) which short-circuits at LIMIT
// without scanning the full index — very fast even on 15M rows.
func (s *Store) Search(ctx context.Context, query string, limit int) ([]gnaf.Address, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}

	numberHint := extractLeadingNumber(query)
	ftsQuery := buildFTSQuery(query)

	rows, err := s.db.QueryContext(ctx, `
		SELECT
			a.address_detail_pid,
			a.full_address,
			ifnull(a.number_first, ''),
			ifnull(a.street_name, ''),
			ifnull(a.street_type_code, ''),
			ifnull(a.locality_name, ''),
			ifnull(a.state_abbreviation, ''),
			ifnull(a.postcode, ''),
			ifnull(a.latitude, 0.0),
			ifnull(a.longitude, 0.0)
		FROM addresses_fts f
		JOIN addresses a ON a.id = f.rowid
		WHERE addresses_fts MATCH ?
		ORDER BY
			CASE WHEN ? != '' AND a.number_first = ? AND a.is_principal = 1 THEN 2
			     WHEN ? != '' AND a.number_first = ?                        THEN 1
			     ELSE 0 END DESC,
			f.rank
		LIMIT ?
	`, ftsQuery,
		numberHint, numberHint,
		numberHint, numberHint,
		limit)
	if err != nil {
		return nil, fmt.Errorf("search query: %w", err)
	}
	defer func(rows *sql.Rows) {
		err := rows.Close()
		if err != nil {
			slog.Warn("close rows", "error", err)
		}
	}(rows)

	return scanAddresses(rows)
}

// Validate finds the best structured match for a given address input.
func (s *Store) Validate(ctx context.Context, req gnaf.ValidateRequest) (*gnaf.Address, float64, error) {
	parts := []string{}
	if req.Number != "" {
		parts = append(parts, req.Number)
	}
	if req.StreetName != "" {
		parts = append(parts, req.StreetName)
	}
	if req.StreetType != "" {
		parts = append(parts, req.StreetType)
	}
	if req.Suburb != "" {
		parts = append(parts, req.Suburb)
	}
	if req.State != "" {
		parts = append(parts, req.State)
	}
	if req.Postcode != "" {
		parts = append(parts, req.Postcode)
	}

	if len(parts) == 0 {
		return nil, 0, nil
	}

	ftsQuery := buildFTSQuery(strings.Join(parts, " "))

	row := s.db.QueryRowContext(ctx, `
		SELECT
			a.address_detail_pid,
			a.full_address,
			ifnull(a.number_first, ''),
			ifnull(a.street_name, ''),
			ifnull(a.street_type_code, ''),
			ifnull(a.locality_name, ''),
			ifnull(a.state_abbreviation, ''),
			ifnull(a.postcode, ''),
			ifnull(a.latitude, 0.0),
			ifnull(a.longitude, 0.0),
			f.rank AS score
		FROM addresses_fts f
		JOIN addresses a ON a.id = f.rowid
		WHERE addresses_fts MATCH ?
		ORDER BY
			CASE WHEN ? != '' AND a.number_first = ? AND a.is_principal = 1 THEN 2
			     WHEN ? != '' AND a.number_first = ?                        THEN 1
			     ELSE 0 END DESC,
			f.rank
		LIMIT 1
	`, ftsQuery,
		req.Number, req.Number,
		req.Number, req.Number)

	var (
		pid, fullAddress, number       sql.NullString
		streetName, streetType, suburb sql.NullString
		state, postcode                sql.NullString
		lat, lon                       sql.NullFloat64
		score                          float64
	)
	err := row.Scan(
		&pid, &fullAddress,
		&number, &streetName, &streetType,
		&suburb, &state, &postcode,
		&lat, &lon,
		&score,
	)
	if err == sql.ErrNoRows {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, fmt.Errorf("validate query: %w", err)
	}

	a := gnaf.Address{
		PID:         pid.String,
		FullAddress: fullAddress.String,
		Number:      number.String,
		StreetName:  streetName.String,
		StreetType:  streetType.String,
		Suburb:      suburb.String,
		State:       state.String,
		Postcode:    postcode.String,
		Latitude:    lat.Float64,
		Longitude:   lon.Float64,
	}

	confidence := ftsRankToConfidence(score, len(parts))
	return &a, confidence, nil
}

// buildFTSQuery converts plain text into an FTS5 phrase prefix query.
// "218 newmarket road" → `"218 newmarket road"*`
// This is efficient because FTS5 seeks to the phrase in the positional index.
func buildFTSQuery(query string) string {
	query = strings.TrimSpace(query)
	if query == "" {
		return ""
	}
	query = strings.ReplaceAll(query, `"`, `""`)
	return `"` + query + `"*`
}

// extractLeadingNumber returns the first token if it's purely numeric.
func extractLeadingNumber(query string) string {
	query = strings.TrimSpace(query)
	if query == "" {
		return ""
	}
	token := strings.Fields(query)[0]
	for _, c := range token {
		if c < '0' || c > '9' {
			return ""
		}
	}
	return token
}

// ftsRankToConfidence converts FTS5 rank to 0–1 confidence.
// FTS5 rank is negative; more negative = stronger match.
func ftsRankToConfidence(rank float64, fieldCount int) float64 {
	if rank >= 0 {
		return 0
	}
	magnitude := -rank
	threshold := 10.0 * float64(fieldCount) / 6.0
	confidence := magnitude / threshold
	if confidence > 1.0 {
		confidence = 1.0
	}
	return confidence
}

func scanAddresses(rows *sql.Rows) ([]gnaf.Address, error) {
	var results []gnaf.Address
	for rows.Next() {
		var (
			pid, fullAddress, number       sql.NullString
			streetName, streetType, suburb sql.NullString
			state, postcode                sql.NullString
			lat, lon                       sql.NullFloat64
		)
		if err := rows.Scan(
			&pid, &fullAddress,
			&number, &streetName, &streetType,
			&suburb, &state, &postcode,
			&lat, &lon,
		); err != nil {
			return nil, fmt.Errorf("scan address: %w", err)
		}
		results = append(results, gnaf.Address{
			PID:         pid.String,
			FullAddress: fullAddress.String,
			Number:      number.String,
			StreetName:  streetName.String,
			StreetType:  streetType.String,
			Suburb:      suburb.String,
			State:       state.String,
			Postcode:    postcode.String,
			Latitude:    lat.Float64,
			Longitude:   lon.Float64,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}
	return results, nil
}
