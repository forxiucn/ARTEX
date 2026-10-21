package selfupdate

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Repo 是发布源。写死而不是做成配置项：更新源可配等于给任何能改配置的人一条
// 远程代码执行通道，对一个渗透测试平台来说这个口子开不得。
const Repo = "forxiucn/ARTEX"

// latestURL 查询最新已发布 Release（包括 beta 预发布），以便 beta 用户可从应用内继续更新。
// GitHub 不提供“最新预发布版”专用接口；列表接口会排除草稿并包含 prerelease。
const latestURL = "https://api.github.com/repos/" + Repo + "/releases?per_page=1"

// GitHub serves the public release feed and release assets without consuming API quota.
const latestFeedURL = "https://github.com/" + Repo + "/releases.atom"

// allowedHosts 限定升级链路能访问的域名。配合下面的 checkRedirect，
// 任何一跳被重定向到名单外的主机都会直接失败——这是防止 DNS 污染 / 中间人
// 把二进制换掉的第一道闸门，第二道是 SHA256SUMS 比对。
var allowedHosts = map[string]bool{
	"api.github.com":                       true,
	"github.com":                           true,
	"objects.githubusercontent.com":        true, // release 资产实际落地的对象存储
	"release-assets.githubusercontent.com": true,
	"raw.githubusercontent.com":            true,
}

// Release 是 GitHub Release 里我们关心的字段。
type Release struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	HTMLURL     string    `json:"html_url"`
	Assets      []Asset   `json:"assets"`
}

// Asset 是 Release 上挂的一个文件。
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// ParseProxy validates an optional update proxy without including credentials in errors.
func ParseProxy(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("代理地址格式无效")
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("代理协议须为 http、https、socks5 或 socks5h")
	}
	if u.Hostname() == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("代理地址须包含主机，且不能包含路径、查询或片段")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("代理端口须在 1–65535 之间")
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return nil, fmt.Errorf("代理端口不能为空")
	}
	return u, nil
}

// NewClient validates the proxy before opening any connection. Empty means direct,
// regardless of HTTP_PROXY / HTTPS_PROXY in the process environment.
func NewClient(proxy string) (*http.Client, error) {
	pu, err := ParseProxy(proxy)
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{
		ForceAttemptHTTP2:   true,
		TLSHandshakeTimeout: 15 * time.Second,
	}
	if pu != nil {
		tr.Proxy = http.ProxyURL(pu)
	}

	return &http.Client{
		Transport: tr,
		Timeout:   30 * time.Minute, // 下载整包，不能按请求级超时卡死
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("重定向次数过多")
			}
			return checkURL(req.URL)
		},
	}, nil
}

// checkURL 强制 https + 域名白名单。
func checkURL(u *url.URL) error {
	if u.Scheme != "https" {
		return fmt.Errorf("拒绝非 HTTPS 地址: %s", u.Scheme+"://"+u.Host)
	}
	if !allowedHosts[strings.ToLower(u.Hostname())] {
		return fmt.Errorf("拒绝非 GitHub 域名: %s", u.Hostname())
	}
	return nil
}

// FetchLatest 查询最新已发布版本（包含预发布版本）。
func FetchLatest(ctx context.Context, c *http.Client) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestURL, nil)
	if err != nil {
		return nil, err
	}
	if err := checkURL(req.URL); err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "artex-selfupdate")

	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("访问 GitHub 失败（可在版本与更新中配置更新代理）: %w", err)
	}

	switch {
	case resp.StatusCode == http.StatusForbidden, resp.StatusCode == http.StatusTooManyRequests:
		// 未认证的 GitHub API 是每 IP 每小时 60 次，共用出口 IP 时很容易撞上。
		resp.Body.Close()
		return fetchLatestFromFeed(ctx, c)
	case resp.StatusCode == http.StatusNotFound:
		resp.Body.Close()
		return nil, fmt.Errorf("仓库 %s 不可访问或尚未发布版本", Repo)
	case resp.StatusCode != http.StatusOK:
		resp.Body.Close()
		return nil, fmt.Errorf("GitHub 返回 %d", resp.StatusCode)
	}
	defer resp.Body.Close()

	var releases []Release
	if err := json.NewDecoder(resp.Body).Decode(&releases); err != nil {
		return nil, fmt.Errorf("解析 Release 失败: %w", err)
	}
	if len(releases) == 0 {
		return nil, fmt.Errorf("仓库 %s 尚未发布版本", Repo)
	}
	rel := releases[0]
	if strings.TrimSpace(rel.TagName) == "" {
		return nil, fmt.Errorf("Release 缺少 tag")
	}
	return &rel, nil
}

type atomFeed struct {
	Entries []struct {
		ID      string `xml:"id"`
		Title   string `xml:"title"`
		Updated string `xml:"updated"`
		Content string `xml:"content"`
		Links   []struct {
			Href string `xml:"href,attr"`
			Rel  string `xml:"rel,attr"`
		} `xml:"link"`
	} `xml:"entry"`
}

