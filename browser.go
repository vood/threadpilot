package threadpilot

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

type browserIdentity struct {
	LoggedIn  bool
	Reason    string
	Username  string
	UserID    string
	AccountID string
	TokenV2   string
}

type debuggerVersion struct {
	Browser              string `json:"Browser"`
	UserAgent            string `json:"User-Agent"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

type browserSessionMeta struct {
	ExecPath   string `json:"exec_path"`
	Port       int    `json:"port"`
	Headless   bool   `json:"headless"`
	Proxy      string `json:"proxy"`
	WSEndpoint string `json:"ws_endpoint"`
	StartedAt  string `json:"started_at,omitempty"`
	PID        int    `json:"pid,omitempty"`
}

var chromeMajorPattern = regexp.MustCompile(`(\d+)\.`)

func (a *app) runBrowserModeNative(mode string, extraEnv map[string]string) (payload map[string]interface{}, err error) {
	execPath, err := resolveChromeExecPath()
	if err != nil {
		return nil, err
	}

	allocOpts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(execPath),
		chromedp.UserDataDir(a.profileDir),
		chromedp.WindowSize(1400, 1100),
		chromedp.Flag("headless", a.headless),
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("no-default-browser-check", true),
		chromedp.Flag(
			"disable-features",
			"PrivateNetworkAccessSendPreflights,PrivateNetworkAccessRespectPreflightResults,PrivateNetworkAccessPermissionPrompt,BlockInsecurePrivateNetworkRequests",
		),
	)
	if strings.TrimSpace(a.proxy) != "" {
		allocOpts = append(allocOpts, chromedp.ProxyServer(strings.TrimSpace(a.proxy)))
	}

	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), allocOpts...)
	defer allocCancel()

	ctx, cancel := chromedp.NewContext(allocCtx)
	defer cancel()

	defer func() {
		if err != nil && a.holdOnError > 0 && !a.headless {
			time.Sleep(time.Duration(a.holdOnError) * time.Second)
		}
	}()

	if err = a.browserNavigate(ctx, redditBaseURL+"/"); err != nil {
		return nil, fmt.Errorf("bootstrap navigate failed: %w", err)
	}

	switch mode {
	case "login_phase":
		payload, err = a.browserLoginPhase(ctx, extraEnv)
	case "preflight":
		payload, err = a.browserPreflight(ctx, extraEnv)
	case "whoami":
		payload, err = a.browserWhoami(ctx)
	case "my_comments":
		payload, err = a.browserMyComments(ctx, extraEnv)
	case "my_replies":
		payload, err = a.browserMyReplies(ctx, extraEnv)
	case "my_posts":
		payload, err = a.browserMyPosts(ctx, extraEnv)
	case "my_subreddits":
		payload, err = a.browserMySubreddits(ctx, extraEnv)
	case "subscribe":
		payload, err = a.browserSubscribe(ctx, extraEnv)
	case "like":
		payload, err = a.browserLike(ctx, extraEnv)
	case "publish":
		payload, err = a.browserPublish(ctx, extraEnv)
	default:
		return nil, fmt.Errorf("unsupported browser mode: %s", mode)
	}
	if err != nil {
		return nil, fmt.Errorf("browser mode %q failed: %w", mode, err)
	}
	return payload, nil
}

func (a *app) ensureBrowserEndpoint(execPath string) (string, bool, error) {
	if strings.TrimSpace(a.profileDir) == "" {
		return "", false, errors.New("browser profile path is empty")
	}
	if err := os.MkdirAll(a.profileDir, 0o700); err != nil {
		return "", false, fmt.Errorf("unable to create browser profile directory: %w", err)
	}

	port := a.browserDebugPort()
	sessionPath := a.browserSessionPath()
	if meta, ok := readBrowserSessionMeta(sessionPath); ok {
		if meta.Port == port &&
			meta.Headless == a.headless &&
			strings.TrimSpace(meta.Proxy) == strings.TrimSpace(a.proxy) &&
			strings.TrimSpace(meta.WSEndpoint) != "" {
			if version, versionErr := fetchDebuggerVersion(port); versionErr == nil && strings.TrimSpace(version.WebSocketDebuggerURL) != "" {
				return strings.TrimSpace(version.WebSocketDebuggerURL), false, nil
			}
		}
	}

	if version, versionErr := fetchDebuggerVersion(port); versionErr == nil && strings.TrimSpace(version.WebSocketDebuggerURL) != "" {
		ws := strings.TrimSpace(version.WebSocketDebuggerURL)
		_ = writeBrowserSessionMeta(sessionPath, browserSessionMeta{
			ExecPath:   execPath,
			Port:       port,
			Headless:   a.headless,
			Proxy:      strings.TrimSpace(a.proxy),
			WSEndpoint: ws,
		})
		return ws, false, nil
	}

	pid, err := a.launchPersistentBrowser(execPath, port)
	if err != nil {
		return "", false, err
	}

	deadline := time.Now().Add(a.browserLaunchTimeout())
	for time.Now().Before(deadline) {
		version, versionErr := fetchDebuggerVersion(port)
		if versionErr == nil && strings.TrimSpace(version.WebSocketDebuggerURL) != "" {
			ws := strings.TrimSpace(version.WebSocketDebuggerURL)
			_ = writeBrowserSessionMeta(sessionPath, browserSessionMeta{
				ExecPath:   execPath,
				Port:       port,
				Headless:   a.headless,
				Proxy:      strings.TrimSpace(a.proxy),
				WSEndpoint: ws,
				StartedAt:  time.Now().UTC().Format(time.RFC3339),
				PID:        pid,
			})
			return ws, true, nil
		}
		time.Sleep(450 * time.Millisecond)
	}
	return "", false, fmt.Errorf("chrome did not expose DevTools endpoint on port %d within %s", port, a.browserLaunchTimeout().String())
}

func (a *app) launchPersistentBrowser(execPath string, port int) (int, error) {
	disableFeatures := []string{
		"PrivateNetworkAccessSendPreflights",
		"PrivateNetworkAccessRespectPreflightResults",
		"PrivateNetworkAccessPermissionPrompt",
		"BlockInsecurePrivateNetworkRequests",
	}

	args := []string{
		"--remote-debugging-address=127.0.0.1",
		fmt.Sprintf("--remote-debugging-port=%d", port),
		fmt.Sprintf("--user-data-dir=%s", a.profileDir),
		"--no-first-run",
		"--no-default-browser-check",
		"--window-size=1400,1100",
		"--disable-features=" + strings.Join(disableFeatures, ","),
		"about:blank",
	}
	if a.headless {
		args = append(args, "--headless=new")
	}
	if strings.TrimSpace(a.proxy) != "" {
		args = append(args, "--proxy-server="+strings.TrimSpace(a.proxy))
	}

	command := execPath
	commandArgs := args
	if shouldLaunchArm64Chrome(execPath) {
		command = "/usr/bin/arch"
		commandArgs = append([]string{"-arm64", execPath}, args...)
	}

	devNull, openErr := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if openErr != nil {
		return 0, openErr
	}
	defer devNull.Close()

	cmd := exec.Command(command, commandArgs...)
	cmd.Stdin = nil
	cmd.Stdout = devNull
	cmd.Stderr = devNull
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("chrome failed to start: %w", err)
	}
	pid := 0
	if cmd.Process != nil {
		pid = cmd.Process.Pid
		_ = cmd.Process.Release()
	}
	return pid, nil
}

func shouldLaunchArm64Chrome(execPath string) bool {
	if runtime.GOOS != "darwin" || !fileExists("/usr/bin/arch") {
		return false
	}
	hostArch := hostMachineArch()
	if hostArch != "arm64" {
		return false
	}
	if !fileExists("/usr/bin/lipo") {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/lipo", "-archs", execPath).CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "arm64")
}

func hostMachineArch() string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "uname", "-m").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.ToLower(string(out)))
}

func (a *app) browserSessionPath() string {
	legacyPath := filepath.Join(a.profileDir, ".redditcli-browser-session.json")
	newPath := filepath.Join(a.profileDir, ".threadpilot-browser-session.json")
	if fileExists(legacyPath) && !fileExists(newPath) {
		return legacyPath
	}
	return newPath
}

func (a *app) browserDebugPort() int {
	if value := envInt("REDDIT_BROWSER_PORT", 0); value > 0 {
		return value
	}
	base := filepath.Clean(a.profileDir) + "|" + strconv.FormatBool(a.headless) + "|" + strings.TrimSpace(a.proxy)
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(base))
	return 45000 + int(hash.Sum32()%1000)
}

func (a *app) browserLaunchTimeout() time.Duration {
	seconds := envInt("REDDIT_BROWSER_LAUNCH_TIMEOUT_SEC", 25)
	if seconds <= 0 {
		seconds = 25
	}
	return time.Duration(seconds) * time.Second
}

func readBrowserSessionMeta(path string) (browserSessionMeta, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return browserSessionMeta{}, false
	}
	var meta browserSessionMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return browserSessionMeta{}, false
	}
	return meta, true
}

func writeBrowserSessionMeta(path string, meta browserSessionMeta) error {
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func fetchDebuggerVersion(port int) (debuggerVersion, error) {
	endpoint := fmt.Sprintf("http://127.0.0.1:%d/json/version", port)
	client := &http.Client{
		Timeout: 2 * time.Second,
		Transport: &http.Transport{
			Proxy: nil,
		},
	}
	resp, err := client.Get(endpoint)
	if err != nil {
		return debuggerVersion{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return debuggerVersion{}, fmt.Errorf("devtools endpoint status: %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return debuggerVersion{}, err
	}
	var payload debuggerVersion
	if err := json.Unmarshal(body, &payload); err != nil {
		return debuggerVersion{}, err
	}
	return payload, nil
}

func resolveChromeExecPath() (string, error) {
	if raw := strings.TrimSpace(os.Getenv("CHROME_PATH")); raw != "" {
		if fileExists(raw) {
			return raw, nil
		}
		return "", fmt.Errorf("CHROME_PATH is set but not found: %s", raw)
	}

	candidates := discoverChromeCandidates()
	if len(candidates) == 0 {
		return "", errors.New("chrome executable not found; set CHROME_PATH to your Chrome/Chromium binary")
	}

	// Prefer a normally installed system Chrome/Chromium over cached
	// "Chrome for Testing" builds. The testing build on this machine crashes
	// during app registration on launch, while installed Chrome remains usable.
	if !envBool("REDDIT_PREFER_TESTING_CHROME") {
		bestSystem := ""
		bestSystemMajor := -1
		for _, candidate := range candidates {
			if !isSystemChromeCandidate(candidate) {
				continue
			}
			major := chromeMajorVersion(candidate)
			if major > bestSystemMajor {
				bestSystemMajor = major
				bestSystem = candidate
			}
		}
		if bestSystem != "" {
			return bestSystem, nil
		}
	}

	maxMajor := envInt("REDDIT_CDP_MAX_CHROME_MAJOR", 143)
	bestCompatible := ""
	bestCompatibleMajor := -1
	fallback := candidates[0]

	for _, candidate := range candidates {
		major := chromeMajorVersion(candidate)
		if major > 0 && major <= maxMajor && major > bestCompatibleMajor {
			bestCompatibleMajor = major
			bestCompatible = candidate
		}
	}
	if bestCompatible != "" {
		return bestCompatible, nil
	}
	return fallback, nil
}

func isSystemChromeCandidate(path string) bool {
	path = strings.TrimSpace(path)
	return strings.HasPrefix(path, "/Applications/")
}

func discoverChromeCandidates() []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(path string) {
		path = strings.TrimSpace(path)
		if path == "" || !fileExists(path) {
			return
		}
		if _, exists := seen[path]; exists {
			return
		}
		seen[path] = struct{}{}
		out = append(out, path)
	}

	home, _ := os.UserHomeDir()
	if strings.TrimSpace(home) != "" {
		patterns := []string{
			filepath.Join(home, "Library/Caches/ms-playwright/chromium-*", "chrome-mac-*", "Google Chrome for Testing.app/Contents/MacOS/Google Chrome for Testing"),
			filepath.Join(home, "Library/Caches/ms-playwright/chromium-*", "chrome-mac", "Chromium.app/Contents/MacOS/Chromium"),
		}
		var matches []string
		for _, pattern := range patterns {
			items, _ := filepath.Glob(pattern)
			matches = append(matches, items...)
		}
		sort.Strings(matches)
		for i := len(matches) - 1; i >= 0; i-- {
			add(matches[i])
		}
	}

	add("/Applications/Google Chrome.app/Contents/MacOS/Google Chrome")
	add("/Applications/Chromium.app/Contents/MacOS/Chromium")
	return out
}

func chromeMajorVersion(execPath string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, execPath, "--version").CombinedOutput()
	if err != nil {
		return 0
	}
	match := chromeMajorPattern.FindStringSubmatch(string(out))
	if len(match) < 2 {
		return 0
	}
	value, err := strconv.Atoi(match[1])
	if err != nil || value <= 0 {
		return 0
	}
	return value
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}

func hasStableBrowserIdentity(identity browserIdentity) bool {
	return strings.TrimSpace(identity.Username) != "" || strings.TrimSpace(identity.UserID) != ""
}

func (a *app) browserLoginPhase(ctx context.Context, extraEnv map[string]string) (map[string]interface{}, error) {
	loginUsername := strings.TrimSpace(extraEnv["REDDIT_LOGIN_USERNAME"])
	loginPassword := strings.TrimSpace(extraEnv["REDDIT_LOGIN_PASSWORD"])
	autoLoginRequested := loginUsername != "" && loginPassword != ""

	identity, err := a.browserIdentityFromSession(ctx)
	if err != nil {
		return nil, err
	}
	if identity.LoggedIn {
		url, _ := a.browserCurrentURL(ctx)
		if isLoginURL(url) || !hasStableBrowserIdentity(identity) {
			identity.LoggedIn = false
		} else {
			return map[string]interface{}{
				"transport":  "browser",
				"mode":       "login_phase",
				"status":     "already_logged_in",
				"loggedIn":   true,
				"reason":     identity.Reason,
				"url":        url,
				"username":   nullIfEmpty(identity.Username),
				"account_id": nullIfEmpty(identity.AccountID),
				"user_id":    nullIfEmpty(identity.UserID),
				"login_method": func() interface{} {
					if autoLoginRequested {
						return "already_logged_in_auto_skipped"
					}
					return "already_logged_in"
				}(),
			}, nil
		}
	}

	if a.headless && !autoLoginRequested {
		return nil, errors.New("login validation failed: browser profile is not logged in and headless mode is enabled (pass --username/--password for auto-login)")
	}
	if err := a.browserNavigate(ctx, "https://www.reddit.com/login/"); err != nil {
		return nil, err
	}

	autoSubmitStatus := "not_requested"
	if autoLoginRequested {
		if err := a.browserSubmitCredentials(ctx, loginUsername, loginPassword); err != nil {
			autoSubmitStatus = "submit_failed"
		} else {
			autoSubmitStatus = "submitted"
		}
	}

	deadline := time.Now().Add(time.Duration(a.loginWait) * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		identity, err = a.browserIdentityFromSession(ctx)
		if err != nil {
			return nil, err
		}
		if identity.LoggedIn {
			url, _ := a.browserCurrentURL(ctx)
			if isLoginURL(url) || !hasStableBrowserIdentity(identity) {
				continue
			}
			return map[string]interface{}{
				"transport":  "browser",
				"mode":       "login_phase",
				"status":     "logged_in",
				"loggedIn":   true,
				"reason":     identity.Reason,
				"url":        url,
				"username":   nullIfEmpty(identity.Username),
				"account_id": nullIfEmpty(identity.AccountID),
				"user_id":    nullIfEmpty(identity.UserID),
				"login_method": func() interface{} {
					if autoLoginRequested {
						return "auto_credentials"
					}
					return "manual"
				}(),
				"auto_submit_status": autoSubmitStatus,
			}, nil
		}
	}
	if autoLoginRequested {
		return nil, fmt.Errorf("login validation failed: timeout after %ds (automatic credential login attempted)", a.loginWait)
	}
	return nil, fmt.Errorf("login validation failed: timeout after %ds", a.loginWait)
}

func (a *app) browserSubmitCredentials(ctx context.Context, username, password string) error {
	username = strings.TrimSpace(username)
	password = strings.TrimSpace(password)
	if username == "" || password == "" {
		return errors.New("both username and password are required for auto login")
	}

	userLiteral, _ := json.Marshal(username)
	passLiteral, _ := json.Marshal(password)
	script := fmt.Sprintf(`(function () {
  const username = %s;
  const password = %s;
  function first(selectors) {
    for (const selector of selectors) {
      const node = document.querySelector(selector);
      if (node) return node;
    }
    return null;
  }
  function setValue(node, value) {
    node.focus();
    node.value = value;
    node.dispatchEvent(new Event("input", { bubbles: true }));
    node.dispatchEvent(new Event("change", { bubbles: true }));
  }
  const userNode = first([
    'input[name="username"]',
    'input[name="loginUsername"]',
    '#loginUsername',
    'input[type="email"]',
    'input[autocomplete="username"]'
  ]);
  const passNode = first([
    'input[name="password"]',
    '#loginPassword',
    'input[type="password"]',
    'input[autocomplete="current-password"]'
  ]);
  if (!userNode || !passNode) {
    return { ok: false, reason: "fields_not_found" };
  }

  setValue(userNode, username);
  setValue(passNode, password);

  const submit = first([
    'button[type="submit"]',
    'input[type="submit"]',
    'button.AnimatedForm__submitButton'
  ]);
  if (!submit) {
    return { ok: false, reason: "submit_not_found" };
  }
  submit.click();
  return { ok: true, reason: "submitted" };
})()`, string(userLiteral), string(passLiteral))

	var result struct {
		Ok     bool   `json:"ok"`
		Reason string `json:"reason"`
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(script, &result)); err != nil {
		return err
	}
	if !result.Ok {
		return fmt.Errorf("auto login submit failed: %s", firstNonEmpty(result.Reason, "unknown"))
	}
	return chromedp.Run(ctx, chromedp.Sleep(1200*time.Millisecond))
}

func (a *app) browserPreflight(ctx context.Context, extraEnv map[string]string) (map[string]interface{}, error) {
	identity, err := a.browserIdentityFromSession(ctx)
	if err != nil {
		return nil, err
	}
	if !identity.LoggedIn && identity.Reason == "token_v2_missing" {
		for i := 0; i < 5; i++ {
			if err := a.browserNavigate(ctx, redditBaseURL+"/"); err != nil {
				return nil, err
			}
			time.Sleep(2 * time.Second)
			identity, err = a.browserIdentityFromSession(ctx)
			if err != nil {
				return nil, err
			}
			if identity.LoggedIn {
				break
			}
		}
	}
	if !identity.LoggedIn {
		return nil, fmt.Errorf("preflight failed: browser profile is not logged in (%s)", identity.Reason)
	}

	urls := []string{
		"https://www.reddit.com/",
		"https://www.reddit.com/settings/account",
	}
	if raw := strings.TrimSpace(extraEnv["REDDIT_PREFLIGHT_URLS_JSON"]); raw != "" {
		var parsed []string
		if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
			return nil, fmt.Errorf("invalid REDDIT_PREFLIGHT_URLS_JSON: %w", err)
		}
		if len(parsed) > 0 {
			urls = urls[:0]
			for _, item := range parsed {
				item = strings.TrimSpace(item)
				if item == "" {
					continue
				}
				urls = append(urls, absoluteRedditURL(item))
			}
			if len(urls) == 0 {
				return nil, errors.New("REDDIT_PREFLIGHT_URLS_JSON resolved to an empty list")
			}
		}
	}

	checked := make([]map[string]string, 0, len(urls))
	for _, rawURL := range urls {
		target := absoluteRedditURL(rawURL)
		if err := a.browserNavigate(ctx, target); err != nil {
			return nil, err
		}
		finalURL, _ := a.browserCurrentURL(ctx)
		if isLoginURL(finalURL) {
			return nil, fmt.Errorf("preflight navigation failed: redirected to login while opening %s", target)
		}
		checked = append(checked, map[string]string{
			"requested_url": target,
			"final_url":     finalURL,
		})
	}

	return map[string]interface{}{
		"transport": "browser",
		"mode":      "preflight",
		"status":    "ok",
		"auth": map[string]interface{}{
			"loggedIn":   true,
			"reason":     identity.Reason,
			"username":   nullIfEmpty(identity.Username),
			"account_id": nullIfEmpty(identity.AccountID),
			"user_id":    nullIfEmpty(identity.UserID),
		},
		"checked_urls": checked,
	}, nil
}

func (a *app) browserWhoami(ctx context.Context) (map[string]interface{}, error) {
	identity, err := a.browserIdentityFromSession(ctx)
	if err != nil {
		return nil, fmt.Errorf("identity lookup failed: %w", err)
	}
	return map[string]interface{}{
		"transport":  "browser",
		"mode":       "whoami",
		"loggedIn":   identity.LoggedIn,
		"reason":     identity.Reason,
		"username":   nullIfEmpty(identity.Username),
		"account_id": nullIfEmpty(identity.AccountID),
		"user_id":    nullIfEmpty(identity.UserID),
	}, nil
}

func (a *app) browserMyComments(ctx context.Context, extraEnv map[string]string) (map[string]interface{}, error) {
	limit := clampListLimit(parsePositiveInt(extraEnv["REDDIT_LIST_LIMIT"], 10))
	identity, err := a.browserIdentityFromSession(ctx)
	if err != nil {
		return nil, err
	}
	if !identity.LoggedIn {
		return nil, fmt.Errorf("browser profile is not logged in to Reddit (%s)", identity.Reason)
	}

	endpoint := fmt.Sprintf(
		"%s/user/%s/comments?limit=%d&sort=new&raw_json=1",
		oauthBaseURL,
		neturl.PathEscape(identity.Username),
		limit,
	)
	var listing oauthUserCommentListing
	if _, err := a.oauthGetJSONWithToken(identity.TokenV2, endpoint, &listing); err != nil {
		return nil, fmt.Errorf("my comments lookup failed: %w", err)
	}

	items := make([]map[string]interface{}, 0, len(listing.Data.Children))
	for _, row := range listing.Data.Children {
		data := row.Data
		items = append(items, map[string]interface{}{
			"thing_id":    nullIfEmpty(data.Name),
			"subreddit":   nullIfEmpty(data.Subreddit),
			"score":       data.Score,
			"parent_id":   nullIfEmpty(data.ParentID),
			"link_title":  nullIfEmpty(data.LinkTitle),
			"body":        strings.TrimSpace(data.Body),
			"created_at":  unixToRFC3339(data.CreatedUTC),
			"comment_url": nullIfEmpty(absoluteRedditURL(data.Permalink)),
			"thread_url":  nullIfEmpty(absoluteRedditURL(data.LinkPermalink)),
		})
	}

	return map[string]interface{}{
		"transport": "browser",
		"mode":      "my_comments",
		"username":  nullIfEmpty(identity.Username),
		"count":     len(items),
		"items":     items,
	}, nil
}

func (a *app) browserMyReplies(ctx context.Context, extraEnv map[string]string) (map[string]interface{}, error) {
	limit := clampListLimit(parsePositiveInt(extraEnv["REDDIT_LIST_LIMIT"], 10))
	fetchLimit := clampListLimit(limit * 3)

	identity, err := a.browserIdentityFromSession(ctx)
	if err != nil {
		return nil, err
	}
	if !identity.LoggedIn {
		return nil, fmt.Errorf("browser profile is not logged in to Reddit (%s)", identity.Reason)
	}

	endpoint := fmt.Sprintf("%s/message/inbox?limit=%d&raw_json=1", oauthBaseURL, fetchLimit)
	var listing oauthInboxListing
	if _, err := a.oauthGetJSONWithToken(identity.TokenV2, endpoint, &listing); err != nil {
		return nil, fmt.Errorf("my replies lookup failed: %w", err)
	}

	items := make([]map[string]interface{}, 0, limit)
	for _, row := range listing.Data.Children {
		data := row.Data
		subjectLower := strings.ToLower(strings.TrimSpace(data.Subject))
		isCommentReply := strings.Contains(subjectLower, "comment reply") || (row.Kind == "t1" && data.WasComment)
		if !isCommentReply {
			continue
		}
		items = append(items, map[string]interface{}{
			"thing_id":    nullIfEmpty(data.Name),
			"author":      nullIfEmpty(data.Author),
			"subreddit":   nullIfEmpty(data.Subreddit),
			"subject":     nullIfEmpty(data.Subject),
			"new":         data.New,
			"parent_id":   nullIfEmpty(data.ParentID),
			"link_title":  nullIfEmpty(data.LinkTitle),
			"body":        strings.TrimSpace(data.Body),
			"created_at":  unixToRFC3339(data.CreatedUTC),
			"context_url": nullIfEmpty(absoluteRedditURL(data.Context)),
		})
		if len(items) >= limit {
			break
		}
	}

	return map[string]interface{}{
		"transport": "browser",
		"mode":      "my_replies",
		"username":  nullIfEmpty(identity.Username),
		"count":     len(items),
		"items":     items,
	}, nil
}

func (a *app) browserMyPosts(ctx context.Context, extraEnv map[string]string) (map[string]interface{}, error) {
	limit := clampListLimit(parsePositiveInt(extraEnv["REDDIT_LIST_LIMIT"], 10))
	identity, err := a.browserIdentityFromSession(ctx)
	if err != nil {
		return nil, err
	}
	if !identity.LoggedIn {
		return nil, fmt.Errorf("browser profile is not logged in to Reddit (%s)", identity.Reason)
	}

	endpoint := fmt.Sprintf(
		"%s/user/%s/submitted?limit=%d&sort=new&raw_json=1",
		oauthBaseURL,
		neturl.PathEscape(identity.Username),
		limit,
	)
	var listing oauthUserPostListing
	if _, err := a.oauthGetJSONWithToken(identity.TokenV2, endpoint, &listing); err != nil {
		return nil, fmt.Errorf("my posts lookup failed: %w", err)
	}

	items := make([]map[string]interface{}, 0, len(listing.Data.Children))
	for _, row := range listing.Data.Children {
		data := row.Data
		items = append(items, map[string]interface{}{
			"thing_id":       nullIfEmpty(data.Name),
			"subreddit":      nullIfEmpty(data.Subreddit),
			"title":          nullIfEmpty(data.Title),
			"url":            nullIfEmpty(data.URL),
			"score":          data.Score,
			"num_comments":   data.NumComments,
			"created_at":     unixToRFC3339(data.CreatedUTC),
			"submission_url": nullIfEmpty(absoluteRedditURL(data.Permalink)),
		})
	}

	return map[string]interface{}{
		"transport": "browser",
		"mode":      "my_posts",
		"username":  nullIfEmpty(identity.Username),
		"count":     len(items),
		"items":     items,
	}, nil
}

func (a *app) browserMySubreddits(ctx context.Context, extraEnv map[string]string) (map[string]interface{}, error) {
	limit := clampListLimit(parsePositiveInt(extraEnv["REDDIT_LIST_LIMIT"], 25))
	identity, err := a.browserIdentityFromSession(ctx)
	if err != nil {
		return nil, err
	}
	if !identity.LoggedIn {
		return nil, fmt.Errorf("browser profile is not logged in to Reddit (%s)", identity.Reason)
	}

	endpoint := fmt.Sprintf("%s/subreddits/mine/subscriber?limit=%d&raw_json=1", oauthBaseURL, limit)
	var listing oauthSubredditListing
	if _, err := a.oauthGetJSONWithToken(identity.TokenV2, endpoint, &listing); err != nil {
		return nil, fmt.Errorf("my subreddits lookup failed: %w", err)
	}

	items := make([]map[string]interface{}, 0, len(listing.Data.Children))
	for _, row := range listing.Data.Children {
		data := row.Data
		items = append(items, map[string]interface{}{
			"name":            nullIfEmpty(data.DisplayName),
			"title":           nullIfEmpty(data.Title),
			"subscribers":     data.Subscribers,
			"url":             nullIfEmpty(absoluteRedditURL(data.URL)),
			"thing_id":        nullIfEmpty(data.Name),
			"user_subscribed": data.UserIsSubscriber,
		})
	}

	return map[string]interface{}{
		"transport": "browser",
		"mode":      "my_subreddits",
		"username":  nullIfEmpty(identity.Username),
		"count":     len(items),
		"items":     items,
	}, nil
}

func (a *app) browserSubscribe(ctx context.Context, extraEnv map[string]string) (map[string]interface{}, error) {
	subreddit := strings.TrimSpace(extraEnv["REDDIT_SUBREDDIT"])
	dryRun := parseBoolLike(extraEnv["DRY_RUN_BROWSER"])
	if subreddit == "" {
		return nil, errors.New("missing env var: REDDIT_SUBREDDIT")
	}

	identity, err := a.browserIdentityFromSession(ctx)
	if err != nil {
		return nil, err
	}
	if !identity.LoggedIn {
		return nil, fmt.Errorf("browser profile is not logged in to Reddit (%s)", identity.Reason)
	}

	if dryRun {
		return map[string]interface{}{
			"transport": "browser",
			"mode":      "subscribe",
			"status":    "preview",
			"dry_run":   true,
			"subreddit": subreddit,
			"username":  nullIfEmpty(identity.Username),
		}, nil
	}

	if err := a.oauthSubscribe(identity.TokenV2, subreddit); err != nil {
		return nil, err
	}

	return map[string]interface{}{
		"transport": "browser",
		"mode":      "subscribe",
		"status":    "subscribed",
		"subreddit": subreddit,
		"username":  nullIfEmpty(identity.Username),
	}, nil
}

func (a *app) browserLike(ctx context.Context, extraEnv map[string]string) (map[string]interface{}, error) {
	thingID := strings.TrimSpace(extraEnv["REDDIT_THING_ID"])
	permalink := strings.TrimSpace(extraEnv["REDDIT_PERMALINK"])
	dryRun := parseBoolLike(extraEnv["DRY_RUN_BROWSER"])
	confirmLike := parseBoolLike(extraEnv["REDDIT_CONFIRM_LIKE"])

	if thingID == "" && permalink == "" {
		return nil, errors.New("missing target: REDDIT_THING_ID or REDDIT_PERMALINK")
	}

	identity, err := a.browserIdentityFromSession(ctx)
	if err != nil {
		return nil, err
	}
	if !identity.LoggedIn {
		return nil, fmt.Errorf("browser profile is not logged in to Reddit (%s)", identity.Reason)
	}

	resolvedPermalink := permalink
	if thingID == "" {
		post, resolveErr := a.resolveThreadPost(permalink)
		if resolveErr != nil {
			return nil, fmt.Errorf("unable to resolve thing id from permalink: %w", resolveErr)
		}
		thingID = strings.TrimSpace(post.Name)
		if resolvedPermalink == "" {
			resolvedPermalink = strings.TrimSpace(post.Permalink)
		}
	}
	if thingID == "" {
		return nil, errors.New("resolved thing id is empty")
	}

	if dryRun {
		return map[string]interface{}{
			"transport": "browser",
			"mode":      "like",
			"status":    "preview",
			"dry_run":   true,
			"thing_id":  thingID,
			"permalink": nullIfEmpty(resolvedPermalink),
			"username":  nullIfEmpty(identity.Username),
		}, nil
	}
	if !confirmLike {
		return nil, errors.New("like requires explicit confirmation (set --confirm-like)")
	}

	if err := a.oauthVote(identity.TokenV2, thingID, 1); err != nil {
		return nil, err
	}

	return map[string]interface{}{
		"transport": "browser",
		"mode":      "like",
		"status":    "liked",
		"thing_id":  thingID,
		"permalink": nullIfEmpty(resolvedPermalink),
		"username":  nullIfEmpty(identity.Username),
	}, nil
}

func (a *app) browserPublish(ctx context.Context, extraEnv map[string]string) (map[string]interface{}, error) {
	postKind := strings.TrimSpace(extraEnv["REDDIT_POST_KIND"])
	if postKind == "" {
		postKind = "comment"
	}

	identity, err := a.browserIdentityFromSession(ctx)
	if err != nil {
		return nil, err
	}
	if !identity.LoggedIn {
		return nil, fmt.Errorf("browser profile is not logged in to Reddit (%s)", identity.Reason)
	}

	switch postKind {
	case "comment":
		return a.browserPublishComment(ctx, identity, extraEnv)
	case "self":
		return a.browserPublishSelfPost(ctx, identity, extraEnv)
	default:
		return nil, fmt.Errorf("browser publish does not support kind %q", postKind)
	}
}

func (a *app) browserPublishComment(ctx context.Context, identity browserIdentity, extraEnv map[string]string) (map[string]interface{}, error) {
	thingID := strings.TrimSpace(extraEnv["REDDIT_THING_ID"])
	commentText := strings.TrimSpace(extraEnv["REDDIT_TEXT"])
	permalink := strings.TrimSpace(extraEnv["REDDIT_PERMALINK"])
	subreddit := strings.TrimSpace(extraEnv["REDDIT_SUBREDDIT"])
	dryRun := parseBoolLike(extraEnv["DRY_RUN_BROWSER"])
	confirmDoublePost := parseBoolLike(extraEnv["REDDIT_CONFIRM_DOUBLE_POST"])
	verifyTimeout := parsePositiveInt(extraEnv["REDDIT_POST_VERIFY_TIMEOUT_SEC"], 30)

	if thingID == "" {
		return nil, errors.New("missing env var: REDDIT_THING_ID")
	}
	if commentText == "" {
		return nil, errors.New("missing env var: REDDIT_TEXT")
	}
	if permalink == "" {
		return nil, errors.New("missing env var: REDDIT_PERMALINK")
	}

	duplicate, err := a.findExistingOwnComment(identity, permalink)
	if err != nil {
		return nil, fmt.Errorf("duplicate check failed: %w", err)
	}
	if duplicate.Found && !confirmDoublePost && !dryRun {
		target := firstNonEmpty(duplicate.CommentURL, duplicate.ThingID, permalink)
		return nil, fmt.Errorf(
			"existing comment already found for %q on this thread (%s); rerun with --confirm-double-post to post again",
			identity.Username,
			target,
		)
	}

	if dryRun {
		return map[string]interface{}{
			"transport": "browser",
			"mode":      "publish",
			"dry_run":   true,
			"thing_id":  thingID,
			"permalink": permalink,
			"username":  nullIfEmpty(identity.Username),
			"duplicate_check": map[string]interface{}{
				"found":       duplicate.Found,
				"reason":      duplicate.Reason,
				"thing_id":    nullIfEmpty(duplicate.ThingID),
				"comment_url": nullIfEmpty(duplicate.CommentURL),
			},
		}, nil
	}

	targetURL := absoluteOldRedditURL(permalink)
	if err := a.browserNavigate(ctx, targetURL); err != nil {
		return nil, err
	}
	finalURL, _ := a.browserCurrentURL(ctx)
	if isLoginURL(finalURL) {
		return nil, fmt.Errorf("target navigation redirected to login (%s)", finalURL)
	}

	if err := a.browserOpenComposer(ctx, thingID); err != nil {
		return nil, err
	}
	if err := a.browserFillEditor(ctx, commentText); err != nil {
		return nil, err
	}
	if err := a.browserSubmitComment(ctx); err != nil {
		return nil, err
	}
	if err := chromedp.Run(ctx, chromedp.Sleep(1500*time.Millisecond)); err != nil {
		return nil, err
	}

	verification, err := a.verifyPostedComment(identity, permalink, commentText, verifyTimeout)
	if err != nil {
		return nil, err
	}

	status := "posted_unverified"
	if verification.Verified {
		status = "posted_verified"
	}
	result := map[string]interface{}{
		"transport": "browser",
		"mode":      "publish",
		"status":    status,
		"thing_id":  firstNonEmpty(verification.ThingID, thingID),
		"permalink": permalink,
		"subreddit": nullIfEmpty(subreddit),
		"username":  nullIfEmpty(identity.Username),
		"comment_url": func() interface{} {
			if verification.CommentURL == "" {
				return nil
			}
			return verification.CommentURL
		}(),
		"verification": map[string]interface{}{
			"verified":    verification.Verified,
			"reason":      verification.Reason,
			"username":    nullIfEmpty(verification.Username),
			"thing_id":    nullIfEmpty(verification.ThingID),
			"comment_url": nullIfEmpty(verification.CommentURL),
		},
		"duplicate_check": map[string]interface{}{
			"found":       duplicate.Found,
			"reason":      duplicate.Reason,
			"thing_id":    nullIfEmpty(duplicate.ThingID),
			"comment_url": nullIfEmpty(duplicate.CommentURL),
		},
	}
	return result, nil
}

func (a *app) browserPublishSelfPost(ctx context.Context, identity browserIdentity, extraEnv map[string]string) (map[string]interface{}, error) {
	subreddit := strings.TrimSpace(extraEnv["REDDIT_SUBREDDIT"])
	title := strings.TrimSpace(extraEnv["REDDIT_TITLE"])
	body := strings.TrimSpace(extraEnv["REDDIT_TEXT"])
	flair := strings.TrimSpace(extraEnv["REDDIT_POST_FLAIR"])
	dryRun := parseBoolLike(extraEnv["DRY_RUN_BROWSER"])
	confirmDoublePost := parseBoolLike(extraEnv["REDDIT_CONFIRM_DOUBLE_POST"])
	verifyTimeout := parsePositiveInt(extraEnv["REDDIT_POST_VERIFY_TIMEOUT_SEC"], 30)
	nsfw := parseBoolLike(extraEnv["REDDIT_NSFW"])

	if subreddit == "" {
		return nil, errors.New("missing env var: REDDIT_SUBREDDIT")
	}
	if title == "" {
		return nil, errors.New("missing env var: REDDIT_TITLE")
	}
	if body == "" {
		return nil, errors.New("missing env var: REDDIT_TEXT")
	}

	duplicate, err := a.findExistingOwnSubmission(identity, subreddit, title)
	if err != nil {
		return nil, fmt.Errorf("duplicate check failed: %w", err)
	}
	if duplicate.Found && !confirmDoublePost && !dryRun {
		target := firstNonEmpty(duplicate.SubmissionURL, duplicate.ThingID, subreddit)
		return nil, fmt.Errorf(
			"existing post already found for %q in r/%s (%s); rerun with --confirm-double-post to post again",
			identity.Username,
			subreddit,
			target,
		)
	}

	if dryRun {
		return map[string]interface{}{
			"transport": "browser",
			"mode":      "publish",
			"kind":      "self",
			"dry_run":   true,
			"subreddit": subreddit,
			"title":     title,
			"username":  nullIfEmpty(identity.Username),
			"duplicate_check": map[string]interface{}{
				"found":          duplicate.Found,
				"reason":         duplicate.Reason,
				"thing_id":       nullIfEmpty(duplicate.ThingID),
				"submission_url": nullIfEmpty(duplicate.SubmissionURL),
			},
		}, nil
	}

	selectedFlair := ""
	flairID := ""
	flairText := ""
	if flair != "" {
		flairID, flairText, err = a.resolveLinkFlairTemplate(identity.TokenV2, subreddit, flair)
		if err != nil {
			return nil, err
		}
		selectedFlair = flairText
	}
	thingID, responseURL, err := a.submitPostWithToken(identity.TokenV2, subreddit, title, "self", body, "", nsfw, flairID, flairText)
	if err != nil {
		return nil, err
	}
	verification, err := a.verifyPostedSubmission(identity, subreddit, title, body, verifyTimeout)
	if err != nil {
		return nil, err
	}

	status := "posted_unverified"
	if verification.Verified {
		status = "posted_verified"
	}
	return map[string]interface{}{
		"transport": "browser",
		"mode":      "publish",
		"kind":      "self",
		"status":    status,
		"subreddit": subreddit,
		"title":     title,
		"flair":     nullIfEmpty(selectedFlair),
		"thing_id":  nullIfEmpty(firstNonEmpty(verification.ThingID, thingID)),
		"username":  nullIfEmpty(identity.Username),
		"submission_url": func() interface{} {
			if verification.SubmissionURL != "" {
				return verification.SubmissionURL
			}
			if strings.TrimSpace(responseURL) != "" {
				return responseURL
			}
			return nil
		}(),
		"verification": map[string]interface{}{
			"verified":       verification.Verified,
			"reason":         verification.Reason,
			"username":       nullIfEmpty(verification.Username),
			"thing_id":       nullIfEmpty(verification.ThingID),
			"submission_url": nullIfEmpty(verification.SubmissionURL),
		},
		"duplicate_check": map[string]interface{}{
			"found":          duplicate.Found,
			"reason":         duplicate.Reason,
			"thing_id":       nullIfEmpty(duplicate.ThingID),
			"submission_url": nullIfEmpty(duplicate.SubmissionURL),
		},
	}, nil
}

func (a *app) browserNavigate(ctx context.Context, targetURL string) error {
	err := chromedp.Run(ctx, chromedp.Navigate(targetURL))
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "err_aborted") {
		return nil
	}
	if err != nil {
		return err
	}
	return chromedp.Run(ctx, chromedp.Sleep(900*time.Millisecond))
}

func (a *app) browserCurrentURL(ctx context.Context) (string, error) {
	var current string
	err := chromedp.Run(ctx, chromedp.Evaluate(`window.location.href`, &current))
	return strings.TrimSpace(current), err
}

func isLoginURL(value string) bool {
	return strings.Contains(strings.ToLower(value), "/login")
}

func (a *app) browserIdentityFromSession(ctx context.Context) (browserIdentity, error) {
	token, accountID, err := a.browserTokenV2(ctx)
	if err != nil {
		return browserIdentity{}, fmt.Errorf("token extraction failed: %w", err)
	}
	identity := browserIdentity{
		TokenV2:   token,
		AccountID: accountID,
	}
	if token == "" {
		identity.Reason = "token_v2_missing"
		return identity, nil
	}

	me, status, err := a.oauthMe(token)
	if err != nil {
		if status > 0 {
			identity.Reason = fmt.Sprintf("oauth_me_status_%d", status)
		} else {
			identity.Reason = "oauth_me_failed"
		}
		return identity, nil
	}

	identity.LoggedIn = strings.TrimSpace(me.Name) != "" || strings.TrimSpace(me.ID) != "" || identity.AccountID != ""
	identity.Reason = "oauth_me_ok"
	identity.Username = strings.TrimSpace(me.Name)
	identity.UserID = strings.TrimSpace(me.ID)
	if identity.AccountID == "" && identity.UserID != "" {
		identity.AccountID = "t2_" + identity.UserID
	}
	return identity, nil
}

func (a *app) browserTokenV2(ctx context.Context) (tokenV2 string, accountID string, err error) {
	var cookies []*network.Cookie
	err = chromedp.Run(ctx, chromedp.ActionFunc(func(execCtx context.Context) error {
		var innerErr error
		cookies, innerErr = network.GetCookies().WithUrls([]string{redditBaseURL + "/"}).Do(execCtx)
		if innerErr != nil {
			cookies, innerErr = network.GetCookies().Do(execCtx)
		}
		return innerErr
	}))
	if err != nil {
		return "", "", fmt.Errorf("network.getCookies failed: %w", err)
	}
	for _, cookie := range cookies {
		if cookie.Name == "token_v2" {
			tokenV2 = cookie.Value
			break
		}
	}
	if tokenV2 == "" {
		return "", "", nil
	}
	accountID = decodeJWTAccountID(tokenV2)
	return tokenV2, accountID, nil
}

func decodeJWTAccountID(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return ""
	}
	for _, key := range []string{"aid", "lid"} {
		if value, ok := payload[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

type oauthMePayload struct {
	Name string `json:"name"`
	ID   string `json:"id"`
}

func (a *app) oauthMe(token string) (oauthMePayload, int, error) {
	req, err := http.NewRequest(http.MethodGet, oauthBaseURL+"/api/v1/me", nil)
	if err != nil {
		return oauthMePayload{}, 0, err
	}
	req.Header.Set("Authorization", "bearer "+token)
	req.Header.Set("User-Agent", a.userAgent)

	resp, err := a.client.Do(req)
	if err != nil {
		return oauthMePayload{}, 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return oauthMePayload{}, resp.StatusCode, err
	}
	if resp.StatusCode != http.StatusOK {
		return oauthMePayload{}, resp.StatusCode, fmt.Errorf("oauth /api/v1/me returned status %d: %s", resp.StatusCode, clip(string(body), 240))
	}

	var payload oauthMePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return oauthMePayload{}, resp.StatusCode, err
	}
	return payload, resp.StatusCode, nil
}

func (a *app) oauthGetJSONWithToken(token, endpoint string, out interface{}) (int, error) {
	if strings.TrimSpace(token) == "" {
		return 0, errors.New("missing oauth token")
	}

	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "bearer "+token)
	req.Header.Set("User-Agent", a.userAgent)

	resp, err := a.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("oauth GET returned status %d: %s", resp.StatusCode, clip(string(body), 240))
	}
	if out == nil {
		return resp.StatusCode, nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return resp.StatusCode, fmt.Errorf("unable to parse oauth JSON response: %w", err)
	}
	return resp.StatusCode, nil
}

func (a *app) oauthPostFormWithToken(token, endpoint string, values neturl.Values, out interface{}) (int, error) {
	if strings.TrimSpace(token) == "" {
		return 0, errors.New("missing oauth token")
	}

	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "bearer "+token)
	req.Header.Set("User-Agent", a.userAgent)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := a.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("oauth POST returned status %d: %s", resp.StatusCode, clip(string(body), 240))
	}
	if out == nil {
		return resp.StatusCode, nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return resp.StatusCode, fmt.Errorf("unable to parse oauth JSON response: %w", err)
	}
	return resp.StatusCode, nil
}

func (a *app) oauthSubscribe(token, subreddit string) error {
	subreddit = strings.TrimSpace(strings.TrimPrefix(subreddit, "r/"))
	if subreddit == "" {
		return errors.New("subreddit is required")
	}

	values := neturl.Values{}
	values.Set("action", "sub")
	values.Set("sr_name", subreddit)
	values.Set("api_type", "json")

	var payload redditAPIResponse
	if _, err := a.oauthPostFormWithToken(token, oauthBaseURL+"/api/subscribe", values, &payload); err != nil {
		return err
	}
	if err := ensureAPISuccess(payload); err != nil {
		return err
	}
	return nil
}

type linkFlairTemplate struct {
	ID      string
	Text    string
	ModOnly bool
}

func (a *app) resolveLinkFlairTemplate(token, subreddit, desired string) (string, string, error) {
	subreddit = strings.TrimSpace(strings.TrimPrefix(subreddit, "r/"))
	desired = strings.TrimSpace(desired)
	if subreddit == "" {
		return "", "", errors.New("subreddit is required")
	}

	endpoint := fmt.Sprintf("%s/r/%s/api/link_flair_v2/", oauthBaseURL, neturl.PathEscape(subreddit))
	var raw interface{}
	if _, err := a.oauthGetJSONWithToken(token, endpoint, &raw); err != nil {
		return "", "", fmt.Errorf("unable to list post flairs for r/%s: %w", subreddit, err)
	}

	templates := extractLinkFlairTemplates(raw)
	if len(templates) == 0 {
		return "", "", fmt.Errorf("r/%s did not return any selectable post flairs", subreddit)
	}

	if desired == "" {
		for _, template := range templates {
			if template.ID != "" && !template.ModOnly {
				return template.ID, template.Text, nil
			}
		}
		first := templates[0]
		return first.ID, first.Text, nil
	}

	bestIndex := -1
	bestScore := -1
	for i, template := range templates {
		score := scoreLinkFlairMatch(template.Text, desired)
		if score < 0 {
			continue
		}
		if template.ModOnly {
			score -= 10
		}
		if score > bestScore {
			bestScore = score
			bestIndex = i
		}
	}
	if bestIndex >= 0 {
		match := templates[bestIndex]
		return match.ID, match.Text, nil
	}

	available := make([]string, 0, len(templates))
	for _, template := range templates {
		if template.Text == "" {
			continue
		}
		available = append(available, template.Text)
	}
	sort.Strings(available)
	return "", "", fmt.Errorf(
		"unable to find post flair %q in r/%s; available flairs: %s",
		desired,
		subreddit,
		strings.Join(available, ", "),
	)
}

func extractLinkFlairTemplates(raw interface{}) []linkFlairTemplate {
	items := make([]interface{}, 0)
	switch value := raw.(type) {
	case []interface{}:
		items = append(items, value...)
	case map[string]interface{}:
		if choices, ok := value["choices"].([]interface{}); ok {
			items = append(items, choices...)
		}
		if current, ok := value["current"].(map[string]interface{}); ok {
			items = append(items, current)
		}
	}

	templates := make([]linkFlairTemplate, 0, len(items))
	seen := map[string]struct{}{}
	for _, item := range items {
		entry, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		template := linkFlairTemplate{
			ID:      firstMapString(entry, "id", "flair_template_id", "template_id"),
			Text:    strings.TrimSpace(firstNonEmpty(firstMapString(entry, "text", "flair_text"), richtextPlainText(entry["richtext"]))),
			ModOnly: firstMapBool(entry, "mod_only", "is_mod_only"),
		}
		if template.ID == "" && template.Text == "" {
			continue
		}
		key := template.ID + "\x00" + template.Text
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		templates = append(templates, template)
	}
	return templates
}

func firstMapString(values map[string]interface{}, keys ...string) string {
	for _, key := range keys {
		raw, ok := values[key]
		if !ok {
			continue
		}
		switch typed := raw.(type) {
		case string:
			if strings.TrimSpace(typed) != "" {
				return strings.TrimSpace(typed)
			}
		}
	}
	return ""
}

func firstMapBool(values map[string]interface{}, keys ...string) bool {
	for _, key := range keys {
		raw, ok := values[key]
		if !ok {
			continue
		}
		switch typed := raw.(type) {
		case bool:
			return typed
		case string:
			return parseBoolLike(typed)
		}
	}
	return false
}

func richtextPlainText(raw interface{}) string {
	items, ok := raw.([]interface{})
	if !ok {
		return ""
	}
	var parts []string
	for _, item := range items {
		entry, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if text, ok := entry["t"].(string); ok && text != "" {
			parts = append(parts, text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, ""))
}

func scoreLinkFlairMatch(actual, desired string) int {
	actual = strings.TrimSpace(actual)
	desired = strings.TrimSpace(desired)
	if actual == "" || desired == "" {
		return -1
	}

	actualFold := strings.ToLower(actual)
	desiredFold := strings.ToLower(desired)
	if actualFold == desiredFold {
		return 100
	}

	actualNorm := normalizeFlairLabel(actual)
	desiredNorm := normalizeFlairLabel(desired)
	switch {
	case actualNorm == desiredNorm:
		return 90
	case strings.Contains(actualNorm, desiredNorm):
		return 70
	case strings.Contains(desiredNorm, actualNorm):
		return 60
	case strings.HasPrefix(actualNorm, desiredNorm):
		return 55
	case strings.HasPrefix(desiredNorm, actualNorm):
		return 50
	default:
		return -1
	}
}

func normalizeFlairLabel(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return ""
	}
	var b strings.Builder
	lastSpace := false
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastSpace = false
		case !lastSpace:
			b.WriteByte(' ')
			lastSpace = true
		}
	}
	return strings.TrimSpace(b.String())
}

func (a *app) resolveThreadPost(permalink string) (postData, error) {
	if strings.TrimSpace(permalink) == "" {
		return postData{}, errors.New("permalink is required")
	}
	endpoint := fmt.Sprintf("%s%s.json?raw_json=1&limit=1", redditBaseURL, normalizePermalink(permalink))

	var payload []threadListing
	if err := a.getJSON(endpoint, &payload, false); err != nil {
		return postData{}, err
	}
	if len(payload) < 1 || len(payload[0].Data.Children) == 0 {
		return postData{}, errors.New("thread response did not include post data")
	}

	var post postData
	if err := json.Unmarshal(payload[0].Data.Children[0].Data, &post); err != nil {
		return postData{}, fmt.Errorf("unable to decode thread metadata: %w", err)
	}
	return post, nil
}

func (a *app) oauthVote(token, thingID string, dir int) error {
	if strings.TrimSpace(token) == "" {
		return errors.New("token is required for vote")
	}
	if strings.TrimSpace(thingID) == "" {
		return errors.New("thing id is required for vote")
	}
	if dir != 1 && dir != 0 && dir != -1 {
		return fmt.Errorf("invalid vote direction: %d", dir)
	}

	values := neturl.Values{}
	values.Set("id", strings.TrimSpace(thingID))
	values.Set("dir", strconv.Itoa(dir))
	values.Set("api_type", "json")

	req, err := http.NewRequest(http.MethodPost, oauthBaseURL+"/api/vote", strings.NewReader(values.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "bearer "+token)
	req.Header.Set("User-Agent", a.userAgent)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("oauth vote returned status %d: %s", resp.StatusCode, clip(string(body), 240))
	}
	if strings.TrimSpace(string(body)) == "" {
		return nil
	}

	var payload redditAPIResponse
	if err := json.Unmarshal(body, &payload); err == nil {
		if ensureErr := ensureAPISuccess(payload); ensureErr != nil {
			return ensureErr
		}
	}
	return nil
}

func (a *app) browserHasVisibleEditor(ctx context.Context) (bool, error) {
	script := `(function () {
  function isVisible(el) {
    if (!el) return false;
    const style = window.getComputedStyle(el);
    if (!style) return false;
    if (style.display === "none" || style.visibility === "hidden") return false;
    return el.offsetWidth > 0 || el.offsetHeight > 0 || el.getClientRects().length > 0;
  }
  const nodes = Array.from(document.querySelectorAll('textarea, div[contenteditable="true"], [role="textbox"]'));
  return nodes.some(isVisible);
})()`
	var has bool
	err := chromedp.Run(ctx, chromedp.Evaluate(script, &has))
	return has, err
}

func (a *app) browserOpenComposer(ctx context.Context, thingID string) error {
	has, err := a.browserHasVisibleEditor(ctx)
	if err != nil {
		return err
	}
	if has {
		return nil
	}

	thingLiteral, _ := json.Marshal(thingID)
	clickScript := fmt.Sprintf(`(function () {
  const thingId = %s;
  const clickables = [];
  function isVisible(el) {
    if (!el) return false;
    const style = window.getComputedStyle(el);
    if (!style) return false;
    if (style.display === "none" || style.visibility === "hidden") return false;
    return el.offsetWidth > 0 || el.offsetHeight > 0 || el.getClientRects().length > 0;
  }
  function addCandidates(root) {
    if (!root || !root.querySelectorAll) return;
    for (const node of root.querySelectorAll('button, [role="button"]')) {
      clickables.push(node);
    }
  }
  if (thingId && thingId.startsWith("t1_")) {
    const commentId = thingId.slice(3);
    addCandidates(document.querySelector('[data-fullname="t1_' + commentId + '"]'));
    addCandidates(document.getElementById('thing_t1_' + commentId));
    const fuzzy = Array.from(document.querySelectorAll('[id*="' + commentId + '"]'));
    for (const root of fuzzy) addCandidates(root);
  }
  addCandidates(document);
  const wanted = [/reply/i, /add a comment/i, /comment/i];
  for (const node of clickables) {
    if (!isVisible(node)) continue;
    const label = ((node.innerText || node.textContent || "") + " " + (node.getAttribute("aria-label") || "")).trim();
    if (!wanted.some((rx) => rx.test(label))) continue;
    try {
      node.click();
      return true;
    } catch (_err) {
      continue;
    }
  }
  return false;
})()`, string(thingLiteral))

	var clicked bool
	if err := chromedp.Run(ctx, chromedp.Evaluate(clickScript, &clicked)); err != nil {
		return err
	}
	if clicked {
		if err := chromedp.Run(ctx, chromedp.Sleep(700*time.Millisecond)); err != nil {
			return err
		}
	}
	has, err = a.browserHasVisibleEditor(ctx)
	if err != nil {
		return err
	}
	if !has {
		return errors.New("comment composer could not be opened")
	}
	return nil
}

func (a *app) browserFillEditor(ctx context.Context, commentText string) error {
	textLiteral, _ := json.Marshal(commentText)
	script := fmt.Sprintf(`(function () {
  const value = %s;
  function isVisible(el) {
    if (!el) return false;
    const style = window.getComputedStyle(el);
    if (!style) return false;
    if (style.display === "none" || style.visibility === "hidden") return false;
    return el.offsetWidth > 0 || el.offsetHeight > 0 || el.getClientRects().length > 0;
  }
  const textarea = Array.from(document.querySelectorAll('textarea')).find(isVisible);
  if (textarea) {
    textarea.focus();
    textarea.value = value;
    textarea.dispatchEvent(new Event("input", { bubbles: true }));
    textarea.dispatchEvent(new Event("change", { bubbles: true }));
    return "textarea";
  }
  const editable = Array.from(document.querySelectorAll('div[contenteditable="true"], [contenteditable="true"], [role="textbox"]')).find(isVisible);
  if (editable) {
    editable.focus();
    editable.textContent = value;
    editable.dispatchEvent(new Event("input", { bubbles: true }));
    editable.dispatchEvent(new Event("change", { bubbles: true }));
    return "contenteditable";
  }
  return "";
})()`, string(textLiteral))

	var method string
	if err := chromedp.Run(ctx, chromedp.Evaluate(script, &method)); err != nil {
		return err
	}
	if strings.TrimSpace(method) == "" {
		return errors.New("comment editor not found")
	}
	return nil
}

func (a *app) browserFillSubmissionForm(ctx context.Context, title, body string, nsfw bool) error {
	titleLiteral, _ := json.Marshal(title)
	bodyLiteral, _ := json.Marshal(body)
	nsfwLiteral := "false"
	if nsfw {
		nsfwLiteral = "true"
	}
	script := fmt.Sprintf(`(function () {
  const titleValue = %s;
  const bodyValue = %s;
  const nsfwWanted = %s;
  function isVisible(el) {
    if (!el) return false;
    const style = window.getComputedStyle(el);
    if (!style) return false;
    if (style.display === "none" || style.visibility === "hidden") return false;
    return el.offsetWidth > 0 || el.offsetHeight > 0 || el.getClientRects().length > 0;
  }
  function setValue(node, value) {
    node.focus();
    node.value = value;
    node.dispatchEvent(new Event("input", { bubbles: true }));
    node.dispatchEvent(new Event("change", { bubbles: true }));
  }
  const textTab = document.querySelector('a.text-button, button.text-button, a[href$="#text-post"]');
  if (textTab && isVisible(textTab)) {
    try { textTab.click(); } catch (_err) {}
  }

  let titleNode = document.querySelector('textarea[name="title"], input[name="title"], #title-field input, input#title');
  if (!titleNode) {
    return { ok: false, reason: "title_not_found" };
  }
  setValue(titleNode, titleValue);

  let bodyNode = document.querySelector('textarea[name="text"], #text-field textarea, textarea[name="article"]');
  if (!bodyNode) {
    return { ok: false, reason: "body_not_found" };
  }
  setValue(bodyNode, bodyValue);

  if (nsfwWanted) {
    const checkboxes = Array.from(document.querySelectorAll('input[type="checkbox"]'));
    for (const box of checkboxes) {
      const container = box.closest('label') || box.parentElement || box;
      const label = ((container && (container.innerText || container.textContent)) || "" ) + " " + (box.getAttribute("aria-label") || "");
      if (!/nsfw/i.test(label)) continue;
      if (!box.checked) {
        box.click();
      }
      break;
    }
  }
  return { ok: true, reason: "filled" };
})()`, string(titleLiteral), string(bodyLiteral), nsfwLiteral)

	var result struct {
		Ok     bool   `json:"ok"`
		Reason string `json:"reason"`
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(script, &result)); err != nil {
		return err
	}
	if !result.Ok {
		return fmt.Errorf("submission form fill failed: %s", firstNonEmpty(result.Reason, "unknown"))
	}
	return nil
}

func (a *app) browserPageHasText(ctx context.Context, snippet string) bool {
	snippet = strings.TrimSpace(strings.ToLower(snippet))
	if snippet == "" {
		return false
	}
	var bodyText string
	if err := chromedp.Run(ctx, chromedp.Evaluate(`document.body ? document.body.innerText : ""`, &bodyText)); err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(bodyText), snippet)
}

func (a *app) browserSelectPostFlair(ctx context.Context, desired string) (string, error) {
	if chosen, ok, err := a.browserTryChooseVisibleFlair(ctx, desired); err == nil && ok {
		return chosen, nil
	}
	if err := a.browserOpenFlairSelector(ctx); err != nil {
		return "", err
	}
	if err := chromedp.Run(ctx, chromedp.Sleep(700*time.Millisecond)); err != nil {
		return "", err
	}
	chosen, ok, err := a.browserTryChooseVisibleFlair(ctx, desired)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errors.New("no selectable flair option found")
	}
	return chosen, nil
}

func (a *app) browserOpenFlairSelector(ctx context.Context) error {
	script := `(function () {
  function isVisible(el) {
    if (!el) return false;
    const style = window.getComputedStyle(el);
    if (!style) return false;
    if (style.display === "none" || style.visibility === "hidden") return false;
    return el.offsetWidth > 0 || el.offsetHeight > 0 || el.getClientRects().length > 0;
  }
  const candidates = Array.from(document.querySelectorAll('a, button, span, label, input[type="button"]'));
  for (const node of candidates) {
    if (!isVisible(node)) continue;
    const label = ((node.innerText || node.textContent || "") + " " + (node.getAttribute("aria-label") || "") + " " + (node.getAttribute("name") || "") + " " + (node.id || "") + " " + (node.className || "")).replace(/\s+/g, " ").trim().toLowerCase();
    if (!/flair|select/.test(label)) continue;
    if (!/flair/.test(label) && label !== "select") continue;
    try {
      node.click();
      return true;
    } catch (_err) {
      continue;
    }
  }
  return false;
})()`
	var opened bool
	if err := chromedp.Run(ctx, chromedp.Evaluate(script, &opened)); err != nil {
		return err
	}
	if !opened {
		return errors.New("flair selector opener not found")
	}
	return nil
}

func (a *app) browserTryChooseVisibleFlair(ctx context.Context, desired string) (string, bool, error) {
	desiredLiteral, _ := json.Marshal(strings.TrimSpace(desired))
	script := fmt.Sprintf(`(function () {
  const desired = %s.trim().toLowerCase();
  function isVisible(el) {
    if (!el) return false;
    const style = window.getComputedStyle(el);
    if (!style) return false;
    if (style.display === "none" || style.visibility === "hidden") return false;
    return el.offsetWidth > 0 || el.offsetHeight > 0 || el.getClientRects().length > 0;
  }
  function textOf(el) {
    return ((el && (el.innerText || el.textContent)) || "").replace(/\s+/g, " ").trim();
  }
  function validLabel(label) {
    const lower = label.toLowerCase();
    if (!lower) return false;
    if (lower === "select" || lower === "(none)" || lower === "none") return false;
    if (lower === "clear" || lower === "cancel" || lower === "close") return false;
    if (lower === "save" || lower === "apply" || lower === "ok" || lower === "done") return false;
    if (lower === "post" || lower === "submit") return false;
    return true;
  }
  function chooseOption(label, clickNode, finalize) {
    if (!clickNode) return null;
    try {
      clickNode.click();
    } catch (_err) {
      return null;
    }
    if (finalize) {
      for (const node of Array.from(document.querySelectorAll('button, a, input[type="button"], input[type="submit"]'))) {
        if (!isVisible(node) || node.disabled) continue;
        const action = textOf(node).toLowerCase() || String(node.getAttribute("value") || "").toLowerCase();
        if (!/save|apply|ok|done/.test(action)) continue;
        try { node.click(); } catch (_err) {}
        break;
      }
    }
    return label;
  }

  const selects = Array.from(document.querySelectorAll('select')).filter(el => isVisible(el) && /flair/i.test((el.name || "") + " " + (el.id || "") + " " + (el.className || "")));
  for (const select of selects) {
    const options = Array.from(select.options || []).map(option => ({ node: option, label: textOf(option), value: option.value || "" })).filter(option => option.value && validLabel(option.label));
    const match = desired ? options.find(option => option.label.toLowerCase() === desired || option.label.toLowerCase().includes(desired)) : options[0];
    if (match) {
      select.value = match.value;
      select.dispatchEvent(new Event("change", { bubbles: true }));
      return { ok: true, selected: match.label };
    }
  }

  const flairContainers = Array.from(document.querySelectorAll('.flairselector, [class*="flair"], [id*="flair"]')).filter(isVisible);
  const pool = flairContainers.length > 0 ? flairContainers : [document.body];
  const seen = new Set();
  const candidates = [];
  for (const root of pool) {
    const nodes = Array.from(root.querySelectorAll('a, button, label, span, li, input[type="radio"]'));
    for (const node of nodes) {
      if (!isVisible(node)) continue;
      let clickNode = node;
      let label = textOf(node);
      if (node.tagName === "INPUT" && node.type === "radio") {
        label = textOf(node.closest('label')) || String(node.getAttribute('value') || "");
      }
      if (!validLabel(label)) continue;
      if (seen.has(label)) continue;
      seen.add(label);
      candidates.push({ label, clickNode });
    }
  }

  const match = desired ? candidates.find(option => option.label.toLowerCase() === desired || option.label.toLowerCase().includes(desired)) : candidates[0];
  if (!match) {
    return { ok: false, selected: "" };
  }
  const chosen = chooseOption(match.label, match.clickNode, true);
  if (!chosen) {
    return { ok: false, selected: "" };
  }
  return { ok: true, selected: chosen };
})()`, string(desiredLiteral))

	var result struct {
		Ok       bool   `json:"ok"`
		Selected string `json:"selected"`
	}
	if err := chromedp.Run(ctx, chromedp.Evaluate(script, &result)); err != nil {
		return "", false, err
	}
	return strings.TrimSpace(result.Selected), result.Ok, nil
}

func (a *app) browserSubmitComment(ctx context.Context) error {
	script := `(function () {
  function isVisible(el) {
    if (!el) return false;
    const style = window.getComputedStyle(el);
    if (!style) return false;
    if (style.display === "none" || style.visibility === "hidden") return false;
    return el.offsetWidth > 0 || el.offsetHeight > 0 || el.getClientRects().length > 0;
  }
  const candidates = Array.from(document.querySelectorAll('button, [role="button"], input[type="submit"]'));
  const preferred = [/comment/i, /reply/i, /post/i, /save/i];
  for (const node of candidates) {
    if (!isVisible(node)) continue;
    if (node.disabled) continue;
    const label = ((node.innerText || node.textContent || "") + " " + (node.getAttribute("aria-label") || "") + " " + (node.getAttribute("value") || "")).trim();
    if (!preferred.some((rx) => rx.test(label))) continue;
    try {
      node.click();
      return label || "clicked";
    } catch (_err) {
      continue;
    }
  }
  return "";
})()`
	var label string
	if err := chromedp.Run(ctx, chromedp.Evaluate(script, &label)); err != nil {
		return err
	}
	if strings.TrimSpace(label) == "" {
		return errors.New("submit button not found")
	}
	return nil
}

func (a *app) browserSubmitPost(ctx context.Context) error {
	script := `(function () {
  function isVisible(el) {
    if (!el) return false;
    const style = window.getComputedStyle(el);
    if (!style) return false;
    if (style.display === "none" || style.visibility === "hidden") return false;
    return el.offsetWidth > 0 || el.offsetHeight > 0 || el.getClientRects().length > 0;
  }
  const anchorField = document.querySelector('textarea[name="text"], textarea[name="title"], input[name="title"]');
  const form = anchorField ? anchorField.closest('form') : document.querySelector('form.usertext, form#newlink, form[name="newlink"]');
  if (form) {
    try {
      if (typeof form.requestSubmit === "function") {
        form.requestSubmit();
      } else {
        form.submit();
      }
      return "form_submit";
    } catch (_err) {}
  }
  const direct = Array.from(document.querySelectorAll('button[name="submit"], button[type="submit"], input[name="submit"], input[type="submit"]'));
  for (const node of direct) {
    if (!isVisible(node)) continue;
    if (node.disabled) continue;
    try {
      node.click();
      return (node.getAttribute("value") || node.getAttribute("name") || node.innerText || node.textContent || "submit").trim();
    } catch (_err) {
      continue;
    }
  }
  const candidates = Array.from(document.querySelectorAll('button, input[type="submit"], [role="button"]'));
  const preferred = [/submit/i, /post/i];
  for (const node of candidates) {
    if (!isVisible(node)) continue;
    if (node.disabled) continue;
    const label = ((node.innerText || node.textContent || "") + " " + (node.getAttribute("aria-label") || "") + " " + (node.getAttribute("value") || "")).trim();
    if (!preferred.some((rx) => rx.test(label))) continue;
    try {
      node.click();
      return label || "clicked";
    } catch (_err) {
      continue;
    }
  }
  return "";
})()`
	var label string
	if err := chromedp.Run(ctx, chromedp.Evaluate(script, &label)); err != nil {
		return err
	}
	if strings.TrimSpace(label) == "" {
		return errors.New("post submit button not found")
	}
	return nil
}

type verificationResult struct {
	Verified   bool
	Reason     string
	Username   string
	ThingID    string
	CommentURL string
}

type duplicateCheckResult struct {
	Found      bool
	Reason     string
	Username   string
	ThingID    string
	CommentURL string
}

type submissionVerificationResult struct {
	Verified      bool
	Reason        string
	Username      string
	ThingID       string
	SubmissionURL string
}

type submissionDuplicateCheckResult struct {
	Found         bool
	Reason        string
	Username      string
	ThingID       string
	SubmissionURL string
}

type oauthUserCommentListing struct {
	Data struct {
		Children []struct {
			Data struct {
				Name          string  `json:"name"`
				Subreddit     string  `json:"subreddit"`
				Score         int     `json:"score"`
				ParentID      string  `json:"parent_id"`
				LinkTitle     string  `json:"link_title"`
				Body          string  `json:"body"`
				CreatedUTC    float64 `json:"created_utc"`
				Permalink     string  `json:"permalink"`
				LinkPermalink string  `json:"link_permalink"`
			} `json:"data"`
		} `json:"children"`
	} `json:"data"`
}

type oauthUserPostListing struct {
	Data struct {
		Children []struct {
			Data struct {
				Name        string  `json:"name"`
				Subreddit   string  `json:"subreddit"`
				Title       string  `json:"title"`
				Selftext    string  `json:"selftext"`
				URL         string  `json:"url"`
				Score       int     `json:"score"`
				NumComments int     `json:"num_comments"`
				CreatedUTC  float64 `json:"created_utc"`
				Permalink   string  `json:"permalink"`
			} `json:"data"`
		} `json:"children"`
	} `json:"data"`
}

type oauthInboxListing struct {
	Data struct {
		Children []struct {
			Kind string `json:"kind"`
			Data struct {
				Name       string  `json:"name"`
				Author     string  `json:"author"`
				Subreddit  string  `json:"subreddit"`
				Subject    string  `json:"subject"`
				Body       string  `json:"body"`
				Context    string  `json:"context"`
				LinkTitle  string  `json:"link_title"`
				ParentID   string  `json:"parent_id"`
				CreatedUTC float64 `json:"created_utc"`
				New        bool    `json:"new"`
				WasComment bool    `json:"was_comment"`
			} `json:"data"`
		} `json:"children"`
	} `json:"data"`
}

type oauthSubredditListing struct {
	Data struct {
		Children []struct {
			Data struct {
				Name             string `json:"name"`
				DisplayName      string `json:"display_name"`
				Title            string `json:"title"`
				URL              string `json:"url"`
				Subscribers      int    `json:"subscribers"`
				UserIsSubscriber bool   `json:"user_is_subscriber"`
			} `json:"data"`
		} `json:"children"`
	} `json:"data"`
}

type oauthUserCommentsResponse struct {
	Data struct {
		Children []struct {
			Data struct {
				Author        string `json:"author"`
				Body          string `json:"body"`
				Name          string `json:"name"`
				Permalink     string `json:"permalink"`
				LinkPermalink string `json:"link_permalink"`
			} `json:"data"`
		} `json:"children"`
	} `json:"data"`
}

func (a *app) findExistingOwnComment(identity browserIdentity, targetPermalink string) (duplicateCheckResult, error) {
	if !identity.LoggedIn || identity.Username == "" {
		return duplicateCheckResult{
			Found:    false,
			Reason:   "identity_unavailable",
			Username: identity.Username,
		}, nil
	}

	listing, status, err := a.publicUserComments(identity.Username)
	if err != nil {
		return duplicateCheckResult{}, err
	}
	if status != http.StatusOK {
		return duplicateCheckResult{}, fmt.Errorf("public comments returned status %d", status)
	}

	expectedThread := normalizePermalinkForCompareGo(targetPermalink)
	expectedPostID := permalinkPostID(expectedThread)
	for _, row := range listing.Data.Children {
		author := strings.TrimSpace(row.Data.Author)
		if !strings.EqualFold(author, identity.Username) {
			continue
		}
		commentThread := normalizePermalinkForCompareGo(firstNonEmpty(row.Data.LinkPermalink, row.Data.Permalink))
		if !sameThreadPermalink(commentThread, expectedThread, permalinkPostID(commentThread), expectedPostID) {
			continue
		}
		return duplicateCheckResult{
			Found:      true,
			Reason:     "existing_comment_found",
			Username:   identity.Username,
			ThingID:    strings.TrimSpace(row.Data.Name),
			CommentURL: absoluteRedditURL(row.Data.Permalink),
		}, nil
	}

	return duplicateCheckResult{
		Found:    false,
		Reason:   "no_existing_comment",
		Username: identity.Username,
	}, nil
}

func (a *app) verifyPostedComment(identity browserIdentity, targetPermalink string, commentText string, timeoutSec int) (verificationResult, error) {
	if !identity.LoggedIn || identity.Username == "" {
		return verificationResult{
			Verified: false,
			Reason:   "identity_unavailable",
			Username: identity.Username,
		}, nil
	}
	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	expectedThread := normalizePermalinkForCompareGo(targetPermalink)
	expectedPostID := permalinkPostID(expectedThread)
	expectedBody := strings.TrimSpace(commentText)
	expectedPrefix := expectedBody
	if len(expectedPrefix) > 80 {
		expectedPrefix = expectedPrefix[:80]
	}

	for time.Now().Before(deadline) {
		listing, status, err := a.publicUserComments(identity.Username)
		if err == nil && status == http.StatusOK {
			for _, row := range listing.Data.Children {
				body := strings.TrimSpace(row.Data.Body)
				author := strings.TrimSpace(row.Data.Author)
				commentThread := normalizePermalinkForCompareGo(firstNonEmpty(row.Data.LinkPermalink, row.Data.Permalink))
				sameThread := sameThreadPermalink(commentThread, expectedThread, permalinkPostID(commentThread), expectedPostID)
				if !sameThread {
					continue
				}
				if !strings.EqualFold(author, identity.Username) {
					continue
				}
				if body == expectedBody || (expectedPrefix != "" && strings.Contains(body, expectedPrefix)) {
					return verificationResult{
						Verified:   true,
						Reason:     "matched_recent_comment",
						Username:   identity.Username,
						ThingID:    strings.TrimSpace(row.Data.Name),
						CommentURL: absoluteRedditURL(row.Data.Permalink),
					}, nil
				}
			}
		}
		time.Sleep(2 * time.Second)
	}

	return verificationResult{
		Verified: false,
		Reason:   "verify_timeout",
		Username: identity.Username,
	}, nil
}

func (a *app) findExistingOwnSubmission(identity browserIdentity, subreddit, title string) (submissionDuplicateCheckResult, error) {
	if !identity.LoggedIn || identity.Username == "" {
		return submissionDuplicateCheckResult{
			Found:    false,
			Reason:   "identity_unavailable",
			Username: identity.Username,
		}, nil
	}

	listing, status, err := a.publicUserPosts(identity.Username)
	if err != nil {
		return submissionDuplicateCheckResult{}, err
	}
	if status != http.StatusOK {
		return submissionDuplicateCheckResult{}, fmt.Errorf("public submissions returned status %d", status)
	}

	expectedSubreddit := strings.TrimSpace(strings.ToLower(subreddit))
	expectedTitle := strings.TrimSpace(title)
	for _, row := range listing.Data.Children {
		data := row.Data
		if !strings.EqualFold(strings.TrimSpace(data.Subreddit), expectedSubreddit) {
			continue
		}
		if strings.TrimSpace(data.Title) != expectedTitle {
			continue
		}
		return submissionDuplicateCheckResult{
			Found:         true,
			Reason:        "existing_submission_found",
			Username:      identity.Username,
			ThingID:       strings.TrimSpace(data.Name),
			SubmissionURL: absoluteRedditURL(data.Permalink),
		}, nil
	}

	return submissionDuplicateCheckResult{
		Found:    false,
		Reason:   "no_existing_submission",
		Username: identity.Username,
	}, nil
}

func (a *app) verifyPostedSubmission(identity browserIdentity, subreddit, title, body string, timeoutSec int) (submissionVerificationResult, error) {
	if !identity.LoggedIn || identity.Username == "" {
		return submissionVerificationResult{
			Verified: false,
			Reason:   "identity_unavailable",
			Username: identity.Username,
		}, nil
	}

	expectedSubreddit := strings.TrimSpace(strings.ToLower(subreddit))
	expectedTitle := strings.TrimSpace(title)
	expectedBody := strings.TrimSpace(body)
	expectedPrefix := expectedBody
	if len(expectedPrefix) > 80 {
		expectedPrefix = expectedPrefix[:80]
	}
	deadline := time.Now().Add(time.Duration(timeoutSec) * time.Second)
	for time.Now().Before(deadline) {
		listing, status, err := a.publicUserPosts(identity.Username)
		if err == nil && status == http.StatusOK {
			for _, row := range listing.Data.Children {
				data := row.Data
				if !strings.EqualFold(strings.TrimSpace(data.Subreddit), expectedSubreddit) {
					continue
				}
				if strings.TrimSpace(data.Title) != expectedTitle {
					continue
				}
				selftext := strings.TrimSpace(data.Selftext)
				if expectedBody == "" || selftext == expectedBody || (expectedPrefix != "" && strings.Contains(selftext, expectedPrefix)) {
					return submissionVerificationResult{
						Verified:      true,
						Reason:        "matched_recent_submission",
						Username:      identity.Username,
						ThingID:       strings.TrimSpace(data.Name),
						SubmissionURL: absoluteRedditURL(data.Permalink),
					}, nil
				}
			}
		}
		time.Sleep(2 * time.Second)
	}

	return submissionVerificationResult{
		Verified: false,
		Reason:   "verify_timeout",
		Username: identity.Username,
	}, nil
}

func (a *app) publicUserComments(username string) (oauthUserCommentsResponse, int, error) {
	path := fmt.Sprintf("%s/user/%s/comments/.json?limit=25&sort=new&raw_json=1", redditBaseURL, neturl.PathEscape(username))
	req, err := http.NewRequest(http.MethodGet, path, nil)
	if err != nil {
		return oauthUserCommentsResponse{}, 0, err
	}
	req.Header.Set("User-Agent", a.userAgent)

	resp, err := a.client.Do(req)
	if err != nil {
		return oauthUserCommentsResponse{}, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return oauthUserCommentsResponse{}, resp.StatusCode, err
	}
	if resp.StatusCode != http.StatusOK {
		return oauthUserCommentsResponse{}, resp.StatusCode, fmt.Errorf("public comments returned status %d: %s", resp.StatusCode, clip(string(body), 240))
	}
	var payload oauthUserCommentsResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return oauthUserCommentsResponse{}, resp.StatusCode, err
	}
	return payload, resp.StatusCode, nil
}

func (a *app) publicUserPosts(username string) (oauthUserPostListing, int, error) {
	path := fmt.Sprintf("%s/user/%s/submitted/.json?limit=25&sort=new&raw_json=1", redditBaseURL, neturl.PathEscape(username))
	req, err := http.NewRequest(http.MethodGet, path, nil)
	if err != nil {
		return oauthUserPostListing{}, 0, err
	}
	req.Header.Set("User-Agent", a.userAgent)

	resp, err := a.client.Do(req)
	if err != nil {
		return oauthUserPostListing{}, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return oauthUserPostListing{}, resp.StatusCode, err
	}
	if resp.StatusCode != http.StatusOK {
		return oauthUserPostListing{}, resp.StatusCode, fmt.Errorf("public submissions returned status %d: %s", resp.StatusCode, clip(string(body), 240))
	}
	var payload oauthUserPostListing
	if err := json.Unmarshal(body, &payload); err != nil {
		return oauthUserPostListing{}, resp.StatusCode, err
	}
	return payload, resp.StatusCode, nil
}

func parseBoolLike(value string) bool {
	switch strings.TrimSpace(strings.ToLower(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func parsePositiveInt(value string, fallback int) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func clampListLimit(value int) int {
	if value <= 0 {
		return 10
	}
	if value > 100 {
		return 100
	}
	return value
}

func absoluteRedditURL(value string) string {
	text := strings.TrimSpace(value)
	if text == "" {
		return redditBaseURL + "/"
	}
	if strings.HasPrefix(text, "http://") || strings.HasPrefix(text, "https://") {
		return text
	}
	if !strings.HasPrefix(text, "/") {
		return redditBaseURL + "/" + text
	}
	return redditBaseURL + text
}

func absoluteOldRedditURL(value string) string {
	text := strings.TrimSpace(value)
	if text == "" {
		return oldRedditBaseURL + "/"
	}
	if strings.HasPrefix(text, "http://") || strings.HasPrefix(text, "https://") {
		parsed, err := neturl.Parse(text)
		if err == nil {
			return oldRedditBaseURL + parsed.Path
		}
		return text
	}
	if !strings.HasPrefix(text, "/") {
		return oldRedditBaseURL + "/" + text
	}
	return oldRedditBaseURL + text
}

func normalizePermalinkForCompareGo(value string) string {
	text := strings.TrimSpace(value)
	if text == "" {
		return ""
	}
	if strings.HasPrefix(text, "http://") || strings.HasPrefix(text, "https://") {
		parsed, err := neturl.Parse(text)
		if err == nil {
			text = parsed.Path
		}
	}
	if !strings.HasPrefix(text, "/") {
		text = "/" + text
	}
	return strings.TrimRight(text, "/")
}

func permalinkPostID(value string) string {
	normalized := normalizePermalinkForCompareGo(value)
	if normalized == "" {
		return ""
	}
	parts := strings.Split(strings.Trim(normalized, "/"), "/")
	for idx := 0; idx < len(parts)-1; idx++ {
		if parts[idx] == "comments" {
			return strings.TrimSpace(parts[idx+1])
		}
	}
	return ""
}

func sameThreadPermalink(actual, expected, actualPostID, expectedPostID string) bool {
	if actualPostID != "" && expectedPostID != "" {
		return actualPostID == expectedPostID
	}
	if actual == expected {
		return true
	}
	if expected != "" && strings.HasPrefix(actual, expected) {
		return true
	}
	if actual != "" && strings.HasPrefix(expected, actual) {
		return true
	}
	return false
}

func nullIfEmpty(value string) interface{} {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
