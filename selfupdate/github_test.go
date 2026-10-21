package selfupdate

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type releaseTransport func(*http.Request) (*http.Response, error)

func (f releaseTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFetchLatestUsesForkRelease(t *testing.T) {
	client := &http.Client{Transport: releaseTransport(func(r *http.Request) (*http.Response, error) {
		if got := r.URL.String(); got != "https://api.github.com/repos/forxiucn/ARTEX/releases?per_page=1" {
			t.Fatalf("unexpected update source: %s", got)
		}
		if r.Method != http.MethodGet || r.Header.Get("Accept") != "application/vnd.github+json" {
			t.Fatalf("invalid GitHub request: %s %v", r.Method, r.Header)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`[{
			"tag_name":"v0.3.16",
			"assets":[
				{"name":"artex-0.3.16-linux-arm64.zip","browser_download_url":"https://github.com/forxiucn/ARTEX/releases/download/v0.3.16/artex-0.3.16-linux-arm64.zip"},
				{"name":"SHA256SUMS","browser_download_url":"https://github.com/forxiucn/ARTEX/releases/download/v0.3.16/SHA256SUMS"}
			], "prerelease":true
		}]`))}, nil
	})}
	rel, err := FetchLatest(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{AssetName(rel.TagName, "linux", "arm64"), sumsAsset} {
		asset, ok := rel.FindAsset(name)
		if !ok {
			t.Fatalf("missing release asset %s", name)
		}
		body, err := get(context.Background(), &http.Client{Transport: releaseTransport(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader("asset"))}, nil
		})}, asset.URL)
		if err != nil {
			t.Fatalf("fork asset URL rejected: %v", err)
		}
		body.Close()
	}
}

func TestFetchLatestMissingRelease(t *testing.T) {
	client := &http.Client{Transport: releaseTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})}
	_, err := FetchLatest(context.Background(), client)
	if err == nil || !strings.Contains(err.Error(), "forxiucn/ARTEX") || !strings.Contains(err.Error(), "不可访问或尚未发布") {
		t.Fatalf("unexpected missing-release error: %v", err)
	}
}

func TestFetchLatestFallsBackToPublicFeedWhenRateLimited(t *testing.T) {
	requests := 0
	client := &http.Client{Transport: releaseTransport(func(r *http.Request) (*http.Response, error) {
		requests++
		switch r.URL.String() {
		case latestURL:
			return &http.Response{StatusCode: http.StatusForbidden, Body: io.NopCloser(strings.NewReader(`{"message":"API rate limit exceeded"}`))}, nil
		case latestFeedURL:
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`<?xml version="1.0"?><feed xmlns="http://www.w3.org/2005/Atom"><entry><id>tag:github.com,2008:Repository/1/v0.3.22-beta.1</id><title>ARTEX v0.3.22-beta.1</title><updated>2026-10-21T02:56:21Z</updated><link rel="alternate" href="https://github.com/forxiucn/ARTEX/releases/tag/v0.3.22-beta.1"/></entry></feed>`))}, nil
		case "https://github.com/forxiucn/ARTEX/releases/expanded_assets/v0.3.22-beta.1":
			page := `<a href="/forxiucn/ARTEX/releases/download/v0.3.22-beta.1/artex-0.3.22-beta.1-linux-arm64.zip"><span>artex-0.3.22-beta.1-linux-arm64.zip</span></a><a href="/forxiucn/ARTEX/releases/download/v0.3.22-beta.1/SHA256SUMS"><span>SHA256SUMS</span></a>`
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(page))}, nil
		default:
			t.Fatalf("unexpected request URL: %s", r.URL)
			return nil, nil
		}
	})}
	rel, err := FetchLatest(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 3 || rel.TagName != "v0.3.22-beta.1" || !rel.Prerelease {
		t.Fatalf("unexpected fallback result: requests=%d release=%+v", requests, rel)
	}
	if rel.HTMLURL != "https://github.com/forxiucn/ARTEX/releases/tag/v0.3.22-beta.1" {
		t.Fatalf("unexpected release URL: %s", rel.HTMLURL)
	}
	for _, name := range []string{AssetName(rel.TagName, "linux", "arm64"), sumsAsset} {
		asset, ok := rel.FindAsset(name)
		if !ok || !strings.HasPrefix(asset.URL, "https://github.com/forxiucn/ARTEX/releases/download/") {
			t.Fatalf("missing or invalid fallback asset %s: %+v", name, asset)
		}
	}
}
