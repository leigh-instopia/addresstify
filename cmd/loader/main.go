package main

import (
	"archive/zip"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/marcboeker/go-duckdb"
	_ "github.com/mattn/go-sqlite3" // requires CGO_ENABLED=1
)

const (
	// Feb 2026 G-NAF release (GDA94). Update this URL each quarter.
	// Find the latest at: https://data.gov.au/data/dataset/geocoded-national-address-file-g-naf
	gnafURL = "https://data.gov.au/data/dataset/19432f89-dc3a-4ef3-b943-5326ef1dbecc/resource/43e3933b-078b-4a65-8e42-ebd653e3f8df/download/g-naf_feb26_allstates_gda94_psv_1022.zip"

	// Expected size of the Feb 2026 zip in bytes. Used to skip re-download.
	// Update alongside gnafURL each quarter.
	gnafExpectedBytes = int64(1700877251)

	batchSize = 10_000
)

type psvTarget struct {
	suffix        string
	table         string
	exclude       []string
	noStateFilter bool
}

var psvTargets = []psvTarget{
	{suffix: "_ADDRESS_DETAIL_psv.psv", table: "raw_address_detail"},
	{suffix: "_ADDRESS_DEFAULT_GEOCODE_psv.psv", table: "raw_geocode"},
	{suffix: "_STREET_LOCALITY_psv.psv", table: "raw_street_locality"},
	{suffix: "_LOCALITY_psv.psv", table: "raw_locality", exclude: []string{"STREET_LOCALITY", "LOCALITY_ALIAS", "LOCALITY_NEIGHBOUR", "LOCALITY_POINT"}},
	{suffix: "_STATE_psv.psv", table: "raw_state", exclude: []string{"LOCALITY"}, noStateFilter: true},
}

func main() {
	dbPath := envOrDefault("GNAF_DB_PATH", "/data/gnaf.sqlite")
	dataDir := envOrDefault("GNAF_DATA_DIR", "/data/gnaf-raw")
	states := envOrDefault("GNAF_STATES", "")
	url := envOrDefault("GNAF_URL", gnafURL)

	if err := os.MkdirAll(dataDir, 0755); err != nil {
		slog.Error("failed to create data dir", "error", err)
		os.Exit(1)
	}

	ctx := context.Background()

	zipPath := filepath.Join(dataDir, "gnaf.zip")
	if shouldDownload(zipPath, gnafExpectedBytes) {
		slog.Info("downloading G-NAF", "url", url)
		if err := downloadFile(ctx, url, zipPath); err != nil {
			slog.Error("download failed", "error", err)
			os.Exit(1)
		}
	} else {
		slog.Info("skipping download, zip already present", "path", zipPath)
	}

	// Use a temp directory for the intermediate DuckDB build file.
	// This avoids any permission issues with the data volume.
	buildDir, err := os.MkdirTemp("", "gnaf-build-*")
	if err != nil {
		slog.Error("failed to create build dir", "error", err)
		os.Exit(1)
	}
	defer func(path string) {
		err := os.RemoveAll(path)
		if err != nil {
			slog.Error("failed to remove build dir", "error", err)
		}
	}(buildDir)

	duckPath := filepath.Join(buildDir, "gnaf_build.duckdb")
	tmpSQLite := dbPath + ".tmp"
	err = os.Remove(tmpSQLite)
	if err != nil {
		return
	}

	slog.Info("building intermediate DuckDB database")
	if err := buildDuckDB(ctx, zipPath, duckPath, states); err != nil {
		slog.Error("duckdb build failed", "error", err)
		err := os.Remove(duckPath)
		if err != nil {
			return
		}
		os.Exit(1)
	}

	slog.Info("exporting to SQLite")
	if err := exportToSQLite(ctx, duckPath, tmpSQLite); err != nil {
		slog.Error("sqlite export failed", "error", err)
		err := os.Remove(duckPath)
		if err != nil {
			return
		}
		err = os.Remove(tmpSQLite)
		if err != nil {
			return
		}
		os.Exit(1)
	}

	err = os.Remove(duckPath)
	if err != nil {
		return
	}

	slog.Info("swapping database file")
	if err := os.Rename(tmpSQLite, dbPath); err != nil {
		slog.Error("rename failed", "error", err)
		os.Exit(1)
	}

	slog.Info("done", "db", dbPath)
}

