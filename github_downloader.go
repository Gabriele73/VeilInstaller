/*
 * SPDX-License-Identifier: GPL-3.0
 * Vencord Installer, a cross platform gui/cli app for installing Vencord
 * Copyright (c) 2023 Vendicated and Vencord contributors
 */

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	path "path/filepath"
	"regexp"
	"strings"
)

type GithubRelease struct {
	Name    string `json:"name"`
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name        string `json:"name"`
		DownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

var ReleaseData GithubRelease
var GithubError error
var GithubDoneChan chan bool

var InstalledHash = "None"
var LatestHash = "Unknown"
var IsDevInstall bool

func GetGithubRelease(url, fallbackUrl string) (*GithubRelease, error) {
	Log.Debug("Fetching", url)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		Log.Error("Failed to create Request", err)
		return nil, err
	}

	req.Header.Set("User-Agent", UserAgent)

	res, err := http.DefaultClient.Do(req)
	if err != nil {
		Log.Error("Failed to send Request", err)
		return nil, err
	}

	defer res.Body.Close()

	if res.StatusCode >= 300 {
		isRateLimitedOrBlocked := res.StatusCode == 401 || res.StatusCode == 403 || res.StatusCode == 429
		triedFallback := url == fallbackUrl

		// GitHub has a very strict 60 req/h rate limit and some (mostly indian) isps block github for some reason.
		// If that is the case, try our fallback at https://vencord.dev/releases/project
		if isRateLimitedOrBlocked && !triedFallback {
			Log.Error(fmt.Sprintf("Failed to fetch %s (status code %d). Trying fallback url %s", url, res.StatusCode, fallbackUrl))
			return GetGithubRelease(fallbackUrl, fallbackUrl)
		}

		err = errors.New(res.Status)
		Log.Error(url, "returned Non-OK status", GithubError)
		return nil, err
	}

	var data GithubRelease

	if err = json.NewDecoder(res.Body).Decode(&data); err != nil {
		Log.Error("Failed to decode GitHub JSON Response", err)
		return nil, err
	}

	return &data, nil
}

func InitGithubDownloader() {
	GithubDoneChan = make(chan bool, 1)

	IsDevInstall = os.Getenv("VENCORD_DEV_INSTALL") == "1"
	Log.Debug("Is Dev Install: ", IsDevInstall)
	if IsDevInstall {
		GithubDoneChan <- true
		return
	}

	go func() {
		// Make sure UI updates once the request either finished or failed
		defer func() {
			GithubDoneChan <- GithubError == nil
		}()

		data, err := GetGithubRelease(ReleaseUrl, ReleaseUrlFallback)
		if err != nil {
			GithubError = err
			return
		}

		ReleaseData = *data

		i := strings.LastIndex(data.Name, " ") + 1
		LatestHash = data.Name[i:]
		Log.Debug("Finished fetching GitHub Data")
		Log.Debug("Latest hash is", LatestHash, "Local Install is", Ternary(LatestHash == InstalledHash, "up to date!", "outdated!"))
	}()

	// directory containing patcher.js (or main.js for legacy DEV installs)
	VencordFile := VencordDirectory

	stat, err := os.Stat(VencordFile)
	if err != nil {
		return
	}

	if stat.IsDir() {
		if IsDevInstall {
			VencordFile = path.Join(VencordFile, "main.js")
		} else {
			VencordFile = path.Join(VencordFile, "patcher.js")
		}
	}

	// Check hash of installed version if exists
	b, err := os.ReadFile(VencordFile)
	if err != nil {
		return
	}

	Log.Debug("Found existing Vencord Install. Checking for hash...")

	re := regexp.MustCompile(`// Vencord (\w+)`)
	match := re.FindSubmatch(b)
	if match != nil {
		InstalledHash = string(match[1])
		Log.Debug("Existing hash is", InstalledHash)

	} else {
		Log.Debug("Didn't find hash")

	}
}

func installLatestBuilds() (retErr error) {
	Log.Debug("Installing latest builds...")

	if IsDevInstall {
		Log.Debug("Skipping due to dev install")
		return
	}

	requiredFiles := []string{"patcher.js", "preload.js", "renderer.js", "renderer.css"}

	assets := make(map[string]string)
	for _, ass := range ReleaseData.Assets {
		assets[ass.Name] = ass.DownloadURL
	}

	for _, name := range requiredFiles {
		if _, ok := assets[name]; !ok {
			retErr = errors.New("Didn't find " + name + " download link")
			Log.Error(retErr)
			return
		}
	}

	if err := os.MkdirAll(VencordDirectory, 0755); err != nil {
		Log.Error("Failed to create", VencordDirectory+":", err)
		retErr = err
		return
	}

	legacyAsar := VencordDirectory + ".asar"
	if _, err := os.Stat(legacyAsar); err == nil {
		Log.Debug("Removing legacy", legacyAsar)
		_ = os.Remove(legacyAsar)
	}

	for _, name := range requiredFiles {
		Log.Debug("Downloading", name)

		res, err := http.Get(assets[name])
		if err == nil && res.StatusCode >= 300 {
			err = errors.New(res.Status)
		}
		if err != nil {
			Log.Error("Failed to download "+name+":", err)
			retErr = err
			return
		}

		outPath := path.Join(VencordDirectory, name)
		out, err := os.OpenFile(outPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
		if err != nil {
			res.Body.Close()
			Log.Error("Failed to create", outPath+":", err)
			retErr = err
			return
		}
		_, err = io.Copy(out, res.Body)
		res.Body.Close()
		out.Close()
		if err != nil {
			Log.Error("Failed to write", outPath+":", err)
			retErr = err
			return
		}
	}

	pkgPath := path.Join(VencordDirectory, "package.json")
	if err := os.WriteFile(pkgPath, []byte(`{"name":"vencord","main":"patcher.js"}`), 0644); err != nil {
		Log.Error("Failed to write package.json:", err)
		retErr = err
		return
	}

	_ = FixOwnership(VencordDirectory)

	InstalledHash = LatestHash
	return
}
