package threadpilot

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	cliName          = "threadpilot"
	defaultUserAgent = "marketing-cli-threadpilot/0.1"
	redditBaseURL    = "https://www.reddit.com"
	oldRedditBaseURL = "https://old.reddit.com"
	oauthBaseURL     = "https://oauth.reddit.com"
)

type app struct {
	client            *http.Client
	userAgent         string
	accessToken       string
	proxy             string
	profileDir        string
	browserWSEndpoint string
	browserDebugURL   string
	headless          bool
	loginWait         int
	holdOnError       int
}

type listingResponse struct {
	Data struct {
		Children []struct {
			Kind string   `json:"kind"`
			Data postData `json:"data"`
		} `json:"children"`
	} `json:"data"`
}

type postData struct {
	Name        string  `json:"name"`
	Title       string  `json:"title"`
	Subreddit   string  `json:"subreddit"`
	Author      string  `json:"author"`
	Score       int     `json:"score"`
	NumComments int     `json:"num_comments"`
	Permalink   string  `json:"permalink"`
	URL         string  `json:"url"`
	CreatedUTC  float64 `json:"created_utc"`
}

type threadListing struct {
	Data struct {
		Children []struct {
			Kind string          `json:"kind"`
			Data json.RawMessage `json:"data"`
		} `json:"children"`
	} `json:"data"`
}

type commentData struct {
	Name      string  `json:"name"`
	Author    string  `json:"author"`
	Body      string  `json:"body"`
	Score     int     `json:"score"`
	Permalink string  `json:"permalink"`
	Created   float64 `json:"created_utc"`
}

type subredditRule struct {
	ShortName       string `json:"short_name"`
	Description     string `json:"description"`
	Kind            string `json:"kind"`
	ViolationReason string `json:"violation_reason"`
}

type subredditRulesPayload struct {
	Rules         []subredditRule          `json:"rules"`
	SiteRules     []string                 `json:"site_rules"`
	SiteRulesFlow []map[string]interface{} `json:"site_rules_flow"`
}

type redditAPIResponse struct {
	Message string `json:"message"`
	Error   int    `json:"error"`
	JSON    struct {
		Errors [][]interface{}        `json:"errors"`
		Data   map[string]interface{} `json:"data"`
	} `json:"json"`
}

// Run executes the ThreadPilot CLI command with the provided arguments.
func Run(args []string) error {
	root := flag.NewFlagSet(cliName, flag.ContinueOnError)
	proxy := root.String("proxy", "", "HTTP/HTTPS proxy URL")
	userAgent := root.String("user-agent", defaultUserAgent, "User-Agent header")
	token := root.String("token", strings.TrimSpace(os.Getenv("REDDIT_ACCESS_TOKEN")), "Reddit OAuth access token (defaults to REDDIT_ACCESS_TOKEN)")
	browserProfile := root.String("browser-profile", profileDefault(), "Persistent browser profile directory")
	browserWSEndpoint := root.String(
		"browser-ws-endpoint",
		firstNonEmpty(
			strings.TrimSpace(os.Getenv("THREADPILOT_BROWSER_WS_URL")),
			strings.TrimSpace(os.Getenv("REDDIT_BROWSER_WS_URL")),
			strings.TrimSpace(os.Getenv("GOLOGIN_WS_URL")),
			strings.TrimSpace(os.Getenv("GOLOGIN_WS_ENDPOINT")),
		),
		"Attach to an existing Chromium-compatible browser via CDP WebSocket URL (supports GoLogin-style endpoints)",
	)
	browserDebugURL := root.String(
		"browser-debug-url",
		firstNonEmpty(
			strings.TrimSpace(os.Getenv("THREADPILOT_BROWSER_DEBUG_URL")),
			strings.TrimSpace(os.Getenv("REDDIT_BROWSER_DEBUG_URL")),
			strings.TrimSpace(os.Getenv("GOLOGIN_DEBUG_URL")),
		),
		"Attach to an existing browser via DevTools HTTP endpoint (base URL or /json/version URL)",
	)
	browserHeadless := root.Bool("browser-headless", envBool("REDDIT_HEADLESS"), "Run browser automation in headless mode")
	loginTimeout := root.Int("login-timeout", envInt("REDDIT_LOGIN_TIMEOUT_SEC", 180), "Browser login timeout in seconds")
	holdOnError := root.Int("hold-on-error", envInt("REDDIT_HOLD_ON_ERROR_SEC", 0), "Keep browser open on errors for N seconds")

	if err := root.Parse(args); err != nil {
		return err
	}

	rest := root.Args()
	if len(rest) == 0 {
		printRootUsage()
		return flag.ErrHelp
	}

	client, err := newHTTPClient(*proxy)
	if err != nil {
		return err
	}

	a := &app{
		client:            client,
		userAgent:         strings.TrimSpace(*userAgent),
		accessToken:       strings.TrimSpace(*token),
		proxy:             strings.TrimSpace(*proxy),
		profileDir:        strings.TrimSpace(*browserProfile),
		browserWSEndpoint: strings.TrimSpace(*browserWSEndpoint),
		browserDebugURL:   strings.TrimSpace(*browserDebugURL),
		headless:          *browserHeadless,
		loginWait:         *loginTimeout,
		holdOnError:       *holdOnError,
	}
	if a.userAgent == "" {
		a.userAgent = defaultUserAgent
	}

	command := rest[0]
	commandArgs := rest[1:]
	switch command {
	case "login":
		return a.runLogin(commandArgs)
	case "whoami":
		return a.runWhoami(commandArgs)
	case "my-comments":
		return a.runMyComments(commandArgs)
	case "my-replies":
		return a.runMyReplies(commandArgs)
	case "my-posts":
		return a.runMyPosts(commandArgs)
	case "my-subreddits":
		return a.runMySubreddits(commandArgs)
	case "subscribe":
		return a.runSubscribe(commandArgs)
	case "read":
		return a.runRead(commandArgs)
	case "search":
		return a.runSearch(commandArgs)
	case "rules":
		return a.runRules(commandArgs)
	case "like":
		return a.runLike(commandArgs)
	case "post":
		return a.runPost(commandArgs)
	default:
		printRootUsage()
		return fmt.Errorf("unknown command: %s", command)
	}
}

