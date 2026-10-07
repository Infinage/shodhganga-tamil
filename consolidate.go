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

	"github.com/pdfcpu/pdfcpu/pkg/api"
)

var uriRegexExp = regexp.MustCompile(`^http://hdl\.handle\.net/(\d+)/(\d+)$`)
var fnameRegexExp = regexp.MustCompile(`^/bitstream/(\d+)/(\d+)/\d+/(\d+)([^/]+)$`)

// consolidatMeta processes a single meta directory and merges all constituent
// PDFs into a single PDF.
//
//  1. If merged file exists, skip the meta after logging it.
//  2. Fail even if one of the PDFs do not have a numeric prefix.
//  3. Sort based on numeric prefix.
//  4. Extension is not PDF, check if it is still a PDF. If yes add otherwise ignore.
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

	// Slices of Filepath, Order
	expectedFiles := make([][2]string, 0, len(m.Files))
	for _, u := range m.Files {
		matches := fnameRegexExp.FindStringSubmatch(u.String())
		if len(matches) != 5 {
			return fmt.Errorf("filename format: %q", u.String())
		} else if matches[1] != group || matches[2] != item {
			return fmt.Errorf("want: group: %d, item: %d; got: group: %d, item: %d",
				group, item, matches[1], matches[2])
		}

		fnameUnescaped, err := url.PathUnescape(matches[3] + matches[4])
		if err != nil {
			return fmt.Errorf("path unescape: %w", err)
		}

		fpath := filepath.Join(matches[1], matches[2], fnameUnescaped)
		expectedFiles = append(expectedFiles, [2]string{fpath, matches[3]})
	}

	// Sort expected files based on file ordering provided
	slices.SortFunc(expectedFiles, func(a, b [2]string) int {
		no1, err1 := strconv.Atoi(a[1])
		no2, err2 := strconv.Atoi(b[1])
		if err1 != nil || err2 != nil {
			panic("non numeric filename prefix")
		}
		return no1 - no2
	})

	// Get the files for merging after double checking that file exists
	var inFiles []string
	for _, entry := range expectedFiles {
		fpath := entry[0]
		if _, err := os.Stat(fpath); errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("missing required file %q", fpath)
		} else if err != nil {
			return fmt.Errorf("file stat: %w", err)
		}

		if !strings.HasSuffix(fpath, ".pdf") {
			err := api.ValidateFile(context.Background(), fpath, nil, nil)
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

	err := api.MergeCreateFile(context.Background(), inFiles, opath, false, nil)
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
