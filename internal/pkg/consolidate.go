package shodhganga

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pdfcpu/pdfcpu/pkg/api"
)

var (
	uriRegexExp   = regexp.MustCompile(`^http://hdl\.handle\.net/(\d+)/(\d+)$`)
	fnameRegexExp = regexp.MustCompile(`^/bitstream/(\d+)/(\d+)/\d+/([^/]+)$`)
	reLeadingNum  = regexp.MustCompile(`^(\d+)[\s_.\-)]*(.*)`)
	intRegexExp   = regexp.MustCompile(`\d+`)

	whitespaceRepl    = strings.NewReplacer("\r\n", " ", "\n", " ", "\t", " ")
	abstractDataNoise = regexp.MustCompile(`(?i)newline|^(none|not available)$`)
)

// getFileWeight returns an integer to help sort the filenames
func getFileWeight(filepathStr string) int {
	baseName := filepath.Base(filepathStr)
	name := strings.ToLower(baseName)
	norm := strings.NewReplacer("_", " ", "-", " ", ".", " ").Replace(name)

	// Always at the front
	if strings.Contains(norm, "title") {
		return -1
	}

	// Always at the end
	if strings.Contains(norm, "recommendation") || strings.Contains(norm, "plagiarism") {
		return 9999
	}

	// Explicit leading numbers in the filename takes priority
	if m := reLeadingNum.FindStringSubmatch(name); len(m) > 1 {
		seq, _ := strconv.Atoi(m[1])
		return seq
	}

	// Simple keyword heuristics (ties are broken downstream by alphabetical order)
	switch {
	// Front mattter - always at the top
	case strings.Contains(norm, "cert"), strings.Contains(norm, "decl"),
		strings.Contains(norm, "ack"), strings.Contains(norm, "prelim"):
		return 0

	case strings.Contains(norm, "content"), strings.Contains(norm, "contant"),
		strings.Contains(norm, "index"):
		return 2

	case strings.Contains(norm, "abstract"), strings.Contains(norm, "abbreviation"):
		return 3

	case strings.Contains(norm, "intro"):
		return 5

	case strings.Contains(norm, "chapter"), strings.Contains(norm, "iyal"),
		strings.Contains(norm, "cha ") || strings.Contains(norm, "ch "):
		if m := intRegexExp.FindString(norm); m != "" {
			ch, _ := strconv.Atoi(m)
			return 10 + ch
		}
		return 20 // Fallback for unnumbered chapters

	case strings.Contains(norm, "conclu"), strings.Contains(norm, "coun"),
		strings.Contains(norm, "conu"):
		return 90

	case strings.Contains(norm, "biblio"), strings.Contains(norm, "referen"):
		return 91

	case strings.Contains(norm, "append"), strings.Contains(norm, "annex"):
		return 92
	}

	// Fallback: Extract upload sequence ID
	// Eg: "/bitstream/10603/725407/3/03. content.pdf" -> "3"
	parts := strings.Split(filepathStr, "/")
	if len(parts) >= 2 {
		seqStr := parts[len(parts)-2]
		if seq, err := strconv.Atoi(seqStr); err == nil {
			return 50 + seq // towards middle but ordered amongst themselves
		}
	}

	// unknown - towards middle
	return 50
}