func newHTTPClient(proxyRaw string) (*http.Client, error) {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
	}
	proxyRaw = strings.TrimSpace(proxyRaw)
	if proxyRaw != "" {
		parsed, err := neturl.Parse(proxyRaw)
		if err != nil {
			return nil, fmt.Errorf("invalid --proxy value: %w", err)
		}
		transport.Proxy = http.ProxyURL(parsed)
	}
	return &http.Client{
		Transport: transport,
		Timeout:   45 * time.Second,
	}, nil
}

func envBool(name string) bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv(name)))
	return value == "1" || value == "true" || value == "yes"
}

func envInt(name string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}

func profileDefault() string {
	raw := strings.TrimSpace(os.Getenv("THREADPILOT_BROWSER_PROFILE"))
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("REDDIT_BROWSER_PROFILE"))
	}
	if raw != "" {
		return raw
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return ".threadpilot-profile"
	}
	return home + "/.threadpilot-profile"
}

func (a *app) runRead(args []string) error {
	fs := flag.NewFlagSet("read", flag.ContinueOnError)
	subreddit := fs.String("subreddit", "", "Subreddit name (example: SaaS)")
	sort := fs.String("sort", "hot", "Post sort: hot|new|top")
	limit := fs.Int("limit", 10, "Number of posts/comments to show")
	permalink := fs.String("permalink", "", "Thread permalink to read comments")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if *limit <= 0 {
		return fmt.Errorf("--limit must be > 0")
	}
	if *permalink != "" {
		return a.readThread(*permalink, *limit)
	}
	if strings.TrimSpace(*subreddit) == "" {
		fs.Usage()
		return errors.New("--subreddit is required when --permalink is not set")
	}
	if !isAllowed(*sort, []string{"hot", "new", "top"}) {
		return fmt.Errorf("--sort must be one of: hot, new, top")
	}

	endpoint := fmt.Sprintf(
		"%s/r/%s/%s.json?raw_json=1&limit=%d",
		redditBaseURL,
		neturl.PathEscape(strings.TrimSpace(*subreddit)),
		*sort,
		*limit,
	)
	var payload listingResponse
	if err := a.getJSON(endpoint, &payload, false); err != nil {
		return err
	}
	if len(payload.Data.Children) == 0 {
		fmt.Println("No posts found.")
		return nil
	}

	for i, child := range payload.Data.Children {
		post := child.Data
		fmt.Printf(
			"%d. [%s] %s\n   author=%s score=%d comments=%d created=%s\n   link=%s%s\n",
			i+1,
			post.Subreddit,
			clip(post.Title, 140),
			post.Author,
			post.Score,
			post.NumComments,
			unixToRFC3339(post.CreatedUTC),
			redditBaseURL,
			post.Permalink,
		)
	}
	return nil
}

