package main

import (
	"log"
	shodhganga "shodhganga-tamil/internal/pkg"
)

func main() {
	urls, err := shodhganga.DiscoverTheses("urls.txt")
	if err != nil {
		log.Fatalf("Failed to scrape thesis urls: %v", err)
	}

	metas, err := shodhganga.ExploreTheses(urls)
	if err != nil {
		log.Fatalf("Failed to scrape meta: %v", err)
	}

	downloadCount, err := shodhganga.DownloadPDFs(metas, true)
	if err != nil {
		log.Fatalf("Failed to download PDFs: %v", err)
	}

	log.Printf("Downloaded %d PDFs to disk", downloadCount)

	if err = shodhganga.ConsolidateMetas(metas); err != nil {
		log.Fatalf("Failed to merge PDFs: %v", err)
	}

	log.Println("Scraping successful, please execute: " +
		"`ia upload --spreadsheet=uploads.csv --retries=100`")
}