// consolidatMeta processes a single meta directory and merges all constituent
// PDFs into a single PDF.
//
// On success, it returns the CSV representation of the meta data.
//
//  1. If merged file exists, skip the meta after logging it.
//  2. Sort based on `getFileWeight` logic.
//  3. Extension is not PDF, check if it is still a PDF. If yes add otherwise ignore.
func consolidateMeta(m Meta) (csvRecord []string, err error) {
	matches := uriRegexExp.FindStringSubmatch(m.URI.String())
	if len(matches) != 3 {
		return nil, fmt.Errorf("expected URI: `http://hdl.handle.net/<group>/<item>`")
	}

	// Extract the group number and item number
	group, item := matches[1], matches[2]
	ofile := fmt.Sprintf("%s-%s.pdf", matches[1], matches[2])
	identitifer := fmt.Sprintf("shodhganga-tamil-%s-%s", matches[1], matches[2])
	opath := filepath.Join("archive", ofile)

	// Parse the list of strings subjects into sep variables to
	//write as CSV fields
	var sub1, sub2, sub3 string
	switch len(m.Keywords) {
	default: // 3 and above
		sub3 = m.Keywords[2]
		fallthrough
	case 2:
		sub2 = m.Keywords[1]
		fallthrough
	case 1:
		sub1 = m.Keywords[0]
	case 0:
		// do nothing
	}

	titleNorm := whitespaceRepl.Replace(m.Title)
	absNorm := abstractDataNoise.ReplaceAllString(m.Abstract, " ")

	csvRecord = []string{
		identitifer,      // identitifer
		ofile,            // file
		absNorm,          // description
		sub1, sub2, sub3, // subject[...]
		titleNorm,    // title
		m.Researcher, // creator
		m.Completed,  // date
		"opensource", // collection
		"texts",      // mediatype
	}

	// If PDF already exists, it is as good as them being merged fresh
	if _, err := os.Stat(opath); err == nil {
		return csvRecord, errPDFAlreadyExists
	}

	// Slices of Filepaths
	expectedFiles := make([]string, 0, len(m.Files))
	for _, u := range m.Files {
		matches := fnameRegexExp.FindStringSubmatch(u.String())
		if len(matches) != 4 {
			return nil, fmt.Errorf("filename format: %q", u.String())
		} else if matches[1] != group || matches[2] != item {
			return nil, fmt.Errorf("want: group: %s, item: %s; got: group: %s, item: %s",
				group, item, matches[1], matches[2])
		}

		fnameUnescaped, err := url.PathUnescape(matches[3])
		if err != nil {
			return nil, fmt.Errorf("path unescape: %w", err)
		}

		fpath := filepath.Join(matches[1], matches[2], fnameUnescaped)
		expectedFiles = append(expectedFiles, fpath)
	}

	// Sort expected files based on file ordering provided
	slices.SortFunc(expectedFiles, func(a, b string) int {
		wa, wb := getFileWeight(a), getFileWeight(b)
		if wa != wb {
			return wa - wb
		}
		return strings.Compare(a, b) // Alphabetical tie breaker
	})

	// Get the files for merging after double checking that file exists
	var inFiles []string
	for _, fpath := range expectedFiles {
		if _, err := os.Stat(fpath); errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("missing required file %q", fpath)
		} else if err != nil {
			return nil, fmt.Errorf("file stat: %w", err)
		}

		if !strings.EqualFold(filepath.Ext(fpath), ".pdf") {
			timeoutCtx, cancel := context.WithTimeout(context.Background(), time.Minute*5)
			err := api.ValidateFile(timeoutCtx, fpath, nil, nil)
			cancel()
			if err != nil {
				log.Printf("Non PDF entry %q skipped: %v", fpath, err)
				continue
			}
			log.Printf("Valid PDF entry %q without extension added", fpath)
		}

		// Add entry in sorted order with '.pdf' extension or
		// without extension if it is a valid PDF.
		inFiles = append(inFiles, fpath)
	}

	// Try merging using pdfcpu
	timeoutCtx, cancel := context.WithTimeout(context.Background(), time.Minute*5)
	err = api.MergeCreateFile(timeoutCtx, inFiles, opath, false, nil)
	cancel()

	if err != nil {
		qpdfPath, qpdfErr := exec.LookPath("qpdf")
		if qpdfErr != nil {
			_ = os.Remove(opath)
			return nil, fmt.Errorf("pdf merge: %w; qpdf unavailable: %w", err, qpdfErr)
		}

		qpdfArgs := []string{"--empty", "--pages"}
		qpdfArgs = append(qpdfArgs, inFiles...)
		qpdfArgs = append(qpdfArgs, "--", opath)

		// On failure, attempt merging using qpdf
		timeoutCtx, cancel := context.WithTimeout(context.Background(), time.Minute*5)
		cmd := exec.CommandContext(timeoutCtx, qpdfPath, qpdfArgs...)
		op, qpdfErr := cmd.CombinedOutput()
		cancel()

		// qpdf sometimes returns warnings as exit code 3 which is okay
		exitErr, ok := errors.AsType[*exec.ExitError](qpdfErr)
		if qpdfErr != nil && (!ok || exitErr.ExitCode() != 3) {
			_ = os.Remove(opath)
			return nil, fmt.Errorf("pdf merge: pdfcpu err: %w | qpdf err: %w | qpdf output: %s",
				err, qpdfErr, op)
		}

		// Let it fall through on success
	}

	return csvRecord, nil
}

// ConsolidateMetas takes in all metas provided and consolidates the
// fragmented PDFs into a single unified PDF.
func ConsolidateMetas(metas []Meta) error {
	var wg sync.WaitGroup
	var mergeCounter atomic.Int32
	metaCh := make(chan Meta, 1)
	csvCh := make(chan []string)

	// Create a target directory for archive uploads
	if err := os.Mkdir("archive", 0755); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("mkdir archive: %w", err)
	}

	// Create a CSV file for bulk uploading via internet archive CLI
	// https://archive.org/developers/internetarchive/cli.html#bulk-uploading
	f, err := os.Create(filepath.Join("archive", "uploads.csv"))
	if err != nil {
		return fmt.Errorf("file create: %w", err)
	}
	defer f.Close()

	// Format: https://archive.org/download/ia-pex/uploading.csv
	w := csv.NewWriter(f)
	if err = w.Write([]string{
		"identifier",
		"file",
		"description",
		"subject[0]",
		"subject[1]",
		"subject[2]",
		"title",
		"creator",
		"date",
		"collection",
		"mediatype",
	}); err != nil {
		return fmt.Errorf("csv header: %w", err)
	}

	// Spawn workers to process each folder
	for workerID := range 8 {
		wg.Go(func() {
			for m := range metaCh {
				csvRecord, err := consolidateMeta(m)
				if err != nil {
					if !errors.Is(err, errPDFAlreadyExists) {
						log.Printf("[Worker %d] Merge fail %q: %v", workerID, m.URI.String(), err)
						continue
					}

					// In case PDF already exists, let it fallthrough
					log.Printf("Merged PDF %q already exists, skipping.", m.URI.String())
				}

				log.Printf("[Worker %d] Merged %q", workerID, m.URI.String())
				csvCh <- csvRecord
				mergeCounter.Add(1)
			}
		})
	}

	// Enqueue work and wait for their completion, signal csvCh
	// to close, main thread is listening on this.
	go func() {
		for _, m := range metas {
			metaCh <- m
		}

		close(metaCh)
		wg.Wait()

		close(csvCh)
	}()

	var writeErr error
	for record := range csvCh {
		if err = w.Write(record); err != nil {
			writeErr = fmt.Errorf("csv write: %w", err)
		}
	}

	w.Flush()
	if writeErr = errors.Join(writeErr, w.Error()); writeErr != nil {
		return writeErr
	}

	mergedCount := mergeCounter.Load()
	if int(mergedCount) != len(metas) {
		log.Printf("WARN: Merged %d / %d theses. %d failed (corrupt server files).",
			mergedCount, len(metas), len(metas)-int(mergedCount))
	}

	return nil
}
