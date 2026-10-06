package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// downloadPDF represents a single unit of concurrency.
// Extracts 'group', 'item' and 'file' to determine the path to save the file
// Sample: '/bitstream/<group>/<item>/<no>/<no>_<filename>.pdf'
func downloadPDF(u _url) error {
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

	fpath := filepath.Join(".", grp, item, fname)
	upath, err := url.JoinPath("https://shodhganga.inflibnet.ac.in/", urlStr)
	if err != nil {
		return fmt.Errorf("url join: %w", err)
	}

	// If already downloaded, skip downloading
	if _, err := os.Stat(fpath); err == nil {
		return nil
	}

	log.Printf("Downloading: %q -> %q", upath, fpath)
	req, err := http.NewRequest(http.MethodGet, upath, nil)
	if err != nil {
		return fmt.Errorf("new request: %w", err)
	}

	// Masquerade as standard web browser
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/webp,application/pdf,*/*;q=0.8")

	client := http.Client{}
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
