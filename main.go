package main

import (
	"log"
	"net/http/cookiejar"
	"net/url"
)

func main() {
	/*
		urls, err := discoverTheses("urls.txt")
		if err != nil {
			log.Fatalf("failed to scrape theses urls: %v", err)
		}

		metas, err := exploreTheses(urls)
	*/

	u, err := newURL("/bitstream/10603/402368/2/02_declaration.pdf")
	if err != nil {
		log.Fatal(err)
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		log.Fatal(err)
	}

	absUrlStr, err := url.JoinPath("https://shodhganga.inflibnet.ac.in/", u.String())
	if err != nil {
		log.Fatal(err)
	}

	absUrl, err := newURL(absUrlStr)
	if err != nil {
		log.Fatal(err)
	}

	if err = bypassAnubis(absUrl, jar); err != nil {
		log.Fatal(err)
	}

	if err = downloadPDF(u, jar); err != nil {
		log.Fatal(err)
	}
}