var releaseAssetLink = regexp.MustCompile(`(?i)<a\s+href="([^"]+/releases/download/[^\"]+)"[^>]*>\s*<span[^>]*>([^<]+)</span>`)

func parseReleaseAssets(page, tag string, rel *Release) error {
	expectedPrefix := "/" + Repo + "/releases/download/" + tag + "/"
	for _, match := range releaseAssetLink.FindAllStringSubmatch(page, -1) {
		href, err := url.PathUnescape(html.UnescapeString(match[1]))
		if err != nil || !strings.HasPrefix(href, expectedPrefix) {
			continue
		}
		name := strings.TrimSpace(html.UnescapeString(match[2]))
		if name == "" || strings.Contains(name, "/") || strings.Contains(name, "\\") {
			continue
		}
		u := &url.URL{Scheme: "https", Host: "github.com", Path: href}
		if err := checkURL(u); err != nil {
			continue
		}
		rel.Assets = append(rel.Assets, Asset{Name: name, URL: u.String()})
	}
	if len(rel.Assets) == 0 {
		return fmt.Errorf("Release feed 可用，但没有找到可下载资产")
	}
	return nil
}

func fetchLatestFromFeed(ctx context.Context, c *http.Client) (*Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, latestFeedURL, nil)
	if err != nil {
		return nil, err
	}
	if err := checkURL(req.URL); err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "artex-selfupdate")
	resp, err := c.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GitHub API 限流，且公开 Release feed 访问失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API 限流，公开 Release feed 返回 %d", resp.StatusCode)
	}
	var feed atomFeed
	if err := xml.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&feed); err != nil {
		return nil, fmt.Errorf("解析 GitHub Release feed 失败: %w", err)
	}
	if len(feed.Entries) == 0 {
		return nil, fmt.Errorf("仓库 %s 尚未发布版本", Repo)
	}
	entry := feed.Entries[0]
	tag := strings.TrimSpace(entry.ID[strings.LastIndex(entry.ID, "/")+1:])
	if _, ok := parseVersion(tag); !ok {
		return nil, fmt.Errorf("GitHub Release feed 中的版本标签无效")
	}
	rel := &Release{TagName: tag, Name: strings.TrimSpace(entry.Title), Body: entry.Content, Prerelease: IsPrereleaseVersion(tag)}
	if rel.Name == "" {
		rel.Name = tag
	}
	if entry.Updated != "" {
		rel.PublishedAt, _ = time.Parse(time.RFC3339, entry.Updated)
	}
	for _, link := range entry.Links {
		if link.Rel == "alternate" && link.Href != "" {
			u, parseErr := url.Parse(link.Href)
			if parseErr == nil && checkURL(u) == nil && u.Hostname() == "github.com" {
				rel.HTMLURL = u.String()
				break
			}
		}
	}
	if rel.HTMLURL == "" {
		rel.HTMLURL = "https://github.com/" + Repo + "/releases/tag/" + url.PathEscape(tag)
	}
	assetsURL := "https://github.com/" + Repo + "/releases/expanded_assets/" + url.PathEscape(tag)
	assetsURLParsed, err := url.Parse(assetsURL)
	if err != nil {
		return nil, err
	}
	if err := checkURL(assetsURLParsed); err != nil {
		return nil, err
	}
	assetsReq, err := http.NewRequestWithContext(ctx, http.MethodGet, assetsURL, nil)
	if err != nil {
		return nil, err
	}
	assetsResp, err := c.Do(assetsReq)
	if err != nil {
		return nil, fmt.Errorf("读取 Release 安装包列表失败: %w", err)
	}
	defer assetsResp.Body.Close()
	if assetsResp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("读取 Release 安装包列表失败: GitHub 返回 %d", assetsResp.StatusCode)
	}
	assetsHTML, err := io.ReadAll(io.LimitReader(assetsResp.Body, 2<<20))
	if err != nil {
		return nil, fmt.Errorf("读取 Release 安装包列表失败: %w", err)
	}
	if err := parseReleaseAssets(string(assetsHTML), tag, rel); err != nil {
		return nil, err
	}
	if _, ok := rel.FindAsset("SHA256SUMS"); !ok {
		return nil, fmt.Errorf("Release feed 中未找到 SHA256SUMS，拒绝提供更新")
	}
	return rel, nil
}

// AssetName 返回当前平台对应的发布包名，与 build.sh 的 package_binary 保持一致：
// artex-<版本>-<os>-<arch>.zip（版本号不带 v 前缀）。
func AssetName(tag, goos, goarch string) string {
	return fmt.Sprintf("artex-%s-%s-%s.zip", strings.TrimPrefix(tag, "v"), goos, goarch)
}

// FindAsset 在 Release 里按名字找资产。
func (r *Release) FindAsset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if strings.EqualFold(a.Name, name) {
			return a, true
		}
	}
	return Asset{}, false
}
