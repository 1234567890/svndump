package main

import (
	"crypto/tls"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	_ "modernc.org/sqlite"
)

type nonRetryableError struct {
	err error
}

func (e *nonRetryableError) Error() string { return e.err.Error() }
func (e *nonRetryableError) Unwrap() error { return e.err }

type fileJob struct {
	localRelPath string
	sha1sum      string
}

type headerSliceFlag []string

func (h *headerSliceFlag) String() string {
	return fmt.Sprint(*h)
}

func (h *headerSliceFlag) Set(value string) error {
	*h = append(*h, value)
	return nil
}

var headers headerSliceFlag

func main() {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})

	dbPath := flag.String("d", "", "path to wc.db (omit to auto-download from the target)")
	baseURL := flag.String("u", "", "base URL of the target site")
	outputDir := flag.String("o", ".", "output directory for downloaded files")
	workers := flag.Int("t", 10, "number of concurrent download workers")
	maxRetries := flag.Int("r", 5, "maximum number of retry attempts per file")
	flag.Var(&headers, "H", "headers to add to each request (can be used multiple times)")
	flag.Parse()

	if *workers <= 0 {
		log.Fatal().Int("workers", *workers).Msg("number of workers (-t) must be positive")
	}
	if *maxRetries < 0 {
		log.Fatal().Int("retries", *maxRetries).Msg("number of retries (-r) must be non-negative")
	}

	if *baseURL == "" {
		log.Fatal().Msg("base URL (-u) is required")
	}

	client := createHTTPClient()

	if *dbPath == "" {
		parsed, err := url.Parse(*baseURL)
		if err != nil {
			log.Fatal().Err(err).Str("url", *baseURL).Msg("failed to parse base URL")
		}

		if *outputDir == "." {
			*outputDir = parsed.Hostname()
		}

		*dbPath = filepath.Join(*outputDir, "wc.db")
		wcdbURL := strings.TrimRight(*baseURL, "/") + "/.svn/wc.db"

		log.Info().Str("url", wcdbURL).Msg("downloading wc.db")
		if err := downloadFile(client, wcdbURL, *dbPath, headers); err != nil {
			log.Fatal().Err(err).Msg("failed to download wc.db")
		}
		log.Info().Str("path", *dbPath).Msg("wc.db downloaded")
	}

	if err := os.MkdirAll(*outputDir, 0o755); err != nil {
		log.Fatal().Err(err).Str("path", *outputDir).Msg("failed to create output directory")
	}

	db, err := sql.Open("sqlite", *dbPath)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to open database")
	}
	defer db.Close()

	query := `
		SELECT local_relpath, checksum
		FROM NODES
		WHERE kind='file' AND checksum IS NOT NULL
	`
	rows, err := db.Query(query)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to execute query")
	}
	defer rows.Close()

	var downloaded, failed atomic.Int64
	jobs := make(chan fileJob)
	var wg sync.WaitGroup

	for i := 0; i < *workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				url := constructURL(*baseURL, job.sha1sum)
				dest := filepath.Join(*outputDir, job.localRelPath)

				var lastErr error
				for attempt := 0; attempt <= *maxRetries; attempt++ {
					if attempt > 0 {
						base := time.Duration(1<<uint(attempt-1)) * time.Second
						jitter := time.Duration(rand.Int64N(int64(base / 2)))
						backoff := base + jitter
						log.Warn().Str("file", job.localRelPath).Int("attempt", attempt+1).Dur("backoff", backoff).Msg("retrying after error")
						time.Sleep(backoff)
					}

					lastErr = downloadFile(client, url, dest, headers)
					if lastErr == nil {
						break
					}
					var nre *nonRetryableError
					if errors.As(lastErr, &nre) {
						break
					}
				}

				if lastErr != nil {
					failed.Add(1)
					log.Error().Str("file", job.localRelPath).Int("attempts", *maxRetries+1).Err(lastErr).Msg("failed to download after all retries")
					continue
				}

				downloaded.Add(1)
				log.Info().Str("file", job.localRelPath).Msg("downloaded")
			}
		}()
	}

	for rows.Next() {
		var localRelPath string
		var checksum string

		if err := rows.Scan(&localRelPath, &checksum); err != nil {
			log.Warn().Err(err).Msg("failed to scan row, skipping")
			continue
		}

		sha1sum, err := extractSHA1FromChecksum(checksum)
		if err != nil {
			log.Warn().Str("file", localRelPath).Err(err).Msg("skipping file due to invalid checksum")
			continue
		}

		jobs <- fileJob{localRelPath: localRelPath, sha1sum: sha1sum}
	}

	close(jobs)
	wg.Wait()

	if err := rows.Err(); err != nil {
		log.Fatal().Err(err).Msg("error during row iteration")
	}

	log.Info().Int64("downloaded", downloaded.Load()).Int64("failed", failed.Load()).Msg("download complete")
}

func createHTTPClient() *http.Client {
	transport := &http.Transport{
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
	}

	return &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}
}

func extractSHA1FromChecksum(checksum string) (string, error) {
	const prefix = "$sha1$"
	if !strings.HasPrefix(checksum, prefix) {
		return "", fmt.Errorf("invalid checksum format: %s", checksum)
	}
	sha1 := checksum[len(prefix):]
	if len(sha1) < 3 {
		return "", fmt.Errorf("sha1 too short: %s", checksum)
	}
	return sha1, nil
}

func constructURL(baseURL, sha1sum string) string {
	dir := sha1sum[:2]
	return fmt.Sprintf("%s/.svn/pristine/%s/%s.svn-base", strings.TrimRight(baseURL, "/"), dir, sha1sum)
}

func downloadFile(client *http.Client, url, destPath string, headers []string) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return fmt.Errorf("failed to create directories: %w", err)
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	for _, header := range headers {
		parts := strings.SplitN(header, ":", 2)
		if len(parts) != 2 {
			return fmt.Errorf("invalid header format: %s", header)
		}
		req.Header.Add(strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1]))
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to download file: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, resp.Body)
		err := fmt.Errorf("bad status: %s", resp.Status)
		if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return &nonRetryableError{err: err}
		}
		return err
	}

	tmpFile, err := os.CreateTemp(filepath.Dir(destPath), ".svndump-*.tmp")
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}
	tmpPath := tmpFile.Name()

	_, copyErr := io.Copy(tmpFile, resp.Body)
	closeErr := tmpFile.Close()

	if copyErr != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to copy data to file: %w", copyErr)
	}
	if closeErr != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to close temp file: %w", closeErr)
	}

	if err := os.Rename(tmpPath, destPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("failed to rename temp file: %w", err)
	}

	return nil
}
