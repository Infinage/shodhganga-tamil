package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-rod/rod"
)

var (
	errPDFAlreadyExists   = errors.New("pdf already exists")
	errAnubisAuthRequired = errors.New("anubis auth required")

	anubisMutex       sync.Mutex
	lastAnubisRefresh time.Time
)

// isAnubisChallenge checks if the returned response is an anubis POW challenge.
// This can only be solved with a JS enabled browser instance.
func isAnubisChallenge(resp *http.Response) bool {
	contentType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || contentType != "text/html" {
		return false
	}

	for _, sc := range resp.Header.Values("Set-Cookie") {
		c, err := http.ParseSetCookie(sc)
		if err != nil {
			continue
		}

		if strings.HasPrefix(c.Name, "techaro.lol-anubis-auth-") &&
			(c.Value == "" || c.MaxAge == -1) {
			return true
		}
	}

	return false
}

// bypassAnubis passes the url through a headless rod instance to submit the proof of work
// to bypass restriction. Note that only the download links are behind anubis protection,
// so need to pass those urls and not the domain directly.
//
// Expects input: '/bitstream/<group>/<item>/<no>/<no>_<filename>.pdf'
func bypassAnubis(u _url, jar *cookiejar.Jar) error {
	auStr, err := url.JoinPath("https://shodhganga.inflibnet.ac.in/", u.String())
	if err != nil {
		return fmt.Errorf("url join: %w", err)
	}

	// Modify this variable copy, just feeling lazy :)
	if u, err = newURL(auStr); err != nil {
		return err
	}

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

// refreshAnubis holds a mutex and checks for most recent refresh, if refreshed
// less than 10s skips the refresh.
//
// This is to handle scenarios where multiple downloader threads could simultaneously
// hit anubis challenge and  race to refresh the cookies.
func refreshAnubis(u _url, jar *cookiejar.Jar) error {
	anubisMutex.Lock()
	defer anubisMutex.Unlock()

	// Skip refresh if recently updated
	if !lastAnubisRefresh.IsZero() && time.Since(lastAnubisRefresh) < time.Second*10 {
		return nil
	}

	err := bypassAnubis(u, jar)
	if err != nil {
		return err
	}

	lastAnubisRefresh = time.Now()
	return nil
}

// downloadPDF extracts 'group', 'item' and 'file' to determine the path to save the
// file and downloads it to disk.
//
// It returns `errAnubisAuthRequired` when it hits the anubis challenge, which can be
// solved by calling `refreshAnubis`.
//
// If file already exists, it returns `errPDFAlreadyExists`.
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
		return errPDFAlreadyExists
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

	if isAnubisChallenge(resp) {
		return errAnubisAuthRequired
	}

	f, err := os.Create(fpath + ".tmp")
	if err != nil {
		return fmt.Errorf("create fail: %w", err)
	}

	if _, err = io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(fpath + ".tmp")
		return fmt.Errorf("write: %w", err)
	}

	if err = f.Close(); err != nil {
		os.Remove(fpath + ".tmp")
		return fmt.Errorf("file close: %w", err)
	}

	if err = os.Rename(fpath+".tmp", fpath); err != nil {
		os.Remove(fpath + ".tmp")
		return fmt.Errorf("rename: %w", err)
	}

	return nil
}

// downloadPDFs downloads all PDFs across the metas supplied and returns a count of
// downloaded PDFs. It returns an error in case of count mismatch. It auto handles
// anubis auth challenges.
func downloadPDFs(ms []meta) (int, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return 0, fmt.Errorf("cookiejar init: %w", err)
	}

	var downloadCounter atomic.Int32
	var wg sync.WaitGroup
	var urlCh = make(chan _url, 1)

	for workerID := range 20 {
		wg.Go(func() {
			for u := range urlCh {
				err := downloadPDF(u, jar)
				switch {
				case errors.Is(err, errPDFAlreadyExists):
					log.Printf("[Worker %d] PDF already downloaded: %q", workerID, u)

				case errors.Is(err, errAnubisAuthRequired):
					err = refreshAnubis(u, jar)
					if err != nil {
						log.Printf("[Worker %d] Fatal: Anubis refresh fail: %v", workerID, err)
						return
					}
					log.Printf("[Worker %d] Anubis auth successful", workerID)

					if err = downloadPDF(u, jar); err != nil {
						log.Printf("[Worker %d] Failed to download PDF %q after anubis auth",
							workerID, u, err)
						continue
					}

					downloadCounter.Add(1)
					log.Printf("[Worker %d]: Download successful: %q", workerID, u)

				case err != nil:
					log.Printf("[Worker %d] Failed to download PDF %q: %v", workerID, u, err)

				default:
					downloadCounter.Add(1)
					log.Printf("[Worker %d]: Download successful: %q", workerID, u)
				}
			}
		})
	}

	urlCount := 0
	for _, m := range ms {
		for _, u := range m.Files {
			urlCh <- u
			urlCount++
		}
	}

	// Wait for download to complete
	close(urlCh)
	wg.Wait()

	// Check total downloaded PDFs against input
	downloadCount := downloadCounter.Load()
	if downloadCount != int32(urlCount) {
		return 0, fmt.Errorf("url count: want %d, got %d", urlCount, downloadCount)
	}

	return int(downloadCount), nil
}