func (a *app) runSearch(args []string) error {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	query := fs.String("query", "", "Search query text")
	subreddit := fs.String("subreddit", "", "Optional subreddit scope")
	sort := fs.String("sort", "relevance", "Sort: relevance|new|top|comments")
	timeWindow := fs.String("time", "all", "Time range: hour|day|week|month|year|all")
	limit := fs.Int("limit", 10, "Number of search results")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*query) == "" {
		fs.Usage()
		return errors.New("--query is required")
	}
	if *limit <= 0 {
		return fmt.Errorf("--limit must be > 0")
	}
	if !isAllowed(*sort, []string{"relevance", "new", "top", "comments"}) {
		return fmt.Errorf("--sort must be one of: relevance, new, top, comments")
	}
	if !isAllowed(*timeWindow, []string{"hour", "day", "week", "month", "year", "all"}) {
		return fmt.Errorf("--time must be one of: hour, day, week, month, year, all")
	}

	values := neturl.Values{}
	values.Set("raw_json", "1")
	values.Set("q", *query)
	values.Set("sort", *sort)
	values.Set("t", *timeWindow)
	values.Set("limit", strconv.Itoa(*limit))

	var endpoint string
	if strings.TrimSpace(*subreddit) == "" {
		endpoint = fmt.Sprintf("%s/search.json?%s", redditBaseURL, values.Encode())
	} else {
		values.Set("restrict_sr", "1")
		endpoint = fmt.Sprintf(
			"%s/r/%s/search.json?%s",
			redditBaseURL,
			neturl.PathEscape(strings.TrimSpace(*subreddit)),
			values.Encode(),
		)
	}

	var payload listingResponse
	if err := a.getJSON(endpoint, &payload, false); err != nil {
		return err
	}
	if len(payload.Data.Children) == 0 {
		fmt.Println("No search results found.")
		return nil
	}

	for i, child := range payload.Data.Children {
		post := child.Data
		fmt.Printf(
			"%d. [%s] %s\n   author=%s score=%d comments=%d\n   link=%s%s\n",
			i+1,
			post.Subreddit,
			clip(post.Title, 140),
			post.Author,
			post.Score,
			post.NumComments,
			redditBaseURL,
			post.Permalink,
		)
	}
	return nil
}

func (a *app) runRules(args []string) error {
	fs := flag.NewFlagSet("rules", flag.ContinueOnError)
	subreddit := fs.String("subreddit", "", "Subreddit name")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*subreddit) == "" {
		return errors.New("--subreddit is required")
	}

	payload, err := a.fetchSubredditRules(*subreddit)
	if err != nil {
		return err
	}
	return printJSON(map[string]interface{}{
		"subreddit":        normalizeSubredditName(*subreddit),
		"count":            len(payload.Rules),
		"site_rules_count": len(payload.SiteRules),
		"rules":            payload.Rules,
		"site_rules":       payload.SiteRules,
		"site_rules_flow":  payload.SiteRulesFlow,
	})
}

func (a *app) fetchSubredditRules(subreddit string) (subredditRulesPayload, error) {
	subreddit = normalizeSubredditName(subreddit)
	if subreddit == "" {
		return subredditRulesPayload{}, errors.New("subreddit is required")
	}

	endpoint := fmt.Sprintf("%s/r/%s/about/rules.json?raw_json=1", redditBaseURL, neturl.PathEscape(subreddit))
	var payload subredditRulesPayload
	if err := a.getJSON(endpoint, &payload, false); err != nil {
		return subredditRulesPayload{}, err
	}
	return payload, nil
}

func (a *app) runLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	preflight := fs.Bool("preflight", true, "Run preflight URL navigation check after login")
	username := fs.String("username", strings.TrimSpace(os.Getenv("REDDIT_LOGIN_USERNAME")), "Reddit username/email for automatic login (optional)")
	password := fs.String("password", os.Getenv("REDDIT_LOGIN_PASSWORD"), "Reddit password for automatic login (optional)")
	passwordStdin := fs.Bool("password-stdin", false, "Read Reddit password from stdin for automatic login")
	newProfile := fs.Bool("new-profile", envBool("REDDIT_LOGIN_NEW_PROFILE"), "Create and use a fresh browser profile for this login run")
	newProfileDir := fs.String("new-profile-dir", strings.TrimSpace(os.Getenv("REDDIT_LOGIN_NEW_PROFILE_DIR")), "Parent directory for --new-profile (default: system temp dir)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *passwordStdin {
		secret, err := io.ReadAll(io.LimitReader(os.Stdin, 4096))
		if err != nil {
			return fmt.Errorf("unable to read password from stdin: %w", err)
		}
		*password = strings.TrimSpace(string(secret))
	}
	if *newProfile {
		baseDir := strings.TrimSpace(*newProfileDir)
		if baseDir != "" {
			if err := os.MkdirAll(baseDir, 0o700); err != nil {
				return fmt.Errorf("unable to create --new-profile-dir: %w", err)
			}
		}
		created, err := os.MkdirTemp(baseDir, "threadpilot-profile-")
		if err != nil {
			return fmt.Errorf("unable to create fresh browser profile: %w", err)
		}
		a.profileDir = created
	}

	extraEnv := map[string]string{
		"REDDIT_LOGIN_USERNAME": strings.TrimSpace(*username),
		"REDDIT_LOGIN_PASSWORD": *password,
	}

	loginPayload, err := a.runBrowserMode("login_phase", extraEnv)
	if err != nil {
		return err
	}
	loginPayload["browser_profile"] = a.profileDir
	loginPayload["new_profile"] = *newProfile
	if err := printJSON(loginPayload); err != nil {
		return err
	}

	if !*preflight {
		return nil
	}

	preflightPayload, err := a.runBrowserMode("preflight", nil)
	if err != nil {
		return err
	}
	preflightPayload["browser_profile"] = a.profileDir
	preflightPayload["new_profile"] = *newProfile
	return printJSON(preflightPayload)
}

