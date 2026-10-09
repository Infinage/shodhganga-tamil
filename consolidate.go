package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
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
	uriRegexExp = regexp.MustCompile(`^http://hdl\.handle\.net/(\d+)/(\d+)$`)
	fnameRegexExp = regexp.MustCompile(`^/bitstream/(\d+)/(\d+)/\d+/([^/]+)$`)
	reLeadingNum = regexp.MustCompile(`^(\d+)[\s_.\-)]*(.*)`)
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
		if m := regexp.MustCompile(`\d+`).FindString(norm); m != "" {
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
//  1. If merged file exists, skip the meta after logging it.
//  2. Sort based on `getFileWeight` logic.
//  3. Extension is not PDF, check if it is still a PDF. If yes add otherwise ignore.
func consolidateMeta(m meta) error {
	matches := uriRegexExp.FindStringSubmatch(m.URI.String())
	if len(matches) != 3 {
		return fmt.Errorf("expected URI: `http://hdl.handle.net/<group>/<item>`")
	}

	// Extract the group number and item number
	group, item := matches[1], matches[2]
	ofile := fmt.Sprintf("%s-%s.pdf", matches[1], matches[2])
	opath := filepath.Join(matches[1], matches[2], ofile)
	if _, err := os.Stat(opath); err == nil {
		return errPDFAlreadyExists
	}

	// Slices of Filepaths
	expectedFiles := make([]string, 0, len(m.Files))
	for _, u := range m.Files {
		matches := fnameRegexExp.FindStringSubmatch(u.String())
		if len(matches) != 4 {
			return fmt.Errorf("filename format: %q", u.String())
		} else if matches[1] != group || matches[2] != item {
			return fmt.Errorf("want: group: %d, item: %d; got: group: %d, item: %d",
				group, item, matches[1], matches[2])
		}

		fnameUnescaped, err := url.PathUnescape(matches[3])
		if err != nil {
			return fmt.Errorf("path unescape: %w", err)
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

	timeoutCtx, cancel := context.WithTimeout(context.Background(), time.Minute * 2)
	defer cancel()

	// Get the files for merging after double checking that file exists
	var inFiles []string
	for _, fpath := range expectedFiles {
		if _, err := os.Stat(fpath); errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("missing required file %q", fpath)
		} else if err != nil {
			return fmt.Errorf("file stat: %w", err)
		}

		if !strings.HasSuffix(fpath, ".pdf") {
			err := api.ValidateFile(timeoutCtx, fpath, nil, nil)
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

	err := api.MergeCreateFile(timeoutCtx, inFiles, opath, false, nil)
	if err != nil {
		return fmt.Errorf("pdf merge: %w", err)
	}

	return nil
}

// consolidateMetas takes in all metas provided and consolidates the
// fragmented PDFs into a single unified PDF.
func consolidateMetas(metas []meta) error {
	var wg sync.WaitGroup
	var mergeCounter atomic.Int32
	metaCh := make(chan meta, 1)

	for workerID := range 8 {
		wg.Go(func() {
			for m := range metaCh {
				err := consolidateMeta(m)
				if err != nil {
					if !errors.Is(err, errPDFAlreadyExists) {
						log.Printf("[Worker %d] Merge fail %q: %v", workerID, m.URI.String(), err)
						continue
					}
					log.Printf("Merged PDF %q already exists, skipping.", m.URI.String())
				}
				log.Printf("[Worker %d] Merged %q", workerID, m.URI.String())
				mergeCounter.Add(1)
			}
		})
	}

	for _, m := range metas {
		metaCh <- m
	}

	close(metaCh)
	wg.Wait()

	mergedCount := mergeCounter.Load()
	if int(mergedCount) != len(metas) {
		return fmt.Errorf("meta count: want %d, got %d", len(metas), mergedCount)
	}

	return nil
}