func shouldDownload(path string, expectedBytes int64) bool {
	if expectedBytes == 0 {
		return true
	}
	info, err := os.Stat(path)
	if err != nil {
		return true
	}
	if info.Size() != expectedBytes {
		slog.Info("zip size mismatch, re-downloading", "have", info.Size(), "want", expectedBytes)
		return true
	}
	return false
}

func downloadFile(ctx context.Context, url, dest string) error {
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects")
			}
			slog.Info("redirect", "to", req.URL.String())
			return nil
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("User-Agent", "addresstify/1.0 (github.com/KAVA-Leigh/addresstify)")

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("http get: %w", err)
	}
	defer func(Body io.ReadCloser) {
		err := Body.Close()
		if err != nil {
			fmt.Println(err)
		}
	}(resp.Body)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status: %s", resp.Status)
	}

	f, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("create file: %w", err)
	}
	defer func(f *os.File) {
		err := f.Close()
		if err != nil {
			fmt.Println(err)
		}
	}(f)

	n, err := io.Copy(f, resp.Body)
	if err != nil {
		return fmt.Errorf("write file: %w", err)
	}
	slog.Info("downloaded", "bytes", n, "path", dest)
	return nil
}

// buildDuckDB loads the G-NAF PSV files, joins them, deduplicates, and
// writes the result to a DuckDB file. This intermediate file is then
// exported to SQLite.
func buildDuckDB(ctx context.Context, zipPath, dbPath, states string) error {
	db, err := sql.Open("duckdb", dbPath)
	if err != nil {
		return fmt.Errorf("open duckdb: %w", err)
	}
	defer func(db *sql.DB) {
		err := db.Close()
		if err != nil {
			fmt.Println(err)
		}
	}(db)

	stateFilter := parseStates(states)
	tmpDir, err := os.MkdirTemp("", "gnaf-psv-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer func(path string) {
		err := os.RemoveAll(path)
		if err != nil {
			fmt.Println(err)
		}
	}(tmpDir)

	for _, target := range psvTargets {
		filter := stateFilter
		if target.noStateFilter {
			filter = nil
		}

		paths, err := extractMatchingFiles(zipPath, tmpDir, target.suffix, target.exclude, filter)
		if err != nil {
			return fmt.Errorf("extract %s: %w", target.suffix, err)
		}
		if len(paths) == 0 {
			return fmt.Errorf("no files matched suffix %q in zip", target.suffix)
		}

		for i, p := range paths {
			escapedPath := strings.ReplaceAll(p, `\`, `/`)
			var stmt string
			if i == 0 {
				stmt = fmt.Sprintf(
					`CREATE TABLE %s AS SELECT * FROM read_csv('%s', delim='|', header=true, all_varchar=true)`,
					target.table, escapedPath,
				)
			} else {
				stmt = fmt.Sprintf(
					`INSERT INTO %s SELECT * FROM read_csv('%s', delim='|', header=true, all_varchar=true)`,
					target.table, escapedPath,
				)
			}
			slog.Info("loading", "table", target.table, "file", filepath.Base(p))
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				return fmt.Errorf("load %s into %s: %w", filepath.Base(p), target.table, err)
			}
		}

		var count int
		err = db.QueryRowContext(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s", target.table)).Scan(&count)
		if err != nil {
			return err
		}
		slog.Info("loaded staging table", "table", target.table, "rows", count)
	}

	// Step 1: join all tables into a flat view first.
	slog.Info("joining addresses")
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE addresses_joined AS
		SELECT
			ad.address_detail_pid,
			TRIM(
				COALESCE(ad.building_name || ' ', '') ||
				COALESCE(ad.flat_number || '/', '') ||
				COALESCE(ad.number_first, '') ||
				CASE WHEN ad.number_last IS NOT NULL AND ad.number_last != ''
					THEN '-' || ad.number_last ELSE '' END || ' ' ||
				COALESCE(sl.street_name, '') || ' ' ||
				COALESCE(sl.street_type_code, '') || ' ' ||
				COALESCE(l.locality_name, '') || ' ' ||
				COALESCE(s.state_abbreviation, '') || ' ' ||
				COALESCE(ad.postcode, '')
			) AS full_address,
			ad.number_first,
			ad.number_last,
			ad.flat_number,
			ad.level_number,
			ad.building_name,
			ad.lot_number,
			sl.street_name,
			sl.street_type_code,
			l.locality_name,
			s.state_abbreviation,
			ad.postcode,
			TRY_CAST(g.latitude AS DOUBLE)  AS latitude,
			TRY_CAST(g.longitude AS DOUBLE) AS longitude,
			g.geocode_type_code             AS geocode_type,
			ad.confidence,
			ad.alias_principal,
			ad.primary_secondary,
			ad.legal_parcel_id,
			ad.date_created,
			(ad.flat_number IS NULL OR ad.flat_number = '') AS is_principal,
			TRIM(
				COALESCE(ad.building_name || ' ', '') ||
				COALESCE(ad.flat_number || ' ', '') ||
				COALESCE(ad.level_number || ' ', '') ||
				COALESCE(ad.number_first, '') || ' ' ||
				COALESCE(sl.street_name, '') || ' ' ||
				COALESCE(sl.street_type_code, '') || ' ' ||
				COALESCE(l.locality_name, '') || ' ' ||
				COALESCE(s.state_abbreviation, '') || ' ' ||
				COALESCE(ad.postcode, '')
			) AS search_text,
			CASE g.geocode_type_code
				WHEN 'PROPERTY'          THEN 0
				WHEN 'FRONTAGE MIDPOINT' THEN 1
				WHEN 'GAP GEOCODE'       THEN 2
				WHEN 'STREET'            THEN 3
				ELSE                          4
			END AS geocode_rank
		FROM raw_address_detail ad
		LEFT JOIN raw_geocode g          ON g.address_detail_pid   = ad.address_detail_pid
		LEFT JOIN raw_street_locality sl ON sl.street_locality_pid = ad.street_locality_pid
		LEFT JOIN raw_locality l         ON l.locality_pid         = ad.locality_pid
		LEFT JOIN raw_state s            ON s.state_pid            = l.state_pid
		WHERE ad.date_retired IS NULL OR ad.date_retired = ''
	`); err != nil {
		return fmt.Errorf("join addresses: %w", err)
	}

	// Step 2: dedup by full_address, keeping the best geocode per address.
	slog.Info("deduplicating addresses")
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE addresses AS
		SELECT
			address_detail_pid, full_address, number_first, number_last,
			flat_number, level_number, building_name, lot_number,
			street_name, street_type_code, locality_name, state_abbreviation,
			postcode, latitude, longitude, geocode_type, confidence,
			alias_principal, primary_secondary, legal_parcel_id,
			date_created, is_principal, search_text
		FROM (
			SELECT *,
				ROW_NUMBER() OVER (
					PARTITION BY full_address
					ORDER BY is_principal DESC, geocode_rank, address_detail_pid
				) AS rn
			FROM addresses_joined
		)
		WHERE rn = 1
	`); err != nil {
		return fmt.Errorf("dedup addresses: %w", err)
	}

	if _, err := db.ExecContext(ctx, `DROP TABLE addresses_joined`); err != nil {
		return fmt.Errorf("drop joined table: %w", err)
	}

	var count int
	err = db.QueryRowContext(ctx, "SELECT COUNT(*) FROM addresses").Scan(&count)
	if err != nil {
		return err
	}
	slog.Info("addresses built and deduplicated", "count", count)

	return nil
}

// exportToSQLite reads the addresses table from DuckDB and writes it to a
// SQLite database with an FTS5 virtual table for fast full-text search.
func exportToSQLite(ctx context.Context, duckPath, sqlitePath string) error {
	duck, err := sql.Open("duckdb", duckPath)
	if err != nil {
		return fmt.Errorf("open duckdb for export: %w", err)
	}
	defer func(duck *sql.DB) {
		err := duck.Close()
		if err != nil {
			fmt.Println(err)
		}
	}(duck)

	lite, err := sql.Open("sqlite3", sqlitePath)
	if err != nil {
		return fmt.Errorf("open sqlite: %w", err)
	}
	defer func(lite *sql.DB) {
		err := lite.Close()
		if err != nil {
			fmt.Println(err)
		}
	}(lite)

	// WAL mode and bulk-insert optimisations.
	for _, pragma := range []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA synchronous=OFF`,
		`PRAGMA cache_size=-65536`,
		`PRAGMA temp_store=MEMORY`,
	} {
		if _, err := lite.ExecContext(ctx, pragma); err != nil {
			return fmt.Errorf("pragma %s: %w", pragma, err)
		}
	}

	// Regular addresses table for column storage.
	if _, err := lite.ExecContext(ctx, `
		CREATE TABLE addresses (
			id                 INTEGER PRIMARY KEY,
			address_detail_pid TEXT NOT NULL,
			full_address       TEXT NOT NULL,
			number_first       TEXT,
			street_name        TEXT,
			street_type_code   TEXT,
			locality_name      TEXT,
			state_abbreviation TEXT,
			postcode           TEXT,
			latitude           REAL,
			longitude          REAL,
			is_principal       INTEGER NOT NULL DEFAULT 1,
			search_text        TEXT
		)
	`); err != nil {
		return fmt.Errorf("create addresses table: %w", err)
	}

	// FTS5 content table — reads column data from addresses table at query time.
	// This avoids storing data twice while still supporting fast FTS lookups.
	if _, err := lite.ExecContext(ctx, `
		CREATE VIRTUAL TABLE addresses_fts USING fts5(
			search_text,
			content=addresses,
			content_rowid=id,
			tokenize='unicode61'
		)
	`); err != nil {
		return fmt.Errorf("create fts5 table: %w", err)
	}

	// Stream rows from DuckDB into SQLite in batches.
	slog.Info("streaming rows from DuckDB to SQLite")
	rows, err := duck.QueryContext(ctx, `
		SELECT
			address_detail_pid, full_address, number_first,
			street_name, street_type_code, locality_name, state_abbreviation,
			postcode, latitude, longitude, is_principal::INTEGER, search_text
		FROM addresses
		ORDER BY address_detail_pid
	`)
	if err != nil {
		return fmt.Errorf("query duckdb: %w", err)
	}
	defer func(rows *sql.Rows) {
		err := rows.Close()
		if err != nil {
			fmt.Println(err)
		}
	}(rows)

	const insertSQL = `
		INSERT INTO addresses (
			address_detail_pid, full_address, number_first,
			street_name, street_type_code, locality_name, state_abbreviation,
			postcode, latitude, longitude, is_principal, search_text
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)
	`

	tx, err := lite.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, insertSQL)
	if err != nil {
		err := tx.Rollback()
		if err != nil {
			return err
		}
		return fmt.Errorf("prepare: %w", err)
	}

	count := 0
	batchCount := 0

	commitBatch := func() error {
		err := stmt.Close()
		if err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit: %w", err)
		}
		tx, err = lite.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin tx: %w", err)
		}
		stmt, err = tx.PrepareContext(ctx, insertSQL)
		if err != nil {
			return fmt.Errorf("prepare: %w", err)
		}
		batchCount = 0
		return nil
	}

	for rows.Next() {
		var (
			pid, fullAddress, searchText        string
			numberFirst, streetName, streetType sql.NullString
			locality, state, postcode           sql.NullString
			lat, lon                            sql.NullFloat64
			isPrincipal                         int64
		)
		if err := rows.Scan(
			&pid, &fullAddress, &numberFirst,
			&streetName, &streetType, &locality, &state,
			&postcode, &lat, &lon, &isPrincipal, &searchText,
		); err != nil {
			return fmt.Errorf("scan row: %w", err)
		}

		if _, err := stmt.ExecContext(ctx,
			pid, fullAddress, numberFirst.String,
			streetName.String, streetType.String, locality.String, state.String,
			postcode.String, lat, lon, isPrincipal, searchText,
		); err != nil {
			err := tx.Rollback()
			if err != nil {
				return err
			}
			return fmt.Errorf("insert: %w", err)
		}

		count++
		batchCount++
		if batchCount >= batchSize {
			if err := commitBatch(); err != nil {
				return err
			}
			if count%500_000 == 0 {
				slog.Info("export progress", "rows", count)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rows error: %w", err)
	}

	err = stmt.Close()
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("final commit: %w", err)
	}
	slog.Info("exported rows to SQLite", "count", count)

	// Build the FTS5 index from the content table.
	slog.Info("building FTS5 index")
	start := time.Now()
	if _, err := lite.ExecContext(ctx, `INSERT INTO addresses_fts(rowid, search_text) SELECT id, search_text FROM addresses`); err != nil {
		return fmt.Errorf("build fts5 index: %w", err)
	}
	slog.Info("FTS5 index built", "elapsed", time.Since(start).Round(time.Second))

	// Indexes for state/number filtering after FTS match.
	for _, idx := range []string{
		`CREATE INDEX idx_state ON addresses (state_abbreviation)`,
		`CREATE INDEX idx_number ON addresses (number_first)`,
		`CREATE INDEX idx_postcode ON addresses (postcode)`,
	} {
		if _, err := lite.ExecContext(ctx, idx); err != nil {
			return fmt.Errorf("create index: %w", err)
		}
	}
	slog.Info("indexes built")

	// Optimise the FTS index for read performance.
	if _, err := lite.ExecContext(ctx, `INSERT INTO addresses_fts(addresses_fts) VALUES('optimize')`); err != nil {
		slog.Warn("fts5 optimize failed", "error", err)
	}

	return nil
}