func (a *app) runWhoami(args []string) error {
	fs := flag.NewFlagSet("whoami", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	payload, err := a.runBrowserMode("whoami", nil)
	if err != nil {
		return err
	}
	return printJSON(payload)
}

func (a *app) runMyComments(args []string) error {
	fs := flag.NewFlagSet("my-comments", flag.ContinueOnError)
	limit := fs.Int("limit", 10, "Number of comments to return")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *limit <= 0 {
		return errors.New("--limit must be > 0")
	}

	payload, err := a.runBrowserMode("my_comments", map[string]string{
		"REDDIT_LIST_LIMIT": strconv.Itoa(*limit),
	})
	if err != nil {
		return err
	}
	return printJSON(payload)
}

func (a *app) runMyReplies(args []string) error {
	fs := flag.NewFlagSet("my-replies", flag.ContinueOnError)
	limit := fs.Int("limit", 10, "Number of replies to return")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *limit <= 0 {
		return errors.New("--limit must be > 0")
	}

	payload, err := a.runBrowserMode("my_replies", map[string]string{
		"REDDIT_LIST_LIMIT": strconv.Itoa(*limit),
	})
	if err != nil {
		return err
	}
	return printJSON(payload)
}

func (a *app) runMyPosts(args []string) error {
	fs := flag.NewFlagSet("my-posts", flag.ContinueOnError)
	limit := fs.Int("limit", 10, "Number of submissions to return")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *limit <= 0 {
		return errors.New("--limit must be > 0")
	}

	payload, err := a.runBrowserMode("my_posts", map[string]string{
		"REDDIT_LIST_LIMIT": strconv.Itoa(*limit),
	})
	if err != nil {
		return err
	}
	return printJSON(payload)
}

func (a *app) runMySubreddits(args []string) error {
	fs := flag.NewFlagSet("my-subreddits", flag.ContinueOnError)
	limit := fs.Int("limit", 25, "Number of subscribed subreddits to return")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *limit <= 0 {
		return errors.New("--limit must be > 0")
	}

	payload, err := a.runBrowserMode("my_subreddits", map[string]string{
		"REDDIT_LIST_LIMIT": strconv.Itoa(*limit),
	})
	if err != nil {
		return err
	}
	return printJSON(payload)
}

func (a *app) runSubscribe(args []string) error {
	fs := flag.NewFlagSet("subscribe", flag.ContinueOnError)
	subreddit := fs.String("subreddit", "", "Subreddit name to subscribe to (example: SaaS)")
	dryRun := fs.Bool("dry-run", false, "Preview subscription request without sending")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*subreddit) == "" {
		return errors.New("--subreddit is required")
	}

	payload, err := a.runBrowserMode("subscribe", map[string]string{
		"REDDIT_SUBREDDIT": strings.TrimSpace(*subreddit),
		"DRY_RUN_BROWSER":  boolToIntString(*dryRun),
	})
	if err != nil {
		return err
	}
	return printJSON(payload)
}

