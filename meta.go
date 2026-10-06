package main

import (
	"encoding/json"
	"net/url"
)

// _url to support custom json marshal/unmarshal behaviour.
type _url struct {
	url.URL
}

// newURL is a convenience function to parse a string into '_url'.
func newURL(raw string) (ret _url, err error) {
	if u, err := url.Parse(raw); err == nil {
		ret.URL = *u
	}
	return ret, err
}

// UnmarshalJSON implements interface json.Unmarshaler for _url.
func (u *_url) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}

	_u, err := url.Parse(s)
	if err != nil {
		return err
	}

	u.URL = *_u
	return nil
}

// MarshalJSON implements interface json.Marshaler for _url.
func (u _url) MarshalJSON() ([]byte, error) {
	return json.Marshal(u.String())
}

// Captures all available data from 'handle/<group>/<item>'.
type meta struct {
	Title       string   `json:"title"`
	Researcher  string   `json:"researcher"`
	Guides      _url     `json:"guides"`
	Keywords    []string `json:"keywords"`
	University  string   `json:"university"`
	Completed   string   `json:"completed"`
	Abstract    string   `json:"abstract"`
	Pagination  string   `json:"pagination"`
	URI         _url     `json:"uri"`
	Departments []_url   `json:"departments"`
	Files       []_url   `json:"files"`
}
