package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/PuerkitoBio/goquery"
)

var resultsCountRegex = regexp.MustCompile(`of\s+([\d,]+)`)

// countAvailableTheses makes a single GET request to shodhganga.inflibnet.ac.in
// to retrieve the total count of available tamil theses.
func countAvailableTheses() (int, error) {
	query := "https://shodhganga.inflibnet.ac.in/simple-search?" +
		"filter_field_1=language&filter_type_1=equals&filter_value_1=Tamil"

	resp, err := http.Get(query)
	if err != nil {
		return 0, fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("fetch: unexpected status %s", resp.Status)
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("goquery: %w", err)
	}

	// There are 2 page navs in results listing
	resultsCountTag := doc.Find(".discovery-result-pagination div:first-child").First()
	matches := resultsCountRegex.FindStringSubmatch(resultsCountTag.Text())
	if len(matches) != 2 {
		return 0, fmt.Errorf("extract result count from %q", resultsCountTag.Text())
	}

	resultsCount, err := strconv.Atoi(strings.ReplaceAll(matches[1], ",", ""))
	if err != nil {
		return 0, fmt.Errorf("invalid result count: %w", err)
	}

	return resultsCount, nil
}

// discoverThesesFromPage takes in a search query and scrapes the results for
// details url to explore further. Details url: 'handle/10603/77876'
func discoverThesesFromPage(u string) (urls []string, err error) {
	resp, err := http.Get(u)
	if err != nil {
		return nil, fmt.Errorf("fetch (%s): %w", u, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch: unexpected status %s", resp.Status)
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("goquery parse: %w", err)
	}

	cssSelector := ".discovery-result-results tbody tr:nth-child(n+2)"
	doc.Find(cssSelector).EachWithBreak(func(idx int, s *goquery.Selection) bool {
		titleLink := s.Find("td[headers='t2'] a")
		details, ok := titleLink.Attr("href")
		if !ok {
			err = fmt.Errorf("title has no href")
			return false
		}
		urls = append(urls, details)
		return true
	})

	return urls, err
}

// discoverTheses checks fpath for cached urls and loads it. Does a check on
// shodhganga to check for count of tamil theses, if counts mismatch, does a
// fresh pull of all the search results.
//
// Note: We may make this more efficient by sorting on the upload date and just
// retrieving the delta. This should do for now considering I only get 54 pages
// as of Oct 2026.
func discoverTheses(fpath string) (urls []string, err error) {
	// Try loading the cache
	if raw, err := os.ReadFile(fpath); err == nil {
		content := strings.TrimSpace(string(raw))
		if content != "" {
			urls = strings.Split(content, "\n")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read: %w", err)
	}

	last, err := countAvailableTheses()
	if err != nil {
		return nil, fmt.Errorf("count theses: %w", err)
	}

	// We already have all the urls parsed
	if len(urls) >= last {
		log.Println("URL count matches with cache, skipping discovery")
		return urls, nil
	}

	urls = nil // start fresh

	var mu sync.Mutex
	var wg sync.WaitGroup
	urlCh := make(chan string)

	for workerID := range 5 {
		wg.Go(func() {
			for u := range urlCh {
				us, err := discoverThesesFromPage(u)
				if err != nil {
					log.Printf("[Worker %d] Failed to discover %q: %v", workerID, u, err)
					continue
				}

				// Write all discovered urls
				log.Printf("[Worker %d] Theses urls scraped from %q", workerID, u)
				mu.Lock()
				urls = append(urls, us...)
				mu.Unlock()
			}
		})
	}

	for start := 0; start < last; start += 100 {
		urlCh <- fmt.Sprintf("https://shodhganga.inflibnet.ac.in/simple-search?"+
			"filter_field_1=language&filter_type_1=equals&filter_value_1=Tamil"+
			"&rpp=100&etal=0&start=%d", start)
	}

	close(urlCh)
	wg.Wait()

	if len(urls) != last {
		return nil, fmt.Errorf("url count: want %d, got %d", last, len(urls))
	}

	// Cache the results
	f, err := os.Create(fpath + ".tmp")
	if err != nil {
		os.Remove(fpath + ".tmp")
		return nil, fmt.Errorf("create: %w", err)
	}

	_, err = f.WriteString(strings.Join(urls, "\n"))
	if err != nil {
		f.Close()
		os.Remove(fpath + ".tmp")
		return nil, fmt.Errorf("file write: %w", err)
	}

	if err = f.Close(); err != nil {
		os.Remove(fpath + ".tmp")
		return nil, fmt.Errorf("file close: %w", err)
	}

	err = os.Rename(fpath+".tmp", fpath)
	if err != nil {
		os.Remove(fpath + ".tmp")
		return nil, fmt.Errorf("file rename: %w", err)
	}

	return urls, nil
}