func (a *app) runLike(args []string) error {
	fs := flag.NewFlagSet("like", flag.ContinueOnError)
	thingID := fs.String("thing", "", "Thing fullname to like (example: t3_abcd12)")
	permalink := fs.String("permalink", "", "Post/comment permalink to resolve like target")
	dryRun := fs.Bool("dry-run", false, "Preview like target without sending vote")
	confirmLike := fs.Bool("confirm-like", false, "Explicit confirmation required to send like")

	if err := fs.Parse(args); err != nil {
		return err
	}

	resolvedThing := strings.TrimSpace(*thingID)
	resolvedPermalink := strings.TrimSpace(*permalink)
	if resolvedThing == "" && resolvedPermalink == "" {
		return errors.New("either --thing or --permalink is required")
	}
	if !*dryRun && !*confirmLike {
		return errors.New("like requires explicit confirmation; rerun with --confirm-like (or use --dry-run)")
	}

	payload, err := a.runBrowserMode("like", map[string]string{
		"REDDIT_THING_ID":        resolvedThing,
		"REDDIT_PERMALINK":       resolvedPermalink,
		"DRY_RUN_BROWSER":        boolToIntString(*dryRun),
		"REDDIT_CONFIRM_LIKE":    boolToIntString(*confirmLike),
		"REDDIT_VOTE_DIRECTION":  "1",
		"REDDIT_ACTION_VERB":     "like",
		"REDDIT_ACTION_PRECHECK": "1",
	})
	if err != nil {
		return err
	}
	return printJSON(payload)
}

func (a *app) runPost(args []string) error {
	fs := flag.NewFlagSet("post", flag.ContinueOnError)
	transport := fs.String("transport", "browser", "Transport: browser|api")
	kind := fs.String("kind", "comment", "Post type: comment|self|link")
	parent := fs.String("parent", "", "Parent thing fullname for comments (example: t3_abcd12 or t1_efgh34)")
	permalink := fs.String("permalink", "", "Thread/comment permalink for browser comment posting")
	subreddit := fs.String("subreddit", "", "Subreddit for self/link posts")
	title := fs.String("title", "", "Title for self/link posts")
	text := fs.String("text", "", "Comment or self-post text")
	flair := fs.String("flair", "", "Post flair text for browser self posts")
	linkURL := fs.String("url", "", "Target URL for link posts")
	nsfw := fs.Bool("nsfw", false, "Mark submission NSFW (self/link only)")
	dryRun := fs.Bool("dry-run", false, "Dry run for browser posting")
	requireVerified := fs.Bool("require-verified", true, "Fail if browser post cannot be verified in your recent comments")
	verifyTimeout := fs.Int("verify-timeout", envInt("REDDIT_POST_VERIFY_TIMEOUT_SEC", 30), "Verification timeout in seconds for browser post")
	confirmDoublePost := fs.Bool("confirm-double-post", false, "Explicitly allow posting again when this user already has a comment on the target thread")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if !isAllowed(*transport, []string{"browser", "api"}) {
		return fmt.Errorf("--transport must be one of: browser, api")
	}
	if !isAllowed(*kind, []string{"comment", "self", "link"}) {
		return fmt.Errorf("--kind must be one of: comment, self, link")
	}
	if *transport == "api" && *dryRun {
		return errors.New("--dry-run is supported only for --transport browser")
	}

	if *transport == "browser" {
		env := map[string]string{
			"REDDIT_POST_KIND":               strings.TrimSpace(*kind),
			"REDDIT_THING_ID":                strings.TrimSpace(*parent),
			"REDDIT_TEXT":                    strings.TrimSpace(*text),
			"REDDIT_PERMALINK":               strings.TrimSpace(*permalink),
			"REDDIT_SUBREDDIT":               strings.TrimSpace(*subreddit),
			"REDDIT_TITLE":                   strings.TrimSpace(*title),
			"REDDIT_URL":                     strings.TrimSpace(*linkURL),
			"REDDIT_POST_FLAIR":              strings.TrimSpace(*flair),
			"REDDIT_NSFW":                    boolToIntString(*nsfw),
			"DRY_RUN_BROWSER":                boolToIntString(*dryRun),
			"REDDIT_CONFIRM_DOUBLE_POST":     boolToIntString(*confirmDoublePost),
			"REDDIT_POST_VERIFY_TIMEOUT_SEC": strconv.Itoa(*verifyTimeout),
		}

		switch *kind {
		case "comment":
			if strings.TrimSpace(*parent) == "" {
				return errors.New("--parent is required for browser comment posting")
			}
			if strings.TrimSpace(*permalink) == "" {
				return errors.New("--permalink is required for browser comment posting")
			}
			if strings.TrimSpace(*text) == "" {
				return errors.New("--text is required for browser comment posting")
			}
		case "self":
			if strings.TrimSpace(*subreddit) == "" {
				return errors.New("--subreddit is required for browser self-posting")
			}
			if strings.TrimSpace(*title) == "" {
				return errors.New("--title is required for browser self-posting")
			}
			if strings.TrimSpace(*text) == "" {
				return errors.New("--text is required for browser self-posting")
			}
		default:
			return errors.New("browser transport currently supports --kind comment and --kind self only")
		}

		payload, err := a.runBrowserMode("publish", env)
		if err != nil {
			return err
		}
		if err := printJSON(payload); err != nil {
			return err
		}
		if *dryRun || !*requireVerified {
			return nil
		}
		status, _ := payload["status"].(string)
		if status != "posted_verified" {
			username, _ := payload["username"].(string)
			return fmt.Errorf("browser post was not verified for user %q; rerun with --require-verified=false to allow unverified submits", username)
		}
		return nil
	}

	if strings.TrimSpace(a.accessToken) == "" {
		return errors.New("missing OAuth token: set REDDIT_ACCESS_TOKEN or pass --token")
	}

	switch *kind {
	case "comment":
		if strings.TrimSpace(*parent) == "" {
			return errors.New("--parent is required for --kind comment")
		}
		if strings.TrimSpace(*text) == "" {
			return errors.New("--text is required for --kind comment")
		}
		return a.postComment(*parent, *text)
	case "self":
		if strings.TrimSpace(*subreddit) == "" || strings.TrimSpace(*title) == "" || strings.TrimSpace(*text) == "" {
			return errors.New("--subreddit, --title, and --text are required for --kind self")
		}
		return a.submitPost(*subreddit, *title, "self", *text, "", *nsfw)
	case "link":
		if strings.TrimSpace(*subreddit) == "" || strings.TrimSpace(*title) == "" || strings.TrimSpace(*linkURL) == "" {
			return errors.New("--subreddit, --title, and --url are required for --kind link")
		}
		return a.submitPost(*subreddit, *title, "link", "", *linkURL, *nsfw)
	default:
		return fmt.Errorf("unsupported --kind value: %s", *kind)
	}
}

