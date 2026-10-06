package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-rod/rod"
)

// bypassAnubis passes the url through a headless rod instance to submit the proof of work
// to bypass restriction. Note that only the download links are behind anubis protection,
// so need to pass those urls and not the domain directly.
func bypassAnubis(u _url, jar *cookiejar.Jar) error {
	domain, err := url.Parse(u.Scheme + "://" + u.Hostname())
	if err != nil {
		return fmt.Errorf("domain parse: %w", err)
	}

	browser := rod.New().MustConnect()
	defer browser.Close()

	// Navigate to url
	page := browser.MustPage(u.String())

	timeout := time.NewTimer(time.Second * time.Duration(30))
	defer timeout.Stop()

	ticker := time.NewTicker(time.Millisecond * 250)
	defer ticker.Stop()

	outer:
	for {
		select {
		case <-ticker.C:
			for _, c := range page.MustCookies() {
				if strings.HasPrefix(c.Name, "techaro.lol-anubis-auth-") {
					break outer
				}
			}

		case <-timeout.C:
			return fmt.Errorf("anubis timeout waiting for POW")
		}
	}

	for _, c := range page.MustCookies() {
		if strings.HasPrefix(c.Name, "techaro.lol-anubis-auth-") {
			jar.SetCookies(domain, []*http.Cookie{{
				Name:     c.Name,
				Value:    c.Value,
				Domain:   c.Domain,
				Path:     c.Path,
				Secure:   c.Secure,
				HttpOnly: c.HTTPOnly,
			}})
			break
		}
	}

	log.Printf("Cookie jar updated: %v", jar.Cookies(domain))
	return nil
}

// downloadPDF extracts 'group', 'item' and 'file' to determine the path to save the file
// and downloads it into disk. It automatically bypasses anubis protection by using a
// headless browser session and using a shared http.CookieJar.
//
// Sample: '/bitstream/<group>/<item>/<no>/<no>_<filename>.pdf'
func downloadPDF(u _url, jar *cookiejar.Jar) error {
	urlStr := u.String()
	splits := strings.Split(urlStr, "/")
	if len(splits) != 6 {
		return fmt.Errorf("expected format '/bitstream/<group>/<item>/<no>/<no>_<filename>.pdf'")
	}

	grp, item := splits[2], splits[3]
	noStr, fname := fmt.Sprintf("%02s", splits[4]), splits[5]
	if !strings.HasPrefix(fname, noStr) || !strings.HasSuffix(fname, ".pdf") {
		return fmt.Errorf("expected filename: '<no>_<filename>.pdf'")
	}

	// If already downloaded, skip downloading
	fpath := filepath.Join(".", grp, item, fname)
	if _, err := os.Stat(fpath); err == nil {
		return os.ErrExist
	}

	upath, err := url.JoinPath("https://shodhganga.inflibnet.ac.in/", urlStr)
	if err != nil {
		return fmt.Errorf("url join: %w", err)
	}

	log.Printf("Downloading: %q -> %q", upath, fpath)
	req, err := http.NewRequest(http.MethodGet, upath, nil)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}

	// Without referer set it redirects to an analytics URL and download fails
	req.Header.Set("Referer", upath)

	client := http.Client{Jar: jar}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch fail: %w", err)
	} else if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return fmt.Errorf("fetch fail: status not ok: %d", resp.StatusCode)
	}
	defer resp.Body.Close()

	f, err := os.Create(fpath + ".tmp")
	if err != nil {
		return fmt.Errorf("create fail: %w", err)
	}
	defer f.Close()

	if _, err = io.Copy(f, resp.Body); err != nil {
		return fmt.Errorf("write: %w", err)
	}

	if err = os.Rename(fpath+".tmp", fpath); err != nil {
		return fmt.Errorf("rename: %w", err)
	}

	return nil
}
