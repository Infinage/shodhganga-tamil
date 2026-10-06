package main

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/pdfcpu/pdfcpu/pkg/api"
)

var uriRegexExp = regexp.MustCompile(`^http://hdl\.handle\.net/(\d+)/(\d+)$`)
var fnameRegexExp = regexp.MustCompile(`^/bitstream/(\d+)/(\d+)/\d+/([^/]+\.pdf)$`)

// consolidatMeta processes a single meta directory and merges all
// constituent PDFs into a single PDF.
func consolidateMeta(m meta) error {
	matches := uriRegexExp.FindStringSubmatch(m.URI.String())
	if len(matches) != 3 {
		return fmt.Errorf("expected URI: `http://hdl.handle.net/<group>/<item>`")
	}

	expectedFiles := make(map[string]struct{}, len(m.Files))
	for _, fname := range m.Files {
		matches := fnameRegexExp.FindStringSubmatch(fname.String())
		if len(matches) != 4 {
			return fmt.Errorf("expected filename: `/bitstream/<group>/<item>/<no>/<filename>`")
		}

		fnameUnescaped, err := url.PathUnescape(matches[3])
		if err != nil {
			return fmt.Errorf("path unescape: %w", err)
		}

		fpath := filepath.Join(matches[1], matches[2], fnameUnescaped)
		expectedFiles[fpath] = struct{}{}
	}

	dirPath := filepath.Join(".", matches[1], matches[2])
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return fmt.Errorf("read dir: %w", err)
	}

	// Already arrives in sorted order
	var inFiles []string
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".pdf") {
			fpath := filepath.Join(dirPath, entry.Name())
			if _, ok := expectedFiles[fpath]; ok {
				inFiles = append(inFiles, fpath)
				delete(expectedFiles, fpath)
			}
		}
	}

	if len(expectedFiles) != 0 {
		return fmt.Errorf("missing files: %v", expectedFiles)
	}

	ofile := fmt.Sprintf("%s-%s.pdf", matches[1], matches[2])
	opath := filepath.Join(dirPath, ofile)
	err = api.MergeCreateFile(context.Background(), inFiles, opath, false, nil)
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

	for workerID := range 5 {
		wg.Go(func() {
			for m := range metaCh {
				if err := consolidateMeta(m); err != nil {
					log.Printf("[Worker %d] Merge fail %q: %v", workerID, m.URI.String(), err)
					continue
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