func (a *app) runBrowserMode(mode string, extraEnv map[string]string) (map[string]interface{}, error) {
	payload, err := a.runBrowserModeRemote(mode, extraEnv)
	if err != nil {
		return nil, fmt.Errorf("browser mode %q failed: %w", mode, err)
	}
	return payload, nil
}

func (a *app) readThread(permalink string, limit int) error {
	endpoint := fmt.Sprintf("%s%s.json?raw_json=1&limit=%d", redditBaseURL, normalizePermalink(permalink), limit)

	var payload []threadListing
	if err := a.getJSON(endpoint, &payload, false); err != nil {
		return err
	}
	if len(payload) < 1 || len(payload[0].Data.Children) == 0 {
		return errors.New("thread response did not include post data")
	}

	var post postData
	if err := json.Unmarshal(payload[0].Data.Children[0].Data, &post); err != nil {
		return fmt.Errorf("unable to decode thread metadata: %w", err)
	}
	fmt.Printf(
		"[%s] %s\nauthor=%s score=%d comments=%d\nlink=%s%s\n\n",
		post.Subreddit,
		post.Title,
		post.Author,
		post.Score,
		post.NumComments,
		redditBaseURL,
		post.Permalink,
	)

	if len(payload) < 2 {
		fmt.Println("No comments listing in response.")
		return nil
	}

	shown := 0
	for _, child := range payload[1].Data.Children {
		if child.Kind != "t1" {
			continue
		}
		var comment commentData
		if err := json.Unmarshal(child.Data, &comment); err != nil {
			continue
		}
		if strings.TrimSpace(comment.Body) == "" {
			continue
		}
		shown++
		fmt.Printf(
			"%d. %s (score=%d at %s)\n   %s\n   %s%s\n",
			shown,
			comment.Author,
			comment.Score,
			unixToRFC3339(comment.Created),
			clip(oneLine(comment.Body), 220),
			redditBaseURL,
			comment.Permalink,
		)
		if shown >= limit {
			break
		}
	}
	if shown == 0 {
		fmt.Println("No readable comments found.")
	}
	return nil
}

func (a *app) postComment(parentThingID, text string) error {
	values := neturl.Values{}
	values.Set("api_type", "json")
	values.Set("thing_id", strings.TrimSpace(parentThingID))
	values.Set("text", strings.TrimSpace(text))

	var payload redditAPIResponse
	if err := a.postFormJSON(oauthBaseURL+"/api/comment", values, &payload, true); err != nil {
		return err
	}
	if err := ensureAPISuccess(payload); err != nil {
		return err
	}
	fmt.Println("Comment posted.")

	if things, ok := payload.JSON.Data["things"].([]interface{}); ok && len(things) > 0 {
		first, _ := things[0].(map[string]interface{})
		if data, ok := first["data"].(map[string]interface{}); ok {
			if name, ok := data["name"].(string); ok && name != "" {
				fmt.Printf("thing_id=%s\n", name)
			}
		}
	}
	return nil
}

