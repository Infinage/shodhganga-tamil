package main

import (
	"log"
)

func main() {
	urls, err := discoverTheses("urls.txt")
	if err != nil {
		log.Fatalf("failed to scrape theses urls: %v", err)
	}

	metas, err := exploreTheses(urls)
	log.Println(metas)
}
