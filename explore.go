package main

import (
	"encoding/json"
	"fmt"
	"html"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/PuerkitoBio/goquery"
)

// normalizeShodhgangaText unescapes title, guides that could be still
// escaped. For broken examples: 'handle/10603/218947'
func normalizeShodhgangaText(raw string) string {
	return html.UnescapeString(strings.ReplaceAll(raw, "and#", "&#"))
}

// newURLFromLinkTag is a convenience that checks for presence of href attr.
func newURLFromLinkTag(s *goquery.Selection) (_url, error) {
	href, ok := s.Attr("href")
	if !ok {
		return _url{}, fmt.Errorf("missing href")
	}

	return newURL(href)
}

// scrapeMeta parses the html goquery document and returns a meta struct.
func scrapeMeta(doc *goquery.Document) (meta, error) {
	var m meta
	var scrapeErr error

	// Fetch all the meta except for the 'Files' field
	doc.Find("table.itemDisplayTable tr").
		EachWithBreak(func(_ int, s *goquery.Selection) bool {
			tds := s.Find("td")
			if length := tds.Length(); length != 2 {
				scrapeErr = fmt.Errorf("expected 2 columns, got %d", length)
				return false
			}

			switch strings.TrimSpace(tds.Eq(0).Text()) {
			case "Title:":
				m.Title = normalizeShodhgangaText(strings.TrimSpace(tds.Eq(1).Text()))

			case "Researcher ID:", "Researcher:":
				m.Researcher = strings.TrimSpace(tds.Eq(1).Text())

			case "Guide(s):":
				guide, err := newURLFromLinkTag(tds.Eq(1).Find("a"))
				if err != nil {
					scrapeErr = fmt.Errorf("error parsing 'Guides': %w", err)
					return false
				}
				m.Guides = guide

			case "Keywords:":
				tds.Eq(1).Contents().Each(func(i int, s *goquery.Selection) {
					if goquery.NodeName(s) == "br" {
						return
					}

					content := strings.TrimSpace(s.Text())
					if content != "" {
						m.Keywords = append(m.Keywords, content)
					}
				})

			case "University:":
				m.University = strings.TrimSpace(tds.Eq(1).Text())

			case "Completed Date:":
				m.Completed = strings.TrimSpace(tds.Eq(1).Text())

			case "Abstract:":
				m.Abstract = strings.TrimSpace(tds.Eq(1).Text())

			case "Pagination:":
				m.Pagination = strings.TrimSpace(tds.Eq(1).Text())

			case "URI:":
				uriStr := strings.TrimSpace(tds.Eq(1).Text())
				u, err := newURL(uriStr)
				if err != nil {
					scrapeErr = fmt.Errorf("not a valid url (%s): %w", uriStr, err)
					break
				}
				m.URI = u

			case "Appears in Departments:":
				tds.Eq(1).Find("a").EachWithBreak(func(_ int, s *goquery.Selection) bool {
					u, err := newURLFromLinkTag(s)
					if err != nil {
						scrapeErr = fmt.Errorf("error parsing 'Appears in Departments:': %w", err)
						return false
					}

					m.Departments = append(m.Departments, u)
					return true
				})
			}

			return scrapeErr == nil
		})

	// Fetch the 'files' field
	fileSelector := "table.panel-body tr:nth-child(n+2) td[headers='t1'] a"
	doc.Find(fileSelector).EachWithBreak(func(i int, s *goquery.Selection) bool {
		u, err := newURLFromLinkTag(s)
		if err != nil {
			scrapeErr = fmt.Errorf("error parsing %d file url", i+1)
			return false
		}
		m.Files = append(m.Files, u)
		return true
	})

	return m, scrapeErr
}

// exploreThesis scrapes a single thesis url and extracts the metadata including
// the files that it contains.
func exploreThesis(id string) (m meta, err error) {
	if !strings.HasPrefix(id, "/handle/") {
		return meta{}, fmt.Errorf("expected format: '/handle/XXX/YYYY', got: %q", id)
	}

	// Create dir if it doesn't exist. eg: '10603/77844'
	dirPath := filepath.Join(".", strings.TrimPrefix(id, "/handle/"))
	if err := os.MkdirAll(dirPath, 0755); err != nil {
		return meta{}, fmt.Errorf("mkdir: %w", err)
	}

	// Load from cache if already exists
	metaPath := filepath.Join(dirPath, "meta.json")
	if raw, err := os.ReadFile(metaPath); err == nil {
		if err = json.Unmarshal(raw, &m); err != nil {
			return meta{}, fmt.Errorf("unmarshal meta: %w", err)
		}
		return m, nil
	}

	u, err := url.JoinPath("https://shodhganga.inflibnet.ac.in/", id)
	if err != nil {
		return meta{}, fmt.Errorf("url join: %w", err)
	}

	resp, err := http.Get(u)
	if err != nil {
		return meta{}, fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return meta{}, fmt.Errorf("fetch: unexpected status %s", resp.Status)
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return meta{}, fmt.Errorf("goquery: %w", err)
	}

	if m, err = scrapeMeta(doc); err != nil {
		return meta{}, fmt.Errorf("scrapeMeta: %w", err)
	}

	// Cache the meta info
	f, err := os.Create(metaPath + ".tmp")
	if err != nil {
		return meta{}, fmt.Errorf("create meta: %w", err)
	}

	// Write the meta as a json file
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err = enc.Encode(m); err != nil {
		f.Close()
		os.Remove(metaPath + ".tmp")
		return meta{}, fmt.Errorf("json encode: %w", err)
	}

	if err = f.Close(); err != nil {
		os.Remove(metaPath + ".tmp")
		return meta{}, fmt.Errorf("close meta: %w", err)
	}

	err = os.Rename(metaPath+".tmp", metaPath)
	if err != nil {
		os.Remove(metaPath + ".tmp")
		return meta{}, fmt.Errorf("file rename: %w", err)
	}

	return m, nil
}

// exploreTheses looks into each url passed and extracts the meta data into
// '<group>/<path>/meta.json'. It returns a list of all the scraped meta.
func exploreTheses(urls []string) (metas []meta, err error) {
	var wg sync.WaitGroup
	urlChan := make(chan string, 1)
	metaChan := make(chan meta, 1)

	// Spawn workers to explore each url
	for workerID := range 20 {
		wg.Go(func() {
			for u := range urlChan {
				m, err := exploreThesis(u)
				if err != nil {
					log.Printf("[Worker %d] Failed to process %q: %v", workerID, u, err)
					continue
				}

				log.Printf("[Worker %d] Metadata scraped for %q", workerID, u)
				metaChan <- m
			}
		})
	}

	// Feed the workers and close channel to signal queue is done
	go func() {
		for _, u := range urls {
			urlChan <- u
		}
		close(urlChan)
	}()

	// Wait for workers to finish first before closing their output channel
	go func() {
		wg.Wait()
		close(metaChan)
	}()

	// Read all processed metas and append to result
	for m := range metaChan {
		metas = append(metas, m)
	}

	// Enforce count matches after scraping
	if len(metas) != len(urls) {
		return metas, fmt.Errorf("metadata count: want %d, got %d", len(urls), len(metas))
	}

	return metas, nil
}
