package main

import (
	"log"
)

func main() {
	urls, err := discoverTheses("urls.txt")
	if err != nil {
		log.Fatalf("Failed to scrape thesis urls: %v", err)
	}

	metas, err := exploreTheses(urls)
	if err != nil {
		log.Fatalf("Failed to scrape meta: %v", err)
	}

	downloadCount, err := downloadPDFs(metas, true)
	if err != nil {
		log.Fatalf("Failed to download PDFs: %v", err)
	}

	log.Printf("Downloaded %d PDFs to disk", downloadCount)

	if err = consolidateMetas(metas); err != nil {
		log.Fatalf("Failed to merge PDFs: %v", err)
	}

	log.Printf("Consolidated %d PDFs to disk", len(metas))
}