func (a *app) submitPost(subreddit, title, kind, text, linkURL string, nsfw bool) error {
	thingID, urlValue, err := a.submitPostWithToken(a.accessToken, subreddit, title, kind, text, linkURL, nsfw, "", "")
	if err != nil {
		return err
	}
	fmt.Println("Submission posted.")
	if thingID != "" {
		fmt.Printf("thing_id=%s\n", thingID)
	}
	if urlValue != "" {
		fmt.Printf("url=%s\n", urlValue)
	}
	return nil
}

func (a *app) submitPostWithToken(token, subreddit, title, kind, text, linkURL string, nsfw bool, flairID, flairText string) (string, string, error) {
	values := neturl.Values{}
	values.Set("api_type", "json")
	values.Set("sr", strings.TrimSpace(subreddit))
	values.Set("title", strings.TrimSpace(title))
	values.Set("kind", kind)
	values.Set("resubmit", "true")
	values.Set("sendreplies", "true")
	if nsfw {
		values.Set("nsfw", "true")
	}
	if strings.TrimSpace(flairID) != "" {
		values.Set("flair_id", strings.TrimSpace(flairID))
	}
	if strings.TrimSpace(flairText) != "" {
		values.Set("flair_text", strings.TrimSpace(flairText))
	}

	if kind == "self" {
		values.Set("text", strings.TrimSpace(text))
	} else {
		values.Set("url", strings.TrimSpace(linkURL))
	}

	var payload redditAPIResponse
	trimmedToken := strings.TrimSpace(token)
	switch {
	case trimmedToken == "":
		return "", "", errors.New("missing oauth token")
	case trimmedToken == strings.TrimSpace(a.accessToken):
		if err := a.postFormJSON(oauthBaseURL+"/api/submit", values, &payload, true); err != nil {
			return "", "", err
		}
	default:
		if _, err := a.oauthPostFormWithToken(trimmedToken, oauthBaseURL+"/api/submit", values, &payload); err != nil {
			return "", "", err
		}
	}
	if err := ensureAPISuccess(payload); err != nil {
		return "", "", err
	}
	thingID := ""
	if name, ok := payload.JSON.Data["name"].(string); ok && name != "" {
		thingID = name
	}
	urlValue := ""
	if urlValue, ok := payload.JSON.Data["url"].(string); ok && urlValue != "" {
		return thingID, urlValue, nil
	}
	return thingID, urlValue, nil
}

func ensureAPISuccess(payload redditAPIResponse) error {
	if payload.Error != 0 {
		return fmt.Errorf("reddit API error %d: %s", payload.Error, payload.Message)
	}
	if len(payload.JSON.Errors) == 0 {
		return nil
	}
	parts := make([]string, 0, len(payload.JSON.Errors))
	for _, row := range payload.JSON.Errors {
		if len(row) == 0 {
			continue
		}
		chunks := make([]string, 0, len(row))
		for _, item := range row {
			chunks = append(chunks, fmt.Sprint(item))
		}
		parts = append(parts, strings.Join(chunks, " "))
	}
	return fmt.Errorf("reddit rejected request: %s", strings.Join(parts, "; "))
}

func (a *app) getJSON(endpoint string, out interface{}, auth bool) error {
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	return a.doJSON(req, out, auth)
}

func (a *app) postFormJSON(endpoint string, values neturl.Values, out interface{}, auth bool) error {
	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(values.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return a.doJSON(req, out, auth)
}

func (a *app) doJSON(req *http.Request, out interface{}, auth bool) error {
	req.Header.Set("User-Agent", a.userAgent)
	if auth {
		if strings.TrimSpace(a.accessToken) == "" {
			return errors.New("missing OAuth token")
		}
		req.Header.Set("Authorization", "bearer "+a.accessToken)
	}

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
		return fmt.Errorf("%s %s failed: HTTP %d: %s", req.Method, req.URL.Host, resp.StatusCode, clip(string(body), 300))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("unable to parse JSON response: %w", err)
	}
	return nil
}

func printJSON(payload map[string]interface{}) error {
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(encoded))
	return nil
}

func boolToIntString(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

func normalizePermalink(permalink string) string {
	p := strings.TrimSpace(permalink)
	if p == "" {
		return "/"
	}
	if strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://") {
		parsed, err := neturl.Parse(p)
		if err != nil {
			return p
		}
		if parsed.RawPath != "" {
			return parsed.RawPath
		}
		return parsed.Path
	}
	if !strings.HasPrefix(p, "/") {
		return "/" + p
	}
	return p
}

