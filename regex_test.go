package main

import "testing"

func TestCheckURL(t *testing.T) {
	shouldMatch := []string{
		"https://release-assets.githubusercontent.com/github-production-release-asset/1/abc?sig=x",
		"https://objects.githubusercontent.com/foo/bar",
		"https://github-releases.githubusercontent.com/foo/bar",
		"https://codeload.github.com/cli/cli/tar.gz/refs/tags/v1",
		"https://media.githubusercontent.com/media/octocat/Hello-World/master/README",
		"https://media.githubusercontent.com/octocat/Hello-World/master/README",
		"https://github.com/a/b/releases/download/v1/x.zip",
		"https://raw.githubusercontent.com/a/b/main/c.txt",
		"https://api.github.com/repos/a/b",
	}
	for _, u := range shouldMatch {
		if checkURL(u) == nil {
			t.Errorf("checkURL 应命中但未命中: %s", u)
		}
	}
	shouldNotMatch := []string{
		"https://evil.example.com/a.zip",
		"https://example.com/",
		"https://github.com.evil.com/a/b",
		"https://notcodeload.github.com/a/b",
	}
	for _, u := range shouldNotMatch {
		if checkURL(u) != nil {
			t.Errorf("checkURL 不应命中但命中了: %s", u)
		}
	}
}

func TestIsEntryURL(t *testing.T) {
	// 入口：只有这些才允许重写 Location 返回客户端
	entry := []string{
		"https://github.com/a/b/releases/download/v1/x.zip",
		"https://raw.githubusercontent.com/a/b/main/c.txt",
		"https://gist.githubusercontent.com/u/1/raw/x.py",
		"https://api.github.com/repos/a/b",
	}
	for _, u := range entry {
		if !isEntryURL(u) {
			t.Errorf("isEntryURL 应命中: %s", u)
		}
	}
	// 非入口：资源 CDN 必须在服务端内部跟随，不能重写给客户端
	notEntry := []string{
		"https://release-assets.githubusercontent.com/github-production-release-asset/1/abc?sig=x",
		"https://objects.githubusercontent.com/foo/bar",
		"https://github-releases.githubusercontent.com/foo/bar",
		"https://codeload.github.com/cli/cli/tar.gz/refs/tags/v1",
		"https://media.githubusercontent.com/media/octocat/Hello-World/master/README",
	}
	for _, u := range notEntry {
		if isEntryURL(u) {
			t.Errorf("isEntryURL 不应命中（否则会泄露真实文件地址）: %s", u)
		}
	}
}