func extractMatchingFiles(zipPath, destDir, suffix string, exclude []string, stateFilter map[string]bool) ([]string, error) {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, fmt.Errorf("open zip: %w", err)
	}
	defer func(r *zip.ReadCloser) {
		err := r.Close()
		if err != nil {
			fmt.Println(err)
		}
	}(r)

	var paths []string
	for _, f := range r.File {
		cleanName := strings.Trim(f.Name, `"`)
		base := strings.ToUpper(filepath.Base(cleanName))

		if !strings.HasSuffix(base, strings.ToUpper(suffix)) {
			continue
		}

		excluded := false
		for _, ex := range exclude {
			if strings.Contains(base, strings.ToUpper(ex)) {
				excluded = true
				break
			}
		}
		if excluded {
			continue
		}

		if len(stateFilter) > 0 {
			matched := false
			for state := range stateFilter {
				if strings.HasPrefix(base, state+"_") {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}

		destPath := filepath.Join(destDir, filepath.Base(cleanName))
		slog.Info("extracting", "file", cleanName)
		if err := extractZipFile(f, destPath); err != nil {
			return nil, fmt.Errorf("extract %s: %w", cleanName, err)
		}
		paths = append(paths, destPath)
	}
	return paths, nil
}

func extractZipFile(f *zip.File, dest string) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer func(rc io.ReadCloser) {
		err := rc.Close()
		if err != nil {
			fmt.Println(err)
		}
	}(rc)
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer func(out *os.File) {
		err := out.Close()
		if err != nil {
			fmt.Println(err)
		}
	}(out)
	_, err = io.Copy(out, rc)
	return err
}

func parseStates(states string) map[string]bool {
	result := make(map[string]bool)
	if states == "" {
		return result
	}
	for _, s := range strings.Split(states, ",") {
		s = strings.TrimSpace(strings.ToUpper(s))
		if s != "" {
			result[s] = true
		}
	}
	return result
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