func normalizeSubredditName(value string) string {
	value = strings.TrimSpace(strings.TrimPrefix(value, "r/"))
	value = strings.TrimPrefix(value, "/r/")
	value = strings.Trim(value, "/")
	return value
}

func oneLine(text string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(text)), " ")
}

func clip(text string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	if len(text) <= maxLen {
		return text
	}
	if maxLen <= 3 {
		return text[:maxLen]
	}
	return text[:maxLen-3] + "..."
}

func unixToRFC3339(value float64) string {
	if value <= 0 {
		return "unknown"
	}
	sec := int64(value)
	return time.Unix(sec, 0).UTC().Format(time.RFC3339)
}

func isAllowed(value string, allowed []string) bool {
	value = strings.TrimSpace(strings.ToLower(value))
	for _, item := range allowed {
		if value == item {
			return true
		}
	}
	return false
}

func printRootUsage() {
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintln(os.Stderr, "  threadpilot [--proxy URL] [--browser-profile DIR] [--browser-ws-endpoint URL] [--browser-debug-url URL] [--browser-headless] [--user-agent UA] [--token TOKEN] <command> [flags]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Commands:")
	fmt.Fprintln(os.Stderr, "  login   Open browser session and wait for Reddit login")
	fmt.Fprintln(os.Stderr, "  whoami  Show logged-in Reddit username from browser session")
	fmt.Fprintln(os.Stderr, "  my-comments  List your recent comments (browser-auth)")
	fmt.Fprintln(os.Stderr, "  my-replies   List recent replies to your comments (browser-auth)")
	fmt.Fprintln(os.Stderr, "  my-posts     List your recent posts/submissions (browser-auth)")
	fmt.Fprintln(os.Stderr, "  my-subreddits  List your subscribed subreddits (browser-auth)")
	fmt.Fprintln(os.Stderr, "  subscribe      Subscribe to a subreddit (browser-auth)")
	fmt.Fprintln(os.Stderr, "  read    Read subreddit posts or a thread's top-level comments")
	fmt.Fprintln(os.Stderr, "  search  Search posts globally or in a subreddit")
	fmt.Fprintln(os.Stderr, "  rules   Show subreddit posting rules")
	fmt.Fprintln(os.Stderr, "  like    Like (upvote) a post/comment via browser session with explicit confirmation")
	fmt.Fprintln(os.Stderr, "  post    Publish comment via browser (default) or API (--transport api)")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "Examples:")
	fmt.Fprintln(os.Stderr, "  threadpilot login")
	fmt.Fprintln(os.Stderr, "  threadpilot --browser-ws-endpoint \"$GOLOGIN_WS_URL\" whoami")
	fmt.Fprintln(os.Stderr, "  threadpilot --browser-debug-url http://127.0.0.1:9222 whoami")
	fmt.Fprintln(os.Stderr, "  threadpilot login --new-profile --username myuser --password-stdin <<< \"$REDDIT_LOGIN_PASSWORD\"")
	fmt.Fprintln(os.Stderr, "  threadpilot login --username myuser --password-stdin <<< \"$REDDIT_LOGIN_PASSWORD\"")
	fmt.Fprintln(os.Stderr, "  threadpilot whoami")
	fmt.Fprintln(os.Stderr, "  threadpilot my-comments --limit 10")
	fmt.Fprintln(os.Stderr, "  threadpilot my-replies --limit 10")
	fmt.Fprintln(os.Stderr, "  threadpilot my-posts --limit 10")
	fmt.Fprintln(os.Stderr, "  threadpilot my-subreddits --limit 25")
	fmt.Fprintln(os.Stderr, "  threadpilot subscribe --subreddit SaaS --dry-run")
	fmt.Fprintln(os.Stderr, "  threadpilot like --permalink /r/SaaS/comments/xxxx/post_slug/ --dry-run")
	fmt.Fprintln(os.Stderr, "  threadpilot like --thing t3_abcd12 --confirm-like")
	fmt.Fprintln(os.Stderr, "  threadpilot rules --subreddit SaaS")
	fmt.Fprintln(os.Stderr, "  threadpilot post --kind comment --parent t1_abcd12 --permalink /r/SaaS/comments/xxxx/post_slug/ --text \"Nice take\"")
	fmt.Fprintln(os.Stderr, "  threadpilot --proxy http://127.0.0.1:8080 read --subreddit SaaS --sort hot --limit 5")
	fmt.Fprintln(os.Stderr, "  threadpilot search --query \"cold email\" --subreddit Entrepreneur --limit 10")
	fmt.Fprintln(os.Stderr, "  REDDIT_ACCESS_TOKEN=... threadpilot post --transport api --kind comment --parent t3_abcd12 --text \"Helpful point\"")
}
